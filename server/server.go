// Package server provides the HTTP API and embedded UI for racevis.
// It exposes one data endpoint (/api/timeline) and serves the frontend HTML.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/bramakrishna16/racevis/correlator"
)

// embeddedUI is injected by main via SetEmbeddedUI before Start() is called.

// Server holds the HTTP server state and the pre-computed timeline to serve.
type Server struct {
	mu            sync.RWMutex
	timeline      *correlator.Timeline
	preferredPort int
	httpServer    *http.Server
	embeddedUI    []byte        // pre-read HTML, nil means serve from disk
	sseClients    []chan string // watch mode: channels to push reload events
}

// SetEmbeddedUI injects the compiled-in frontend HTML into the server.
// Call before Start(). When set, the binary is fully self-contained and
// works from any directory without needing ui/index.html on disk.
func (s *Server) SetEmbeddedUI(data []byte) {
	s.embeddedUI = data
}

// New creates a Server. tl must not be nil.
func New(tl *correlator.Timeline, port int) *Server {
	if tl == nil {
		panic("server.New: timeline must not be nil")
	}
	return &Server{timeline: tl, preferredPort: port}
}

// Start binds the listener and begins serving. Blocks until the server stops.
// Falls back to the next available port if the preferred port is taken.
func (s *Server) Start() error {
	ln, port, err := listenWithFallback(s.preferredPort)
	if err != nil {
		return fmt.Errorf("server: could not bind: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/timeline", s.handleTimeline)
	mux.HandleFunc("/api/events", s.handleSSE) // Server-Sent Events for watch mode
	mux.HandleFunc("/", s.handleUI)

	s.httpServer = &http.Server{
		Handler:      withLogging(mux),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second, // generous — timeline JSON can be large
		IdleTimeout:  60 * time.Second,
	}

	log.Printf("[server] listening on http://localhost:%d", port)
	log.Printf("[server] open http://localhost:%d in your browser", port)

	if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server: serve: %w", err)
	}
	return nil
}

// Shutdown gracefully stops the server, waiting up to 5 seconds for in-flight
// requests to complete. Safe to call from a signal handler.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpServer == nil {
		return nil
	}
	log.Println("[server] shutting down...")
	return s.httpServer.Shutdown(ctx)
}

// handleTimeline serializes the timeline to JSON.
// Pre-encodes to a buffer so that encoding errors don't result in a partial
// response with HTTP 200 — the client would have no way to detect truncation.
func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	// Only GET is meaningful here
	if r.Method != http.MethodGet && r.Method != http.MethodOptions {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// CORS — needed for VS Code webview and any browser extension that calls
	// the API from a different origin
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	s.mu.RLock()
	timeline := s.timeline
	s.mu.RUnlock()
	data, err := json.Marshal(timeline)
	if err != nil {
		// Encoding error — log with detail, return 500 with safe message
		log.Printf("[server] ERROR: failed to encode timeline: %v", err)
		http.Error(w, "failed to encode timeline", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache") // always fresh — timeline doesn't change
	if _, err := w.Write(data); err != nil {
		// Client disconnected mid-write — not actionable, just log at debug level
		log.Printf("[server] WARN: write interrupted for /api/timeline: %v", err)
	}
}

// handleUI serves the frontend HTML.
// Strategy: try the embedded file first (works when running as compiled binary),
// then fall back to disk (works during `go run .` development).
// No-cache headers ensure the browser never shows a stale loading screen.
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")

	// Try embedded UI first — injected by main.go via SetEmbeddedUI
	if s.embeddedUI != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if _, err := w.Write(s.embeddedUI); err != nil {
			log.Printf("[server] WARN: failed to write embedded UI: %v", err)
		}
		return
	}

	// Fallback: look for file on disk (development mode via `go run .`)
	for _, path := range []string{"ui/index.html", "../ui/index.html"} {
		if _, err := os.Stat(path); err == nil {
			http.ServeFile(w, r, path)
			return
		}
	}

	// Last resort: inline minimal fallback
	log.Printf("[server] WARN: ui/index.html not found — serving fallback")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprint(w, fallbackHTML)
}

// UpdateTimeline replaces the current timeline and notifies all watch-mode
// browser connections to reload. Safe for concurrent use.
func (s *Server) UpdateTimeline(tl *correlator.Timeline) {
	s.mu.Lock()
	s.timeline = tl
	// Notify all connected SSE clients to reload
	for _, ch := range s.sseClients {
		select {
		case ch <- "reload":
		default: // client too slow — skip
		}
	}
	s.mu.Unlock()
}

// handleSSE handles Server-Sent Events connections for watch mode.
// The browser connects once and receives "reload" events whenever the
// timeline is updated. On receiving a reload event, the browser re-fetches
// /api/timeline and re-renders the visualization.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ch := make(chan string, 4)
	s.mu.Lock()
	s.sseClients = append(s.sseClients, ch)
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		for i, c := range s.sseClients {
			if c == ch {
				s.sseClients = append(s.sseClients[:i], s.sseClients[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
	}()

	for {
		select {
		case event := <-ch:
			_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// listenWithFallback tries the preferred port, then the next 9 ports,
// then asks the OS for any free port. Returns the listener and the actual port.
func listenWithFallback(preferred int) (net.Listener, int, error) {
	for p := preferred; p < preferred+10; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
		if err == nil {
			return ln, p, nil
		}
		log.Printf("[server] port %d in use, trying %d", p, p+1)
	}

	// Last resort: OS assigns an ephemeral port
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, 0, fmt.Errorf("could not bind any port in range %d-%d or ephemeral: %w",
			preferred, preferred+9, err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	log.Printf("[server] WARN: using OS-assigned ephemeral port %d", port)
	return ln, port, nil
}

// withLogging wraps a handler to log every request with method, path, and duration.
func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		log.Printf("[server] %s %s → %d (%s)",
			r.Method, r.URL.Path, rw.status, time.Since(start))
	})
}

// responseWriter wraps http.ResponseWriter to capture the status code for logging.
// It also implements http.Flusher so SSE connections work through the logging middleware.
type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}

// Flush implements http.Flusher — required for Server-Sent Events.
// Delegates to the underlying ResponseWriter if it supports flushing.
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

const fallbackHTML = `<!DOCTYPE html>
<html>
<head><title>racevis</title></head>
<body style="background:#000;color:#0f0;font-family:monospace;padding:2rem;line-height:1.6">
<h2 style="color:#e05">racevis</h2>
<p><strong>UI file not found.</strong></p>
<p>Run racevis from the project root directory:</p>
<pre style="background:#111;padding:1rem;border-radius:4px">cd /path/to/racevis
go run .</pre>
<p>Raw timeline JSON: <a href="/api/timeline" style="color:#39f">/api/timeline</a></p>
</body>
</html>`
