// Package webhook handles inbound GitHub push webhooks.
// It verifies the HMAC-SHA256 signature before doing anything else —
// an unverified request never touches the deploy queue.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// Enqueuer is implemented by the orchestrator to accept inbound deploy jobs.
type Enqueuer interface {
	Enqueue(appName, gitSHA, trigger string) error
}

// SecretProvider returns the per-app webhook secret. A separate interface so
// the webhook package does not import the store directly.
type SecretProvider interface {
	WebhookSecret(appName string) (string, error)
}

// pushEvent is the minimal subset of a GitHub push payload we need.
type pushEvent struct {
	Ref  string `json:"ref"` // e.g. "refs/heads/main"
	After string `json:"after"` // full git SHA of the new HEAD
	Repository struct {
		CloneURL string `json:"clone_url"`
	} `json:"repository"`
}

// Handler returns an http.HandlerFunc that:
//  1. Reads the raw body and verifies X-Hub-Signature-256 via constant-time HMAC comparison.
//  2. Parses the push event and extracts the branch name.
//  3. Enqueues a deploy job to the orchestrator (non-blocking).
//  4. Returns 202 Accepted immediately.
//
// appName is extracted from the URL path by the caller (e.g. /webhook/{app}).
func Handler(secrets SecretProvider, enqueuer Enqueuer, expectedBranch func(appName string) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Extract app name from path: /webhook/{app}
		appName := strings.TrimPrefix(r.URL.Path, "/webhook/")
		appName = strings.Trim(appName, "/")
		if appName == "" {
			http.Error(w, "missing app name", http.StatusBadRequest)
			return
		}

		// Read body in full — we need it for HMAC.
		body, err := io.ReadAll(io.LimitReader(r.Body, 5<<20)) // 5 MB limit
		if err != nil {
			slog.Error("webhook: read body", "app", appName, "err", err)
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}

		// Fetch the app's webhook secret.
		secret, err := secrets.WebhookSecret(appName)
		if err != nil {
			// Return 404 so callers can't enumerate app names via timing.
			slog.Warn("webhook: unknown app", "app", appName)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		// Verify HMAC — constant-time.
		if !verifySignature(body, secret, r.Header.Get("X-Hub-Signature-256")) {
			slog.Warn("webhook: invalid signature", "app", appName)
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}

		// Only process push events.
		if r.Header.Get("X-GitHub-Event") != "push" {
			w.WriteHeader(http.StatusOK)
			return
		}

		var event pushEvent
		if err := json.Unmarshal(body, &event); err != nil {
			slog.Error("webhook: parse push event", "app", appName, "err", err)
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}

		// Only trigger a deploy if this push targets the configured branch.
		branch := strings.TrimPrefix(event.Ref, "refs/heads/")
		if expected := expectedBranch(appName); expected != "" && branch != expected {
			slog.Info("webhook: ignoring push to non-deploy branch", "app", appName, "branch", branch)
			w.WriteHeader(http.StatusOK)
			return
		}

		gitSHA := event.After
		if gitSHA == "" || gitSHA == "0000000000000000000000000000000000000000" {
			// Branch deletion event — ignore.
			w.WriteHeader(http.StatusOK)
			return
		}

		slog.Info("webhook: enqueuing deploy", "app", appName, "sha", gitSHA[:12], "branch", branch)
		if err := enqueuer.Enqueue(appName, gitSHA, "webhook"); err != nil {
			slog.Warn("webhook: enqueue failed (deploy already running?)", "app", appName, "err", err)
		}

		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintln(w, `{"status":"accepted"}`)
	}
}

// verifySignature computes HMAC-SHA256 of body with secret and compares it
// to the signature header using constant-time comparison.
// signature is expected in the form "sha256=<hex>".
func verifySignature(body []byte, secret, signature string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(signature, prefix) {
		return false
	}
	gotHex := strings.TrimPrefix(signature, prefix)
	got, err := hex.DecodeString(gotHex)
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := mac.Sum(nil)

	return hmac.Equal(got, expected)
}
