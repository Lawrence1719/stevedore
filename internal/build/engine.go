// Package build wraps the Docker SDK to build images from a Dockerfile,
// tag them deterministically, stream build output, and prune old images.
package build

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"time"

	dockerbuild "github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	goarchive "github.com/moby/go-archive"
)

// Engine builds Docker images via the host Docker daemon.
type Engine struct {
	cli *client.Client
}

// New creates an Engine using the DOCKER_HOST environment variable or the
// default socket (/var/run/docker.sock).
func New() (*Engine, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("build: docker client: %w", err)
	}
	return &Engine{cli: cli}, nil
}

// Close closes the underlying Docker client.
func (e *Engine) Close() error {
	return e.cli.Close()
}

// buildLine is the JSON shape Docker uses for build progress events.
type buildLine struct {
	Stream string `json:"stream"`
	Error  string `json:"error"`
}

// Build builds a Docker image from the Dockerfile in repoPath.
// It tags the image as "{app}:{gitSHA}-{timestamp}" and streams human-readable
// build output to w. Returns the full image tag on success.
func (e *Engine) Build(ctx context.Context, app, gitSHA, repoPath string, w io.Writer) (string, error) {
	shaShort := gitSHA
	if len(shaShort) > 12 {
		shaShort = shaShort[:12]
	}
	tag := fmt.Sprintf("%s:%s-%s", app, shaShort, time.Now().UTC().Format("20060102-150405"))

	fmt.Fprintf(w, "[stevedore] Building image %s from %s\n", tag, repoPath)

	buildCtx, err := goarchive.TarWithOptions(repoPath, &goarchive.TarOptions{})
	if err != nil {
		return "", fmt.Errorf("build: tar context: %w", err)
	}
	defer buildCtx.Close()

	resp, err := e.cli.ImageBuild(ctx, buildCtx, dockerbuild.ImageBuildOptions{
		Tags:        []string{tag},
		Remove:      true,
		ForceRemove: true,
		Dockerfile:  "Dockerfile",
	})
	if err != nil {
		return "", fmt.Errorf("build: image build: %w", err)
	}
	defer resp.Body.Close()

	if err := streamBuildOutput(resp.Body, w); err != nil {
		return "", err
	}

	fmt.Fprintf(w, "[stevedore] Build complete: %s\n", tag)
	return tag, nil
}

// streamBuildOutput decodes the NDJSON build response from Docker and writes
// plain-text lines to w. Returns an error if Docker reports a build error.
func streamBuildOutput(r io.Reader, w io.Writer) error {
	dec := json.NewDecoder(r)
	for {
		var line buildLine
		if err := dec.Decode(&line); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("build: decode output: %w", err)
		}
		if line.Error != "" {
			fmt.Fprintf(w, "[stevedore][error] %s\n", line.Error)
			return fmt.Errorf("build: docker error: %s", line.Error)
		}
		if line.Stream != "" {
			fmt.Fprint(w, line.Stream)
		}
	}
	return nil
}

// PruneOldImages removes images for the given app, keeping only the keepN
// most recent ones. If keepN <= 0 it defaults to 5.
func (e *Engine) PruneOldImages(ctx context.Context, app string, keepN int) {
	if keepN <= 0 {
		keepN = 5
	}

	f := filters.NewArgs()
	f.Add("reference", app+":*")

	images, err := e.cli.ImageList(ctx, image.ListOptions{Filters: f})
	if err != nil {
		slog.Error("build: list images for pruning", "app", app, "err", err)
		return
	}

	// Sort by Created descending (newest first).
	sort.Slice(images, func(i, j int) bool {
		return images[i].Created > images[j].Created
	})

	for i := keepN; i < len(images); i++ {
		img := images[i]
		if len(img.RepoTags) == 0 {
			continue
		}
		slog.Info("build: pruning old image", "app", app, "image", img.RepoTags[0])
		if _, err := e.cli.ImageRemove(ctx, img.ID, image.RemoveOptions{Force: false}); err != nil {
			slog.Warn("build: could not remove image", "image", img.ID, "err", err)
		}
	}
}

// CloneOrPull ensures repoPath contains an up-to-date clone of repoURL at branch.
// It uses the git binary via exec.Command with a slice of args — never a shell string.
func CloneOrPull(ctx context.Context, repoURL, branch, repoPath string, w io.Writer) error {
	fmt.Fprintf(w, "[stevedore] Fetching %s @ %s\n", repoURL, branch)

	if _, err := os.Stat(repoPath + "/.git"); os.IsNotExist(err) {
		return runGit(ctx, w, ".", "clone", "--branch", branch, "--depth", "1", repoURL, repoPath)
	}

	// Repo already cloned — fetch + reset to latest.
	if err := runGit(ctx, w, repoPath, "fetch", "--depth", "1", "origin", branch); err != nil {
		return err
	}
	return runGit(ctx, w, repoPath, "reset", "--hard", "origin/"+branch)
}
