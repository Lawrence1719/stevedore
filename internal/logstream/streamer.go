// Package logstream provides a per-deploy log broadcaster that fans out to
// multiple SSE subscribers while also persisting every byte to a log file on disk.
package logstream

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// Streamer is created once per deploy. It implements io.Writer so build/runtime
// code can write to it directly. Each Write fans out to all active subscribers
// and appends to the deploy's log file.
type Streamer struct {
	deployID string
	logPath  string

	mu          sync.RWMutex
	subscribers map[chan []byte]struct{}
	closed      bool

	file *os.File
}

// Hub manages Streamers across all active deploys.
type Hub struct {
	mu      sync.Mutex
	streams map[string]*Streamer // keyed by deployID
	logDir  string
}

// NewHub creates a Hub that stores log files under logDir.
func NewHub(logDir string) *Hub {
	return &Hub{
		streams: make(map[string]*Streamer),
		logDir:  logDir,
	}
}

// Create starts a new Streamer for the given deploy. Calling code must call
// Close when the deploy finishes so subscribers receive a terminal signal.
func (h *Hub) Create(appName, deployID string) (*Streamer, error) {
	dir := filepath.Join(h.logDir, appName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("logstream: mkdir %s: %w", dir, err)
	}
	logPath := filepath.Join(dir, deployID+".log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("logstream: open log file: %w", err)
	}

	s := &Streamer{
		deployID:    deployID,
		logPath:     logPath,
		subscribers: make(map[chan []byte]struct{}),
		file:        f,
	}

	h.mu.Lock()
	h.streams[deployID] = s
	h.mu.Unlock()

	return s, nil
}

// Get returns an active Streamer by deployID, or nil if none exists.
func (h *Hub) Get(deployID string) *Streamer {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.streams[deployID]
}

// Remove removes a closed Streamer from the Hub index.
func (h *Hub) Remove(deployID string) {
	h.mu.Lock()
	delete(h.streams, deployID)
	h.mu.Unlock()
}

// LogPath returns the on-disk path for a deploy's log file (may not exist for
// very old deploys if pruned, but safe to try).
func (h *Hub) LogPath(appName, deployID string) string {
	return filepath.Join(h.logDir, appName, deployID+".log")
}

// -------- Streamer methods --------

// Write satisfies io.Writer. It appends p to the log file and broadcasts to
// all current subscribers. Writes after Close are silently dropped.
func (s *Streamer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return len(p), nil
	}

	// Persist to disk first.
	if _, err := s.file.Write(p); err != nil {
		slog.Error("logstream: write to file", "err", err, "deploy", s.deployID)
	}

	// Fan out to subscribers; use non-blocking send so a slow subscriber
	// never stalls the build.
	buf := make([]byte, len(p))
	copy(buf, p)
	for ch := range s.subscribers {
		select {
		case ch <- buf:
		default:
			// Subscriber is too slow; drop this chunk for that subscriber only.
		}
	}
	return len(p), nil
}

// Subscribe returns a channel that receives log chunks as they arrive.
// The channel is closed when the Streamer is closed.
// bufSize controls the channel buffer (use 64 for SSE handlers).
func (s *Streamer) Subscribe(bufSize int) <-chan []byte {
	ch := make(chan []byte, bufSize)
	s.mu.Lock()
	if s.closed {
		close(ch)
	} else {
		s.subscribers[ch] = struct{}{}
	}
	s.mu.Unlock()
	return ch
}

// Unsubscribe removes and closes a subscriber channel.
func (s *Streamer) Unsubscribe(ch <-chan []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.subscribers {
		if k == ch {
			delete(s.subscribers, k)
			close(k)
			return
		}
	}
}

// Close signals all subscribers that no more data will arrive, flushes and
// closes the log file. Safe to call multiple times.
func (s *Streamer) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	for ch := range s.subscribers {
		close(ch)
	}
	s.subscribers = make(map[chan []byte]struct{})
	if s.file != nil {
		_ = s.file.Sync()
		_ = s.file.Close()
	}
}

// ReadHistorical opens the log file for a completed deploy and returns an
// io.ReadCloser. The caller must close it.
func ReadHistorical(logPath string) (io.ReadCloser, error) {
	f, err := os.Open(logPath)
	if err != nil {
		return nil, fmt.Errorf("logstream: open historical log %s: %w", logPath, err)
	}
	return f, nil
}
