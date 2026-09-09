package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/hadyjsc/go-trailog/integration/httpmw"
	"github.com/hadyjsc/go-trailog/revert"
	"github.com/hadyjsc/go-trailog/timeline"
)

// Server is the HTTP API server for the trailog audit service.
// It owns its net/http.Server and exposes Start / Shutdown lifecycle methods.
type Server struct {
	http    *http.Server
	handler *Handler
}

// ServerConfig holds HTTP server tuning parameters.
type ServerConfig struct {
	Addr         string
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration
}

// DefaultServerConfig returns sensible production defaults.
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		Addr:         ":8080",
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
}

// NewServer constructs a Server with all routes registered.
// actorExtractor may be nil — in that case requests are served without actor injection
// (useful when the embedding app handles auth at a higher level).
func NewServer(
	cfg ServerConfig,
	tl *timeline.Service,
	rv *revert.Reverter,
	actorExtractor httpmw.ActorExtractor,
) *Server {
	h := NewHandler(tl, rv)
	mux := http.NewServeMux()

	// ── Routes ──────────────────────────────────────────────────
	// Health check — no auth needed.
	mux.HandleFunc("/health", HealthCheck)

	// Entity timeline & related queries.
	mux.HandleFunc("/entities/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case hasSuffix(path, "/timeline") && r.Method == http.MethodGet:
			h.GetTimeline(w, r)
		case hasSuffix(path, "/diff") && r.Method == http.MethodGet:
			h.GetDiff(w, r)
		case hasSuffix(path, "/snapshot") && r.Method == http.MethodGet:
			h.GetSnapshot(w, r)
		default:
			http.NotFound(w, r)
		}
	})

	// Revision detail + revert.
	mux.HandleFunc("/revisions/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case hasSuffix(path, "/revert/preview") && r.Method == http.MethodPost:
			h.PreviewRevert(w, r)
		case hasSuffix(path, "/revert") && r.Method == http.MethodPost:
			h.ExecuteRevert(w, r)
		case r.Method == http.MethodGet:
			h.GetRevision(w, r)
		default:
			http.NotFound(w, r)
		}
	})

	// Wrap the mux with middleware chain: logger → actor injection → content-type guard.
	var root http.Handler = mux
	root = jsonContentTypeMiddleware(root)
	if actorExtractor != nil {
		root = httpmw.Middleware(actorExtractor)(root)
	}
	root = requestLoggerMiddleware(root)

	s := &Server{
		handler: h,
		http: &http.Server{
			Addr:         cfg.Addr,
			Handler:      root,
			ReadTimeout:  cfg.ReadTimeout,
			WriteTimeout: cfg.WriteTimeout,
			IdleTimeout:  cfg.IdleTimeout,
		},
	}
	return s
}

// Start begins listening and serving. It blocks until the server exits.
// Use Shutdown for graceful termination.
func (s *Server) Start() error {
	fmt.Printf("trailog API listening on %s\n", s.http.Addr)
	if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("trailog/api: server: %w", err)
	}
	return nil
}

// Shutdown gracefully stops the server with the given context deadline.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

// ────────────────────────────────────────────────────────────────
// middleware
// ────────────────────────────────────────────────────────────────

// requestLoggerMiddleware logs method, path, and response time to stdout.
func requestLoggerMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		fmt.Printf("%s %s %d %s\n", r.Method, r.URL.Path, rw.status, time.Since(start))
	})
}

// jsonContentTypeMiddleware sets Accept: application/json on all API routes.
func jsonContentTypeMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only enforce JSON on non-health paths.
		if r.URL.Path != "/health" {
			accept := r.Header.Get("Accept")
			if accept != "" && accept != "*/*" && accept != "application/json" {
				jsonError(w, "this API only serves application/json", http.StatusNotAcceptable)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

// hasSuffix checks if path ends with suffix after stripping query strings.
func hasSuffix(path, suffix string) bool {
	// Path already has query string stripped by net/http before reaching the handler.
	return len(path) >= len(suffix) && path[len(path)-len(suffix):] == suffix
}
