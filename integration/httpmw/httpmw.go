// Package httpmw provides net/http middleware for injecting trailog Actor and
// CorrelationID into request contexts automatically.
package httpmw

import (
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/trailog/trailog"
)

// ActorExtractor is a function that extracts an Actor from an HTTP request.
// Return the zero Actor and false if extraction fails.
type ActorExtractor func(r *http.Request) (trailog.Actor, bool)

// Middleware returns an http.Handler middleware that injects an Actor and a
// fresh CorrelationID into every request context.
//
// Example:
//
//	router.Use(httpmw.Middleware(httpmw.ActorFromJWT))
func Middleware(extract ActorExtractor) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()

			// Inject actor.
			if extract != nil {
				if actor, ok := extract(r); ok {
					ctx = trailog.WithActor(ctx, actor)
				}
			}

			// Inject a fresh correlation ID so all Record* calls within this
			// request share the same revision (fallback path without WithinRevision).
			corrID := r.Header.Get("X-Correlation-ID")
			if corrID == "" {
				corrID = r.Header.Get("X-Request-ID")
			}
			if corrID == "" {
				corrID = uuid.New().String()
			}
			ctx = trailog.WithCorrelationID(ctx, corrID)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ────────────────────────────────────────────────────────────────
// Built-in extractor helpers
// ────────────────────────────────────────────────────────────────

// ActorFromJWT is a lightweight extractor that reads actor fields from a
// Bearer JWT payload. It does NOT verify the signature — the app's own auth
// middleware must validate the token first; this extractor only reads claims.
//
// Expected JWT payload fields (all optional):
//
//	sub  → Actor.ID
//	name → Actor.Name
//	email → Actor.Email
func ActorFromJWT(r *http.Request) (trailog.Actor, bool) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return trailog.Actor{}, false
	}
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return trailog.Actor{}, false
	}
	token := parts[1]
	claims, err := unsafeParseJWTClaims(token)
	if err != nil {
		return trailog.Actor{}, false
	}

	actor := trailog.Actor{
		Type: "user",
		IP:   realIP(r),
	}
	if sub, ok := claims["sub"].(string); ok {
		actor.ID = sub
	}
	if name, ok := claims["name"].(string); ok {
		actor.Name = name
	}
	if email, ok := claims["email"].(string); ok {
		actor.Email = email
	}
	if actorType, ok := claims["actor_type"].(string); ok {
		actor.Type = actorType
	}
	return actor, actor.ID != ""
}

// ActorFromHeader builds an actor from explicit HTTP headers, useful for
// service-to-service calls where the caller sets actor info directly.
//
// Expected headers:
//
//	X-Actor-ID    → Actor.ID
//	X-Actor-Type  → Actor.Type  (default "system")
//	X-Actor-Name  → Actor.Name
func ActorFromHeader(r *http.Request) (trailog.Actor, bool) {
	id := r.Header.Get("X-Actor-ID")
	if id == "" {
		return trailog.Actor{}, false
	}
	actorType := r.Header.Get("X-Actor-Type")
	if actorType == "" {
		actorType = "system"
	}
	return trailog.Actor{
		ID:   id,
		Type: actorType,
		Name: r.Header.Get("X-Actor-Name"),
		IP:   realIP(r),
	}, true
}

// ────────────────────────────────────────────────────────────────
// helpers
// ────────────────────────────────────────────────────────────────

// realIP extracts the best-guess client IP from standard proxy headers.
func realIP(r *http.Request) string {
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		return strings.SplitN(fwd, ",", 2)[0]
	}
	return r.RemoteAddr
}
