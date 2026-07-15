// Package runtime manages the lifecycle of application containers:
// start, stop, health check, and automatic rollback on health-check failure.
package runtime

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	// containerPrefix is prepended to every app container name.
	containerPrefix = "stevedore-"
	// stopTimeout is the graceful shutdown period before SIGKILL.
	stopTimeout = 10
	// healthCheckAttempts is how many times we try the health URL.
	healthCheckAttempts = 5
	// healthCheckInterval is the wait between health-check attempts.
	healthCheckInterval = 5 * time.Second
)

// ContainerInfo holds a summary of a running container.
type ContainerInfo struct {
	ContainerID string `json:"container_id"`
	ImageTag    string `json:"image_tag"`
	Status      string `json:"status"`
}

// Manager controls Docker containers for Stevedore-managed apps.
type Manager struct {
	cli *client.Client
}

// New creates a Manager using the host Docker daemon.
func New() (*Manager, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("runtime: docker client: %w", err)
	}
	return &Manager{cli: cli}, nil
}

// Close closes the Docker client.
func (m *Manager) Close() error {
	return m.cli.Close()
}

// containerName returns the deterministic Docker container name for an app.
func containerName(app string) string {
	return containerPrefix + app
}

// Deploy starts a new container for imageTag, runs health checks, and on
// failure auto-rolls back to previousImageTag (if non-empty).
// envFile is a path on the host containing KEY=VALUE lines; we read it and
// pass the values to Docker. Env values are never written to w.
func (m *Manager) Deploy(
	ctx context.Context,
	app, imageTag, previousImageTag, envFile, healthCheckURL string,
	w io.Writer,
) error {
	name := containerName(app)

	// Stop and remove the currently running container (if any).
	fmt.Fprintf(w, "[stevedore] Stopping existing container %s\n", name)
	if err := m.stopAndRemove(ctx, name); err != nil {
		// Non-fatal — there may be no running container on first deploy.
		fmt.Fprintf(w, "[stevedore] (no existing container to stop: %v)\n", err)
	}

	// Load env vars from the env file (values never echoed to log).
	envVars, err := loadEnvFile(envFile)
	if err != nil {
		return fmt.Errorf("runtime: load env file: %w", err)
	}

	// Start the new container.
	fmt.Fprintf(w, "[stevedore] Starting container %s from %s\n", name, imageTag)
	if err := m.startContainer(ctx, name, imageTag, envVars); err != nil {
		return fmt.Errorf("runtime: start container: %w", err)
	}

	// Health check.
	if healthCheckURL != "" {
		fmt.Fprintf(w, "[stevedore] Running health checks against %s\n", healthCheckURL)
		if err := m.waitHealthy(ctx, healthCheckURL, healthCheckAttempts, healthCheckInterval, w); err != nil {
			fmt.Fprintf(w, "[stevedore][error] Health check failed: %v\n", err)

			// Auto-rollback: stop the new container and restart the previous one.
			_ = m.stopAndRemove(ctx, name)
			if previousImageTag != "" {
				fmt.Fprintf(w, "[stevedore] Rolling back to %s\n", previousImageTag)
				if rbErr := m.startContainer(ctx, name, previousImageTag, envVars); rbErr != nil {
					fmt.Fprintf(w, "[stevedore][error] Rollback start failed: %v\n", rbErr)
				} else {
					fmt.Fprintf(w, "[stevedore] Rollback complete.\n")
				}
			}
			return fmt.Errorf("runtime: health check failed (auto-rolled-back): %w", err)
		}
	}

	fmt.Fprintf(w, "[stevedore] Container %s is healthy and live.\n", name)
	return nil
}

// Stop stops and removes the container for app without starting a replacement.
func (m *Manager) Stop(ctx context.Context, app string) error {
	return m.stopAndRemove(ctx, containerName(app))
}

// GetContainerInfo returns status info for the container of app, or nil if
// no container is found.
func (m *Manager) GetContainerInfo(ctx context.Context, app string) (*ContainerInfo, error) {
	f := filters.NewArgs()
	f.Add("name", containerName(app))
	containers, err := m.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: f,
	})
	if err != nil {
		return nil, fmt.Errorf("runtime: list containers: %w", err)
	}
	if len(containers) == 0 {
		return nil, nil
	}
	c := containers[0]
	id := c.ID
	if len(id) > 12 {
		id = id[:12]
	}
	imgTag := ""
	if len(c.Image) > 0 {
		imgTag = c.Image
	}
	return &ContainerInfo{
		ContainerID: id,
		ImageTag:    imgTag,
		Status:      c.Status,
	}, nil
}

// startContainer creates and starts a container with the given image and env vars.
// Env values are passed as a slice; they are never written to the log streamer.
func (m *Manager) startContainer(ctx context.Context, name, imageTag string, envVars []string) error {
	hostCfg := &container.HostConfig{
		RestartPolicy: container.RestartPolicy{Name: "unless-stopped"},
		NetworkMode:   "bridge",
	}

	resp, err := m.cli.ContainerCreate(ctx, &container.Config{
		Image: imageTag,
		Env:   envVars, // values not echoed to log
		Labels: map[string]string{
			"managed-by":      "stevedore",
			"stevedore.app":   name,
			"stevedore.image": imageTag,
		},
	}, hostCfg, &network.NetworkingConfig{}, (*ocispec.Platform)(nil), name)
	if err != nil {
		return fmt.Errorf("container create: %w", err)
	}

	if err := m.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("container start: %w", err)
	}
	return nil
}

// stopAndRemove gracefully stops and removes a container by name.
func (m *Manager) stopAndRemove(ctx context.Context, name string) error {
	timeout := stopTimeout
	if err := m.cli.ContainerStop(ctx, name, container.StopOptions{Timeout: &timeout}); err != nil {
		if !isNotFound(err) {
			return err
		}
		return nil // container didn't exist — fine
	}
	if err := m.cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true}); err != nil {
		if !isNotFound(err) {
			slog.Warn("runtime: remove container", "name", name, "err", err)
		}
	}
	return nil
}

// waitHealthy polls healthCheckURL until it returns HTTP 2xx/3xx or attempts are exhausted.
func (m *Manager) waitHealthy(ctx context.Context, url string, attempts int, interval time.Duration, w io.Writer) error {
	hc := &http.Client{Timeout: 5 * time.Second}
	for i := 1; i <= attempts; i++ {
		fmt.Fprintf(w, "[stevedore] Health check attempt %d/%d ...\n", i, attempts)
		resp, err := hc.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				return nil
			}
			fmt.Fprintf(w, "[stevedore] Health check returned HTTP %d\n", resp.StatusCode)
		} else {
			fmt.Fprintf(w, "[stevedore] Health check error: %v\n", err)
		}
		if i < attempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interval):
			}
		}
	}
	return fmt.Errorf("health check failed after %d attempts", attempts)
}

// isNotFound returns true if the Docker error indicates the resource was not found.
func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "No such container")
}

// loadEnvFile reads KEY=VALUE pairs from a file on disk.
// Returns nil (no error) if envFile is empty or does not exist.
// Values are NEVER written to any log; only keys are accessible to the caller
// via the returned slice — and the slice itself goes into Docker, not our logs.
func loadEnvFile(envFile string) ([]string, error) {
	if envFile == "" {
		return nil, nil
	}
	f, err := os.Open(envFile)
	if os.IsNotExist(err) {
		return nil, nil // env file optional
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var vars []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		vars = append(vars, line)
	}
	return vars, scanner.Err()
}

// Ensure image package is used (for future PruneOldImages calls via build).
var _ image.Summary
