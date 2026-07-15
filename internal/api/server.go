// Package api provides the HTTP API server for Stevedore.
// All routes are registered here; auth middleware is applied globally
// (webhook routes use HMAC instead of bearer token).
package api

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Config holds the server's runtime configuration.
type Config struct {
	// ListenAddr is the address to bind to, e.g. ":8080".
	ListenAddr string
	// APIToken is the bearer token for all non-webhook routes.
	APIToken string
}

// Server wraps http.Server with graceful shutdown support.
type Server struct {
	cfg        Config
	httpServer *http.Server
	deps       *Deps
}

// Deps holds all the handler dependencies injected from main.
type Deps struct {
	Store        StoreInterface
	Orchestrator OrchestratorInterface
	Hub          LogHubInterface
}

// NewServer wires together all routes and middleware, returning a ready Server.
func NewServer(cfg Config, deps *Deps) *Server {
	mux := http.NewServeMux()

	s := &Server{
		cfg:  cfg,
		deps: deps,
		httpServer: &http.Server{
			Addr:              cfg.ListenAddr,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
			WriteTimeout:      0, // SSE streams need no write deadline
			IdleTimeout:       120 * time.Second,
		},
	}

	h := &Handlers{deps: deps, cfg: cfg}

	// ---- Webhook routes (HMAC-authenticated, not bearer) ----
	// Rate limiting wraps just these handlers.
	webhookLimiter := newRateLimiter(10, time.Minute) // 10 requests per minute per IP
	mux.Handle("POST /webhook/{app}", webhookLimiter.limit(http.HandlerFunc(h.webhook)))

	// ---- API routes (bearer-token authenticated) ----
	authed := func(fn http.HandlerFunc) http.Handler {
		return bearerAuth(cfg.APIToken, fn)
	}

	mux.Handle("GET /apps", authed(h.listApps))
	mux.Handle("POST /apps", authed(h.createApp))
	mux.Handle("GET /apps/{app}/status", authed(h.appStatus))
	mux.Handle("POST /apps/{app}/deploy", authed(h.triggerDeploy))
	mux.Handle("POST /apps/{app}/rollback", authed(h.rollback))
	mux.Handle("GET /apps/{app}/logs", authed(h.logs))

	return s
}

// Run starts the HTTP server and blocks until a SIGTERM or SIGINT is received,
// then shuts down gracefully.
func (s *Server) Run() error {
	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return err
	}
	slog.Info("stevedore agent listening", "addr", ln.Addr())

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("http server error", "err", err)
		}
	}()

	<-stop
	slog.Info("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = s.httpServer.Shutdown(ctx)
	wg.Wait()
	return nil
}

// bearerAuth is middleware that checks Authorization: Bearer <token> using
// constant-time comparison to prevent timing attacks.
func bearerAuth(token string, next http.HandlerFunc) http.Handler {
	tokenBytes := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if len(auth) <= len(prefix) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		got := []byte(auth[len(prefix):])
		if subtle.ConstantTimeCompare(got, tokenBytes) != 1 {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	})
}

// ---- Simple token-bucket rate limiter (per remote IP) ----

type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	max     int
	window  time.Duration
}

type bucket struct {
	count    int
	resetAt  time.Time
}

func newRateLimiter(max int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		buckets: make(map[string]*bucket),
		max:     max,
		window:  window,
	}
}

func (rl *rateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.buckets[ip]
	now := time.Now()
	if !ok || now.After(b.resetAt) {
		rl.buckets[ip] = &bucket{count: 1, resetAt: now.Add(rl.window)}
		return true
	}
	if b.count >= rl.max {
		return false
	}
	b.count++
	return true
}

func (rl *rateLimiter) limit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		if !rl.allow(ip) {
			http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
