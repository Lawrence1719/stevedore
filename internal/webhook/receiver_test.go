package webhook_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rence/stevedore/internal/webhook"
)

// ---- Test doubles ----

type mockSecrets struct {
	secrets map[string]string
}

func (m *mockSecrets) WebhookSecret(appName string) (string, error) {
	s, ok := m.secrets[appName]
	if !ok {
		return "", fmt.Errorf("not found")
	}
	return s, nil
}

type mockEnqueuer struct {
	enqueued []string
}

func (m *mockEnqueuer) Enqueue(appName, gitSHA, trigger string) error {
	m.enqueued = append(m.enqueued, fmt.Sprintf("%s:%s:%s", appName, gitSHA, trigger))
	return nil
}

// sign computes the GitHub-style HMAC-SHA256 signature for body.
func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// pushPayload builds a minimal GitHub push event JSON.
func pushPayload(ref, after string) string {
	b, _ := json.Marshal(map[string]any{
		"ref":   ref,
		"after": after,
		"repository": map[string]string{
			"clone_url": "https://github.com/example/app.git",
		},
	})
	return string(b)
}

func makeHandler(secrets map[string]string, enqueuer *mockEnqueuer) http.Handler {
	s := &mockSecrets{secrets: secrets}
	branchFn := func(_ string) string { return "main" }
	return webhook.Handler(s, enqueuer, branchFn)
}

// ---- Tests ----

func TestWebhook_ValidSignature_Accepted(t *testing.T) {
	enqueuer := &mockEnqueuer{}
	h := makeHandler(map[string]string{"myapp": "supersecret"}, enqueuer)

	body := pushPayload("refs/heads/main", "abc123def456abc123def456abc123def456abc1")
	sig := sign("supersecret", body)

	req := httptest.NewRequest("POST", "/webhook/myapp", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Event", "push")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(enqueuer.enqueued) != 1 {
		t.Fatalf("expected 1 enqueued job, got %d", len(enqueuer.enqueued))
	}
}

func TestWebhook_WrongSecret_Rejected(t *testing.T) {
	enqueuer := &mockEnqueuer{}
	h := makeHandler(map[string]string{"myapp": "supersecret"}, enqueuer)

	body := pushPayload("refs/heads/main", "abc123def456abc123def456abc123def456abc1")
	// Sign with a different secret.
	sig := sign("wrongsecret", body)

	req := httptest.NewRequest("POST", "/webhook/myapp", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Event", "push")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
	if len(enqueuer.enqueued) != 0 {
		t.Fatalf("job should NOT have been enqueued on bad signature")
	}
}

func TestWebhook_TamperedBody_Rejected(t *testing.T) {
	enqueuer := &mockEnqueuer{}
	h := makeHandler(map[string]string{"myapp": "supersecret"}, enqueuer)

	original := pushPayload("refs/heads/main", "abc123def456abc123def456abc123def456abc1")
	sig := sign("supersecret", original)

	// Tamper with the body after signing.
	tampered := strings.Replace(original, "main", "evil", 1)

	req := httptest.NewRequest("POST", "/webhook/myapp", strings.NewReader(tampered))
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Event", "push")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for tampered body, got %d", rr.Code)
	}
}

func TestWebhook_UnknownApp_NotFound(t *testing.T) {
	enqueuer := &mockEnqueuer{}
	h := makeHandler(map[string]string{}, enqueuer) // no apps registered

	body := pushPayload("refs/heads/main", "abc123def456abc123def456abc123def456abc1")
	sig := sign("anysecret", body)

	req := httptest.NewRequest("POST", "/webhook/unknownapp", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Event", "push")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

func TestWebhook_WrongBranch_Ignored(t *testing.T) {
	enqueuer := &mockEnqueuer{}

	s := &mockSecrets{secrets: map[string]string{"myapp": "secret"}}
	// App is configured for "main" but push is to "feature/x".
	h := webhook.Handler(s, enqueuer, func(_ string) string { return "main" })

	body := pushPayload("refs/heads/feature/x", "abc123def456abc123def456abc123def456abc1")
	sig := sign("secret", body)

	req := httptest.NewRequest("POST", "/webhook/myapp", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Event", "push")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 (ignored), got %d", rr.Code)
	}
	if len(enqueuer.enqueued) != 0 {
		t.Fatalf("expected no deploy enqueued for wrong branch")
	}
}

func TestWebhook_MissingSignatureHeader_Rejected(t *testing.T) {
	enqueuer := &mockEnqueuer{}
	h := makeHandler(map[string]string{"myapp": "secret"}, enqueuer)

	body := pushPayload("refs/heads/main", "abc123def456abc123def456abc123def456abc1")

	req := httptest.NewRequest("POST", "/webhook/myapp", strings.NewReader(body))
	// No X-Hub-Signature-256 header.
	req.Header.Set("X-GitHub-Event", "push")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing signature, got %d", rr.Code)
	}
}
