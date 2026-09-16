package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hadyjsc/go-trailog/integration/httpmw"
	"github.com/hadyjsc/go-trailog/revert"
	"github.com/hadyjsc/go-trailog/store"
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
	// CORSOrigins is a comma-separated list of origins allowed by the CORS middleware.
	// Special values:
	//   ""  — CORS headers are not set (disables CORS handling entirely).
	//   "*" — All origins are allowed (default; suitable for development).
	// Example: "https://app.example.com,https://admin.example.com"
	CORSOrigins string
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
// handlerOpts are applied to the Handler — use WithAuditOnlyRevert() when no
// EntityRepository or RevertApplier is registered for the entity types.
func NewServer(
	cfg ServerConfig,
	tl *timeline.Service,
	rv *revert.Reverter,
	st store.Store,
	actorExtractor httpmw.ActorExtractor,
	handlerOpts ...HandlerOption,
) *Server {
	h := NewHandler(tl, rv, st, handlerOpts...)
	mux := http.NewServeMux()

	// ── Routes ──────────────────────────────────────────────────
	// Health check — no auth needed.
	mux.HandleFunc("/health", HealthCheck)

	// Entity list (no type/id) — GET /entities
	mux.HandleFunc("/entities", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		h.GetEntities(w, r)
	})

	// Entity timeline & related queries — /entities/{type}/{id}/...
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

	// Webhook target config CRUD — /webhook-targets/{entity_type}
	mux.HandleFunc("/webhook-targets/", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			h.GetWebhookTarget(w, r)
		case http.MethodPut:
			h.UpsertWebhookTarget(w, r)
		case http.MethodDelete:
			h.DeleteWebhookTarget(w, r)
		default:
			http.NotFound(w, r)
		}
	})

	// Wrap the mux with middleware chain (outermost first):
	// CORS → logger → actor injection → content-type guard → mux
	var root http.Handler = mux
	root = jsonContentTypeMiddleware(root)
	if actorExtractor != nil {
		root = httpmw.Middleware(actorExtractor)(root)
	}
	root = requestLoggerMiddleware(root)
	if cfg.CORSOrigins != "" {
		root = corsMiddleware(cfg.CORSOrigins)(root)
	}

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

// corsMiddleware adds CORS response headers and handles preflight OPTIONS requests.
//
// allowedOrigins is a comma-separated list of allowed origin values.
// Pass "*" to allow any origin. The middleware matches the request Origin header
// against the list; on a match it echoes the origin back (for credentialed requests)
// or reflects "*" for wildcard mode. On no match, no CORS headers are set.
//
// Preflight (OPTIONS) requests are answered immediately with 204 No Content so
// they do not reach downstream handlers or the auth middleware.
func corsMiddleware(allowedOrigins string) func(http.Handler) http.Handler {
	// Parse once at construction time.
	wildcard := false
	allowed := map[string]struct{}{}
	for _, o := range strings.Split(allowedOrigins, ",") {
		o = strings.TrimSpace(o)
		if o == "*" {
			wildcard = true
		} else if o != "" {
			allowed[o] = struct{}{}
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			// No Origin header — not a CORS request; skip header injection.
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}

			// Determine whether to allow this origin.
			allow := false
			if wildcard {
				allow = true
			} else if _, ok := allowed[origin]; ok {
				allow = true
			}

			if allow {
				if wildcard {
					w.Header().Set("Access-Control-Allow-Origin", "*")
				} else {
					// Echo the specific origin so credentialed requests work.
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Set("Vary", "Origin")
				}
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Accept, X-Correlation-ID")
				w.Header().Set("Access-Control-Max-Age", "86400") // 24 h preflight cache
			}

			// Preflight — answer immediately; no need to hit downstream.
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// hasSuffix checks if path ends with suffix after stripping query strings.
func hasSuffix(path, suffix string) bool {
	// Path already has query string stripped by net/http before reaching the handler.
	return len(path) >= len(suffix) && path[len(path)-len(suffix):] == suffix
}
