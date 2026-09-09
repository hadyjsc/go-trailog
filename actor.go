package trailog

import "context"

// Actor represents the entity (user, service, or API key) that performed a change.
type Actor struct {
	ID    string         `json:"id"`
	Type  string         `json:"type"`  // "user", "system", "api_key"
	Name  string         `json:"name"`
	Email string         `json:"email,omitempty"`
	IP    string         `json:"ip,omitempty"`
	Extra map[string]any `json:"extra,omitempty"`
}

type contextKey string

const (
	actorContextKey         contextKey = "trailog_actor"
	correlationIDContextKey contextKey = "trailog_correlation_id"
)

// WithActor stores an Actor in the context, typically injected by HTTP middleware.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorContextKey, a)
}

// ActorFromContext retrieves the Actor from the context. Returns false if none was set.
func ActorFromContext(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorContextKey).(Actor)
	return a, ok
}

// WithCorrelationID stores a correlation ID in the context. All Record* calls sharing
// the same correlation ID in context will be grouped into one Revision (upsert-by-correlation-id
// fallback behavior for callers not using WithinRevision explicitly).
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationIDContextKey, id)
}

// CorrelationIDFromContext retrieves the correlation ID from the context.
func CorrelationIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(correlationIDContextKey).(string)
	return id, ok
}
