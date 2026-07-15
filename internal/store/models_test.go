package store_test

import (
	"fmt"
	"testing"

	"github.com/rence/stevedore/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	// Use an in-memory SQLite database for tests.
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestCreateAndGetApp(t *testing.T) {
	st := openTestStore(t)

	app := &store.App{
		ID:            "app-001",
		Name:          "myapp",
		RepoURL:       "https://github.com/example/myapp.git",
		Branch:        "main",
		WebhookSecret: "secret",
	}
	if err := st.CreateApp(app); err != nil {
		t.Fatalf("create app: %v", err)
	}

	got, err := st.GetApp("myapp")
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	if got.ID != "app-001" {
		t.Errorf("expected ID app-001, got %s", got.ID)
	}
	if got.Branch != "main" {
		t.Errorf("expected branch main, got %s", got.Branch)
	}
}

func TestGetApp_NotFound(t *testing.T) {
	st := openTestStore(t)
	_, err := st.GetApp("nonexistent")
	if err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestListApps(t *testing.T) {
	st := openTestStore(t)
	for i := 0; i < 3; i++ {
		_ = st.CreateApp(&store.App{
			ID:      fmt.Sprintf("id-%d", i),
			Name:    fmt.Sprintf("app-%d", i),
			RepoURL: "https://github.com/example/repo.git",
			Branch:  "main",
		})
	}
	apps, err := st.ListApps()
	if err != nil {
		t.Fatalf("list apps: %v", err)
	}
	if len(apps) != 3 {
		t.Errorf("expected 3 apps, got %d", len(apps))
	}
}

func TestCreateAndUpdateDeploy(t *testing.T) {
	st := openTestStore(t)
	_ = st.CreateApp(&store.App{
		ID: "app-1", Name: "app", RepoURL: "http://x", Branch: "main",
	})

	d := &store.Deploy{
		ID:       "deploy-1",
		AppID:    "app-1",
		GitSHA:   "abc123",
		ImageTag: "app:abc123-20240101",
		Status:   store.StatusRunning,
		Trigger:  "manual",
	}
	if err := st.CreateDeploy(d); err != nil {
		t.Fatalf("create deploy: %v", err)
	}

	d.Status = store.StatusSuccess
	if err := st.UpdateDeploy(d); err != nil {
		t.Fatalf("update deploy: %v", err)
	}

	got, err := st.GetDeploy("deploy-1")
	if err != nil {
		t.Fatalf("get deploy: %v", err)
	}
	if got.Status != store.StatusSuccess {
		t.Errorf("expected status success, got %s", got.Status)
	}
}

func TestGetLatestSuccessfulDeploy(t *testing.T) {
	st := openTestStore(t)
	_ = st.CreateApp(&store.App{ID: "a1", Name: "app", RepoURL: "http://x", Branch: "main"})

	for i, status := range []store.DeployStatus{store.StatusSuccess, store.StatusFailed, store.StatusSuccess} {
		d := &store.Deploy{
			ID: fmt.Sprintf("d%d", i), AppID: "a1",
			GitSHA: "sha", ImageTag: fmt.Sprintf("app:v%d", i),
			Status: status, Trigger: "manual",
		}
		_ = st.CreateDeploy(d)
	}

	// Exclude the newest success (d2) to simulate finding rollback target.
	prev, err := st.GetLatestSuccessfulDeploy("a1", "d2")
	if err != nil {
		t.Fatalf("get latest successful: %v", err)
	}
	if prev.ID != "d0" {
		t.Errorf("expected d0, got %s", prev.ID)
	}
}
