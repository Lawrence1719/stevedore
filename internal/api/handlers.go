package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/rence/stevedore/internal/logstream"
	"github.com/rence/stevedore/internal/orchestrator"
	"github.com/rence/stevedore/internal/runtime"
	"github.com/rence/stevedore/internal/store"
	"github.com/rence/stevedore/internal/webhook"

	"github.com/google/uuid"
)

// ---- Interfaces (avoid circular imports) ----

// StoreInterface is the subset of *store.Store the handlers need.
type StoreInterface interface {
	CreateApp(a *store.App) error
	GetApp(name string) (*store.App, error)
	ListApps() ([]*store.App, error)
	WebhookSecret(appName string) (string, error)
	AppBranch(appName string) string
}

// OrchestratorInterface is the subset of *orchestrator.Orchestrator the handlers need.
type OrchestratorInterface interface {
	Enqueue(appName, gitSHA, trigger string) error
	Rollback(appName string) error
	Status(appName string) (*orchestrator.StatusResponse, error)
}

// LogHubInterface is the subset of *logstream.Hub the handlers need.
type LogHubInterface interface {
	Get(deployID string) *logstream.Streamer
	LogPath(appName, deployID string) string
}

// Handlers holds handler dependencies.
type Handlers struct {
	deps *Deps
	cfg  Config
}

// validAppName restricts app names to alphanumeric + hyphens only (no path traversal,
// no Docker image tag injection).
var validAppName = regexp.MustCompile(`^[a-z0-9][a-z0-9\-]{0,62}$`)

func appNameFromPath(r *http.Request, key string) string {
	// Go 1.22+ net/http supports {param} in patterns.
	return r.PathValue(key)
}

// writeJSON encodes v as JSON and writes it with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("api: encode response", "err", err)
	}
}

// writeError writes a generic error JSON body. Detailed errors go to server logs.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ---- Handlers ----

