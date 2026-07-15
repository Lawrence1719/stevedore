// Package orchestrator coordinates the full deploy pipeline for each app.
// Each app gets a dedicated worker goroutine; deploys for the same app are
// serialized. Different apps deploy in parallel.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/rence/stevedore/internal/build"
	"github.com/rence/stevedore/internal/logstream"
	"github.com/rence/stevedore/internal/runtime"
	"github.com/rence/stevedore/internal/store"

	"github.com/google/uuid"
)

// job is a single deploy request handed to an app's worker goroutine.
type job struct {
	gitSHA  string
	trigger string // "manual" | "webhook"
}

// worker owns the deploy queue for one app.
type worker struct {
	appName string
	ch      chan job    // buffered to 1 — second push while one runs is dropped
	mu      sync.Mutex // guards busy flag
	busy    bool
}

// Orchestrator manages per-app workers and the deploy pipeline.
type Orchestrator struct {
	store   *store.Store
	build   *build.Engine
	runtime *runtime.Manager
	hub     *logstream.Hub
	repoDir string // base directory where repos are checked out

	mu      sync.Mutex
	workers map[string]*worker // keyed by app name
}

// Config holds Orchestrator dependencies and settings.
type Config struct {
	Store   *store.Store
	Build   *build.Engine
	Runtime *runtime.Manager
	Hub     *logstream.Hub
	RepoDir string // e.g. "/opt/stevedore/repos"
}

// New creates a new Orchestrator.
func New(cfg Config) *Orchestrator {
	return &Orchestrator{
		store:   cfg.Store,
		build:   cfg.Build,
		runtime: cfg.Runtime,
		hub:     cfg.Hub,
		repoDir: cfg.RepoDir,
		workers: make(map[string]*worker),
	}
}

// workerFor returns (or creates) the per-app worker goroutine.
func (o *Orchestrator) workerFor(appName string) *worker {
	o.mu.Lock()
	defer o.mu.Unlock()
	w, ok := o.workers[appName]
	if !ok {
		w = &worker{
			appName: appName,
			ch:      make(chan job, 1),
		}
		o.workers[appName] = w
		go o.runWorker(w)
	}
	return w
}

// Enqueue submits a deploy job for appName. If a deploy is already running
// for that app, the new request is dropped (with a warning) to prevent
// overlapping deploys from corrupting rollback state.
func (o *Orchestrator) Enqueue(appName, gitSHA, trigger string) error {
	w := o.workerFor(appName)
	j := job{gitSHA: gitSHA, trigger: trigger}
	select {
	case w.ch <- j:
		slog.Info("orchestrator: deploy enqueued", "app", appName, "sha", gitSHA)
		return nil
	default:
		return fmt.Errorf("orchestrator: deploy already queued/running for %s; new request dropped", appName)
	}
}

// runWorker is the long-lived goroutine for a single app. It processes jobs
// one at a time, guaranteeing serialized deploys for that app.
func (o *Orchestrator) runWorker(w *worker) {
	for j := range w.ch {
		w.mu.Lock()
		w.busy = true
		w.mu.Unlock()

		if err := o.runDeploy(w.appName, j); err != nil {
			slog.Error("orchestrator: deploy failed", "app", w.appName, "err", err)
		}

		w.mu.Lock()
		w.busy = false
		w.mu.Unlock()
	}
}

// runDeploy executes the full deploy pipeline for one job:
// fetch → build → runtime → record.
func (o *Orchestrator) runDeploy(appName string, j job) error {
	ctx := context.Background()

	app, err := o.store.GetApp(appName)
	if err != nil {
		return fmt.Errorf("get app: %w", err)
	}

	deployID := uuid.New().String()

	// Create the deploy record.
	d := &store.Deploy{
		ID:      deployID,
		AppID:   app.ID,
		GitSHA:  j.gitSHA,
		ImageTag: "", // filled after build
		Status:  store.StatusRunning,
		Trigger: j.trigger,
	}
	if err := o.store.CreateDeploy(d); err != nil {
		return fmt.Errorf("create deploy record: %w", err)
	}

	// Create log streamer.
	streamer, err := o.hub.Create(appName, deployID)
	if err != nil {
		return fmt.Errorf("create log streamer: %w", err)
	}
	defer func() {
		streamer.Close()
		o.hub.Remove(deployID)
	}()

	fmt.Fprintf(streamer, "[stevedore] Deploy %s started (trigger: %s)\n", deployID, j.trigger)

	// Determine previous image tag for rollback safety.
	var previousImageTag string
	if app.CurrentDeployID != "" {
		prev, err := o.store.GetDeploy(app.CurrentDeployID)
		if err == nil && prev.Status == store.StatusSuccess {
			previousImageTag = prev.ImageTag
		}
	}

	// --- Step 1: Clone / update repo ---
	repoPath := filepath.Join(o.repoDir, appName)
	if err := build.CloneOrPull(ctx, app.RepoURL, app.Branch, repoPath, streamer); err != nil {
		return o.failDeploy(d, fmt.Sprintf("git: %v", err), streamer)
	}

	// --- Step 2: Build image ---
	imageTag, err := o.build.Build(ctx, appName, j.gitSHA, repoPath, streamer)
	if err != nil {
		return o.failDeploy(d, fmt.Sprintf("build: %v", err), streamer)
	}
	d.ImageTag = imageTag

	// --- Step 3: Start container + health check ---
	if err := o.runtime.Deploy(ctx, appName, imageTag, previousImageTag, app.EnvFile, app.HealthCheckURL, streamer); err != nil {
		return o.failDeploy(d, fmt.Sprintf("runtime: %v", err), streamer)
	}

	// --- Step 4: Record success ---
	now := time.Now()
	d.Status = store.StatusSuccess
	d.FinishedAt = &now
	if err := o.store.UpdateDeploy(d); err != nil {
		slog.Error("orchestrator: update deploy record", "err", err)
	}
	if err := o.store.SetCurrentDeploy(app.ID, deployID); err != nil {
		slog.Error("orchestrator: set current deploy", "err", err)
	}

	// Prune old images (keep last 5).
	o.build.PruneOldImages(ctx, appName, 5)

	fmt.Fprintf(streamer, "[stevedore] Deploy %s succeeded.\n", deployID)
	return nil
}

