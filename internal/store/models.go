package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned when a queried record does not exist.
var ErrNotFound = errors.New("store: not found")

// -------- App --------

// App represents a registered application managed by Stevedore.
type App struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	RepoURL         string     `json:"repo_url"`
	Branch          string     `json:"branch"`
	WebhookSecret   string     `json:"-"` // never serialised — treat as secret
	EnvFile         string     `json:"env_file"`
	HealthCheckURL  string     `json:"health_check_url"`
	CurrentDeployID string     `json:"current_deploy_id"`
	CreatedAt       time.Time  `json:"created_at"`
}

// CreateApp inserts a new app record.
func (s *Store) CreateApp(a *App) error {
	_, err := s.db.Exec(`
		INSERT INTO apps (id, name, repo_url, branch, webhook_secret, env_file, health_check_url)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.Name, a.RepoURL, a.Branch, a.WebhookSecret, a.EnvFile, a.HealthCheckURL,
	)
	if err != nil {
		return fmt.Errorf("store: create app: %w", err)
	}
	return nil
}

// GetApp fetches an app by its name (the natural user-facing key).
func (s *Store) GetApp(name string) (*App, error) {
	row := s.db.QueryRow(`
		SELECT id, name, repo_url, branch, webhook_secret, env_file, health_check_url,
		       COALESCE(current_deploy_id, ''), created_at
		FROM apps WHERE name = ?`, name)
	return scanApp(row)
}

// GetAppByID fetches an app by its internal UUID.
func (s *Store) GetAppByID(id string) (*App, error) {
	row := s.db.QueryRow(`
		SELECT id, name, repo_url, branch, webhook_secret, env_file, health_check_url,
		       COALESCE(current_deploy_id, ''), created_at
		FROM apps WHERE id = ?`, id)
	return scanApp(row)
}

// ListApps returns all registered apps ordered by name.
func (s *Store) ListApps() ([]*App, error) {
	rows, err := s.db.Query(`
		SELECT id, name, repo_url, branch, webhook_secret, env_file, health_check_url,
		       COALESCE(current_deploy_id, ''), created_at
		FROM apps ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list apps: %w", err)
	}
	defer rows.Close()

	var apps []*App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		apps = append(apps, a)
	}
	return apps, rows.Err()
}

// SetCurrentDeploy updates an app's current_deploy_id.
func (s *Store) SetCurrentDeploy(appID, deployID string) error {
	_, err := s.db.Exec(`UPDATE apps SET current_deploy_id = ? WHERE id = ?`, deployID, appID)
	if err != nil {
		return fmt.Errorf("store: set current deploy: %w", err)
	}
	return nil
}

func scanApp(s interface {
	Scan(...any) error
}) (*App, error) {
	a := &App{}
	var createdAt string
	err := s.Scan(
		&a.ID, &a.Name, &a.RepoURL, &a.Branch, &a.WebhookSecret,
		&a.EnvFile, &a.HealthCheckURL, &a.CurrentDeployID, &createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan app: %w", err)
	}
	a.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
	if a.CreatedAt.IsZero() {
		a.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	}
	return a, nil
}

// -------- Deploy --------

// DeployStatus represents the lifecycle state of a deploy.
type DeployStatus string

const (
	StatusPending    DeployStatus = "pending"
	StatusRunning    DeployStatus = "running"
	StatusSuccess    DeployStatus = "success"
	StatusFailed     DeployStatus = "failed"
	StatusRolledBack DeployStatus = "rolled_back"
)

// Deploy represents a single deploy attempt.
type Deploy struct {
	ID         string       `json:"id"`
	AppID      string       `json:"app_id"`
	GitSHA     string       `json:"git_sha"`
	ImageTag   string       `json:"image_tag"`
	Status     DeployStatus `json:"status"`
	Trigger    string       `json:"trigger"`
	StartedAt  time.Time    `json:"started_at"`
	FinishedAt *time.Time   `json:"finished_at,omitempty"`
	ErrorMsg   string       `json:"error_msg,omitempty"`
}