// GET /apps
func (h *Handlers) listApps(w http.ResponseWriter, r *http.Request) {
	apps, err := h.deps.Store.ListApps()
	if err != nil {
		slog.Error("api: list apps", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, apps)
}

// POST /apps  —  register a new app
func (h *Handlers) createApp(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name           string `json:"name"`
		RepoURL        string `json:"repo_url"`
		Branch         string `json:"branch"`
		WebhookSecret  string `json:"webhook_secret"`
		EnvFile        string `json:"env_file"`
		HealthCheckURL string `json:"health_check_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if !validAppName.MatchString(req.Name) {
		writeError(w, http.StatusBadRequest, "invalid app name (use lowercase alphanumeric + hyphens)")
		return
	}
	if req.RepoURL == "" {
		writeError(w, http.StatusBadRequest, "repo_url required")
		return
	}
	if req.Branch == "" {
		req.Branch = "main"
	}
	app := &store.App{
		ID:             uuid.New().String(),
		Name:           req.Name,
		RepoURL:        req.RepoURL,
		Branch:         req.Branch,
		WebhookSecret:  req.WebhookSecret,
		EnvFile:        req.EnvFile,
		HealthCheckURL: req.HealthCheckURL,
	}
	if err := h.deps.Store.CreateApp(app); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			writeError(w, http.StatusConflict, "app name already exists")
			return
		}
		slog.Error("api: create app", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, app)
}

// GET /apps/{app}/status
func (h *Handlers) appStatus(w http.ResponseWriter, r *http.Request) {
	appName := appNameFromPath(r, "app")
	if !validAppName.MatchString(appName) {
		writeError(w, http.StatusBadRequest, "invalid app name")
		return
	}
	status, err := h.deps.Orchestrator.Status(appName)
	if err != nil {
		slog.Error("api: app status", "app", appName, "err", err)
		writeError(w, http.StatusNotFound, "app not found")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// POST /apps/{app}/deploy  —  manual trigger
func (h *Handlers) triggerDeploy(w http.ResponseWriter, r *http.Request) {
	appName := appNameFromPath(r, "app")
	if !validAppName.MatchString(appName) {
		writeError(w, http.StatusBadRequest, "invalid app name")
		return
	}

	var req struct {
		GitSHA string `json:"git_sha"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.GitSHA == "" {
		req.GitSHA = "HEAD"
	}

	if err := h.deps.Orchestrator.Enqueue(appName, req.GitSHA, "manual"); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

// POST /apps/{app}/rollback
func (h *Handlers) rollback(w http.ResponseWriter, r *http.Request) {
	appName := appNameFromPath(r, "app")
	if !validAppName.MatchString(appName) {
		writeError(w, http.StatusBadRequest, "invalid app name")
		return
	}
	if err := h.deps.Orchestrator.Rollback(appName); err != nil {
		slog.Error("api: rollback", "app", appName, "err", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "rolled_back"})
}

// GET /apps/{app}/logs?deploy_id=<id>
// If the deploy is live, streams as SSE. If historical, reads from file.
func (h *Handlers) logs(w http.ResponseWriter, r *http.Request) {
	appName := appNameFromPath(r, "app")
	if !validAppName.MatchString(appName) {
		writeError(w, http.StatusBadRequest, "invalid app name")
		return
	}
	deployID := r.URL.Query().Get("deploy_id")

	// Validate deploy_id if provided (prevent path traversal in log file path).
	if deployID != "" {
		if len(deployID) > 64 || strings.ContainsAny(deployID, "/\\..") {
			writeError(w, http.StatusBadRequest, "invalid deploy_id")
			return
		}
	}

	// Check if a live stream exists.
	if deployID != "" {
		if streamer := h.deps.Hub.Get(deployID); streamer != nil {
			h.streamLive(w, r, streamer)
			return
		}
	}

	// Fall back to historical file.
	if deployID == "" {
		writeError(w, http.StatusBadRequest, "deploy_id required for historical logs")
		return
	}
	logPath := h.deps.Hub.LogPath(appName, deployID)
	rc, err := logstream.ReadHistorical(logPath)
	if err != nil {
		writeError(w, http.StatusNotFound, "log file not found")
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.Copy(w, rc)
}

// streamLive writes an SSE response from the live Streamer.
func (h *Handlers) streamLive(w http.ResponseWriter, r *http.Request, s *logstream.Streamer) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // tell Nginx not to buffer SSE

	ch := s.Subscribe(64)
	defer s.Unsubscribe(ch)

	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				// Streamer closed — deploy finished.
				fmt.Fprintf(w, "event: done\ndata: \n\n")
				flusher.Flush()
				return
			}
			// SSE format: "data: <line>\n\n"
			for _, line := range strings.Split(strings.TrimRight(string(chunk), "\n"), "\n") {
				fmt.Fprintf(w, "data: %s\n\n", line)
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		case <-time.After(15 * time.Second):
			// Heartbeat to keep the connection alive through proxies.
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

// webhook handles POST /webhook/{app} — delegates to the webhook package.
func (h *Handlers) webhook(w http.ResponseWriter, r *http.Request) {
	secrets := &storeSecretProvider{store: h.deps.Store}
	branchFn := func(appName string) string {
		return h.deps.Store.AppBranch(appName)
	}
	handler := webhook.Handler(secrets, h.deps.Orchestrator, branchFn)
	handler(w, r)
}

// storeSecretProvider adapts StoreInterface to webhook.SecretProvider.
type storeSecretProvider struct {
	store StoreInterface
}

func (sp *storeSecretProvider) WebhookSecret(appName string) (string, error) {
	return sp.store.WebhookSecret(appName)
}

// ---- StoreInterface adapter methods (implemented on *store.Store via adapter) ----

// StoreAdapter wraps *store.Store to implement StoreInterface (adds the two
// convenience methods the handlers need).
type StoreAdapter struct {
	S *store.Store
}

func (a *StoreAdapter) CreateApp(app *store.App) error          { return a.S.CreateApp(app) }
func (a *StoreAdapter) GetApp(name string) (*store.App, error)  { return a.S.GetApp(name) }
func (a *StoreAdapter) ListApps() ([]*store.App, error)         { return a.S.ListApps() }

func (a *StoreAdapter) WebhookSecret(appName string) (string, error) {
	app, err := a.S.GetApp(appName)
	if err != nil {
		return "", err
	}
	return app.WebhookSecret, nil
}

func (a *StoreAdapter) AppBranch(appName string) string {
	app, err := a.S.GetApp(appName)
	if err != nil {
		return "main"
	}
	return app.Branch
}

// OrchestratorAdapter adapts *orchestrator.Orchestrator to OrchestratorInterface.
type OrchestratorAdapter struct {
	O *orchestrator.Orchestrator
}

func (a *OrchestratorAdapter) Enqueue(appName, gitSHA, trigger string) error {
	return a.O.Enqueue(appName, gitSHA, trigger)
}

func (a *OrchestratorAdapter) Rollback(appName string) error {
	return a.O.Rollback(appName)
}

func (a *OrchestratorAdapter) Status(appName string) (*orchestrator.StatusResponse, error) {
	return a.O.Status(appName)
}

// LogHubAdapter adapts *logstream.Hub to LogHubInterface.
type LogHubAdapter struct {
	H *logstream.Hub
}

func (a *LogHubAdapter) Get(deployID string) *logstream.Streamer {
	return a.H.Get(deployID)
}

func (a *LogHubAdapter) LogPath(appName, deployID string) string {
	return a.H.LogPath(appName, deployID)
}

// runtime package referenced for ContainerInfo in StatusResponse — imported above.
var _ *runtime.ContainerInfo