// failDeploy marks the deploy as failed, writes the error to the log, and returns it.
func (o *Orchestrator) failDeploy(d *store.Deploy, msg string, w *logstream.Streamer) error {
	fmt.Fprintf(w, "[stevedore][error] Deploy failed: %s\n", msg)
	now := time.Now()
	d.Status = store.StatusFailed
	d.FinishedAt = &now
	d.ErrorMsg = msg
	_ = o.store.UpdateDeploy(d)
	return errors.New(msg)
}

// Rollback reverts appName to its last successful deploy.
func (o *Orchestrator) Rollback(appName string) error {
	app, err := o.store.GetApp(appName)
	if err != nil {
		return fmt.Errorf("rollback: get app: %w", err)
	}

	target, err := o.store.GetLatestSuccessfulDeploy(app.ID, app.CurrentDeployID)
	if err != nil {
		return fmt.Errorf("rollback: no previous successful deploy for %s", appName)
	}

	deployID := uuid.New().String()
	d := &store.Deploy{
		ID:       deployID,
		AppID:    app.ID,
		GitSHA:   target.GitSHA,
		ImageTag: target.ImageTag,
		Status:   store.StatusRunning,
		Trigger:  "rollback",
	}
	if err := o.store.CreateDeploy(d); err != nil {
		return fmt.Errorf("rollback: create deploy: %w", err)
	}

	streamer, err := o.hub.Create(appName, deployID)
	if err != nil {
		return err
	}
	defer func() {
		streamer.Close()
		o.hub.Remove(deployID)
	}()

	fmt.Fprintf(streamer, "[stevedore] Rolling back %s to image %s (from deploy %s)\n",
		appName, target.ImageTag, target.ID)

	ctx := context.Background()
	if err := o.runtime.Deploy(ctx, appName, target.ImageTag, "", app.EnvFile, app.HealthCheckURL, streamer); err != nil {
		return o.failDeploy(d, fmt.Sprintf("rollback runtime: %v", err), streamer)
	}

	now := time.Now()
	d.Status = store.StatusSuccess
	d.FinishedAt = &now
	_ = o.store.UpdateDeploy(d)
	_ = o.store.SetCurrentDeploy(app.ID, deployID)

	// Mark the reverted deploy as rolled_back.
	if app.CurrentDeployID != "" {
		prev, _ := o.store.GetDeploy(app.CurrentDeployID)
		if prev != nil {
			prev.Status = store.StatusRolledBack
			_ = o.store.UpdateDeploy(prev)
		}
	}

	fmt.Fprintf(streamer, "[stevedore] Rollback to %s complete.\n", target.ImageTag)
	return nil
}

// StatusResponse holds the current state of an app for the API.
type StatusResponse struct {
	App           *store.App
	CurrentDeploy *store.Deploy
	Container     *runtime.ContainerInfo
	RecentDeploys []*store.Deploy
}

// Status returns the current state of an app.
func (o *Orchestrator) Status(appName string) (*StatusResponse, error) {
	app, err := o.store.GetApp(appName)
	if err != nil {
		return nil, err
	}

	resp := &StatusResponse{App: app}

	if app.CurrentDeployID != "" {
		resp.CurrentDeploy, _ = o.store.GetDeploy(app.CurrentDeployID)
	}

	resp.Container, _ = o.runtime.GetContainerInfo(context.Background(), appName)
	resp.RecentDeploys, _ = o.store.ListDeploys(app.ID, 10)

	return resp, nil
}