// CreateDeploy inserts a new deploy record with status=pending.
func (s *Store) CreateDeploy(d *Deploy) error {
	_, err := s.db.Exec(`
		INSERT INTO deploys (id, app_id, git_sha, image_tag, status, trigger)
		VALUES (?, ?, ?, ?, ?, ?)`,
		d.ID, d.AppID, d.GitSHA, d.ImageTag, d.Status, d.Trigger,
	)
	if err != nil {
		return fmt.Errorf("store: create deploy: %w", err)
	}
	return nil
}

// UpdateDeploy updates a deploy's mutable fields (status, finished_at, error_msg).
func (s *Store) UpdateDeploy(d *Deploy) error {
	var finishedAt any
	if d.FinishedAt != nil {
		finishedAt = d.FinishedAt.UTC().Format("2006-01-02 15:04:05")
	}
	_, err := s.db.Exec(`
		UPDATE deploys SET status = ?, finished_at = ?, error_msg = ? WHERE id = ?`,
		string(d.Status), finishedAt, d.ErrorMsg, d.ID,
	)
	if err != nil {
		return fmt.Errorf("store: update deploy: %w", err)
	}
	return nil
}

// GetDeploy fetches a single deploy by ID.
func (s *Store) GetDeploy(id string) (*Deploy, error) {
	row := s.db.QueryRow(`
		SELECT id, app_id, git_sha, image_tag, status, trigger, started_at,
		       finished_at, COALESCE(error_msg, '')
		FROM deploys WHERE id = ?`, id)
	return scanDeploy(row)
}

// GetLatestSuccessfulDeploy returns the most recent deploy with status=success for an app,
// excluding the given deployID (used to find the rollback target).
func (s *Store) GetLatestSuccessfulDeploy(appID, excludeDeployID string) (*Deploy, error) {
	row := s.db.QueryRow(`
		SELECT id, app_id, git_sha, image_tag, status, trigger, started_at,
		       finished_at, COALESCE(error_msg, '')
		FROM deploys
		WHERE app_id = ? AND status = 'success' AND id != ?
		ORDER BY started_at DESC LIMIT 1`,
		appID, excludeDeployID,
	)
	return scanDeploy(row)
}

// ListDeploys returns the most recent deploys for an app (latest first).
func (s *Store) ListDeploys(appID string, limit int) ([]*Deploy, error) {
	rows, err := s.db.Query(`
		SELECT id, app_id, git_sha, image_tag, status, trigger, started_at,
		       finished_at, COALESCE(error_msg, '')
		FROM deploys WHERE app_id = ?
		ORDER BY started_at DESC LIMIT ?`, appID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list deploys: %w", err)
	}
	defer rows.Close()

	var deploys []*Deploy
	for rows.Next() {
		d, err := scanDeploy(rows)
		if err != nil {
			return nil, err
		}
		deploys = append(deploys, d)
	}
	return deploys, rows.Err()
}

func scanDeploy(s interface {
	Scan(...any) error
}) (*Deploy, error) {
	d := &Deploy{}
	var startedAt, finishedAt sql.NullString
	err := s.Scan(
		&d.ID, &d.AppID, &d.GitSHA, &d.ImageTag, &d.Status, &d.Trigger,
		&startedAt, &finishedAt, &d.ErrorMsg,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: scan deploy: %w", err)
	}
	if startedAt.Valid {
		d.StartedAt = parseTime(startedAt.String)
	}
	if finishedAt.Valid && finishedAt.String != "" {
		t := parseTime(finishedAt.String)
		if !t.IsZero() {
			d.FinishedAt = &t
		}
	}
	return d, nil
}

// parseTime tries the common SQLite datetime string formats in order.
func parseTime(s string) time.Time {
	formats := []string{
		"2006-01-02 15:04:05",
		"2006-01-02 15:04:05.999999999",
		time.RFC3339,
		time.RFC3339Nano,
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

