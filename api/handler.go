// Package api exposes trailog's timeline and revert engine as a JSON HTTP service.
// All seven endpoints from TRD §12 are implemented here.
//
// Routes (registered by Server.routes()):
//
//	GET  /entities/{type}/{id}/timeline
//	GET  /revisions/{id}
//	GET  /entities/{type}/{id}/diff?from=<revA>&to=<revB>
//	GET  /entities/{type}/{id}/snapshot?at=<revId>
//	POST /revisions/{id}/revert/preview
//	POST /revisions/{id}/revert
//
// All responses are JSON. Errors use {"error":"<message>"} with an appropriate
// HTTP status code.
package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hadyjsc/go-trailog/revert"
	"github.com/hadyjsc/go-trailog/store"
	"github.com/hadyjsc/go-trailog/timeline"
)

// Handler holds the service dependencies injected at construction time.
type Handler struct {
	timeline  *timeline.Service
	reverter  *revert.Reverter
	store     store.Store  // for webhook target config CRUD
	applyMode revert.ApplyMode
}

// HandlerOption configures a Handler.
type HandlerOption func(*Handler)

// WithAuditOnlyRevert sets the handler to audit-only revert mode: the audit
// revision is persisted but no writes are made to the application tables.
// Use this when the API server has no registered EntityRepository or RevertApplier.
func WithAuditOnlyRevert() HandlerOption {
	return func(h *Handler) { h.applyMode = revert.ApplyModeSkip }
}

// NewHandler creates an API Handler wired to the given services.
func NewHandler(tl *timeline.Service, rv *revert.Reverter, st store.Store, opts ...HandlerOption) *Handler {
	h := &Handler{timeline: tl, reverter: rv, store: st}
	for _, o := range opts {
		o(h)
	}
	return h
}

// ────────────────────────────────────────────────────────────────
// GET /entities/{type}/{id}/timeline
// ────────────────────────────────────────────────────────────────

// GetTimeline returns the paginated revision list for an entity.
//
// Query params:
//
//	include_related=true       — widen to related entities
//	depth=<int>                — relation hops (default 1)
//	actor_id=<id>              — filter by actor
//	from=<RFC3339>             — filter start time
//	to=<RFC3339>               — filter end time
//	action=<a1>,<a2>           — comma-separated action types
//	limit=<int>                — page size (default 20, max 200)
//	cursor=<revisionId>        — pagination cursor
func (h *Handler) GetTimeline(w http.ResponseWriter, r *http.Request) {
	entityType, entityID := pathEntityTypeID(r)
	if entityType == "" || entityID == "" {
		jsonError(w, "missing entity type or id in path", http.StatusBadRequest)
		return
	}

	q := timeline.TimelineQuery{
		Entity:         timeline.Entity{Type: entityType, ID: entityID},
		IncludeRelated: r.URL.Query().Get("include_related") == "true",
		ActorID:        r.URL.Query().Get("actor_id"),
		Cursor:         r.URL.Query().Get("cursor"),
	}

	if d := r.URL.Query().Get("depth"); d != "" {
		if n, err := strconv.Atoi(d); err == nil {
			q.RelationDepth = n
		}
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			q.Limit = n
		}
	}
	if f := r.URL.Query().Get("from"); f != "" {
		if t, err := time.Parse(time.RFC3339, f); err == nil {
			q.From = t
		}
	}
	if t := r.URL.Query().Get("to"); t != "" {
		if ts, err := time.Parse(time.RFC3339, t); err == nil {
			q.To = ts
		}
	}
	if a := r.URL.Query().Get("action"); a != "" {
		q.Actions = strings.Split(a, ",")
	}

	result, err := h.timeline.GetTimeline(r.Context(), q)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, result)
}

// ────────────────────────────────────────────────────────────────
// GET /revisions/{id}
// ────────────────────────────────────────────────────────────────

// GetRevision returns the full detail of one revision.
func (h *Handler) GetRevision(w http.ResponseWriter, r *http.Request) {
	revID := pathSegment(r, "/revisions/")
	if revID == "" {
		jsonError(w, "missing revision id in path", http.StatusBadRequest)
		return
	}
	rev, err := h.timeline.GetRevision(r.Context(), revID)
	if err != nil {
		if isNotFound(err) {
			jsonError(w, err.Error(), http.StatusNotFound)
			return
		}
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, rev)
}

// ────────────────────────────────────────────────────────────────
// GET /entities/{type}/{id}/diff?from={revA}&to={revB}
// ────────────────────────────────────────────────────────────────

// GetDiff returns field-level diffs for an entity between two revisions.
func (h *Handler) GetDiff(w http.ResponseWriter, r *http.Request) {
	entityType, entityID := pathEntityTypeID(r)
	if entityType == "" || entityID == "" {
		jsonError(w, "missing entity type or id", http.StatusBadRequest)
		return
	}
	fromRev := r.URL.Query().Get("from")
	toRev := r.URL.Query().Get("to")
	if fromRev == "" || toRev == "" {
		jsonError(w, "query params 'from' and 'to' are required", http.StatusBadRequest)
		return
	}

	diffs, err := h.timeline.Diff(r.Context(),
		timeline.Entity{Type: entityType, ID: entityID},
		fromRev, toRev,
	)
	if err != nil {
		if isNotFound(err) {
			jsonError(w, err.Error(), http.StatusNotFound)
			return
		}
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, diffs)
}

// ────────────────────────────────────────────────────────────────
// GET /entities/{type}/{id}/snapshot?at={revId}
// ────────────────────────────────────────────────────────────────

// GetSnapshot reconstructs entity state as of a given revision.
func (h *Handler) GetSnapshot(w http.ResponseWriter, r *http.Request) {
	entityType, entityID := pathEntityTypeID(r)
	if entityType == "" || entityID == "" {
		jsonError(w, "missing entity type or id", http.StatusBadRequest)
		return
	}
	atRev := r.URL.Query().Get("at")
	if atRev == "" {
		jsonError(w, "query param 'at' (revision id) is required", http.StatusBadRequest)
		return
	}

	snap, err := h.timeline.Snapshot(r.Context(),
		timeline.Entity{Type: entityType, ID: entityID},
		atRev,
	)
	if err != nil {
		if isNotFound(err) {
			jsonError(w, err.Error(), http.StatusNotFound)
			return
		}
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, snap)
}

// ────────────────────────────────────────────────────────────────
// POST /revisions/{id}/revert/preview
// ────────────────────────────────────────────────────────────────

// PreviewRevert is a dry-run revert that returns a RevertPlan without writing anything.
func (h *Handler) PreviewRevert(w http.ResponseWriter, r *http.Request) {
	revID := pathRevertRevisionID(r, "/preview")
	if revID == "" {
		jsonError(w, "missing revision id in path", http.StatusBadRequest)
		return
	}

	plan, err := h.reverter.PreviewRevert(r.Context(), revID)
	if err != nil {
		if isNotFound(err) {
			jsonError(w, err.Error(), http.StatusNotFound)
			return
		}
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, plan)
}

// ────────────────────────────────────────────────────────────────
// POST /revisions/{id}/revert
// ────────────────────────────────────────────────────────────────

// revertRequest is the JSON body accepted by ExecuteRevert.
// All fields are validated before calling the reverter.
type revertRequest struct {
	// Strategy controls conflict resolution: "block" | "field_level" | "force".
	// Defaults to "block" if omitted.
	Strategy string `json:"strategy"`
	// Reason is a mandatory human-readable description of why this revert is
	// being executed. Stored on the new revert revision.
	Reason string `json:"reason"`
	// Target overrides the stored per-entity-type webhook config for this call.
	// When provided: url is required; method defaults to POST; auth is optional.
	// When omitted: the reverter uses the stored config for each entity type.
	// If neither a per-request target nor a stored config exists for an entity
	// type, the revert is rejected with 422.
	Target *revert.WebhookTarget `json:"target,omitempty"`
}

// validMethods is the set of HTTP methods accepted for webhook targets.
var validMethods = map[string]struct{}{
	"POST":  {},
	"PUT":   {},
	"PATCH": {},
}

// ExecuteRevert validates the request body, resolves the webhook target,
// and applies the revert. Returns the new audit Revision on success.
func (h *Handler) ExecuteRevert(w http.ResponseWriter, r *http.Request) {
	revID := pathRevertRevisionID(r, "")
	if revID == "" {
		jsonError(w, "missing revision id in path", http.StatusBadRequest)
		return
	}

	var req revertRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if err.Error() == "EOF" {
			jsonError(w, "request body is required", http.StatusBadRequest)
			return
		}
		jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// ── Validate required fields ────────────────────────────────────────────
	if strings.TrimSpace(req.Reason) == "" {
		jsonError(w, "reason is required", http.StatusBadRequest)
		return
	}

	if req.Target != nil {
		if strings.TrimSpace(req.Target.URL) == "" {
			jsonError(w, "target.url is required when target is provided", http.StatusBadRequest)
			return
		}
		method := strings.ToUpper(strings.TrimSpace(req.Target.Method))
		if method == "" {
			req.Target.Method = "POST"
		} else {
			if _, ok := validMethods[method]; !ok {
				jsonError(w, "target.method must be POST, PUT, or PATCH", http.StatusBadRequest)
				return
			}
			req.Target.Method = method
		}
	}

	// ── Build revert options ────────────────────────────────────────────────
	opts := []revert.RevertOption{}

	if req.Target != nil {
		// Explicit per-request target — always use webhook mode.
		opts = append(opts, revert.WithWebhookApplier(*req.Target))
	} else if h.applyMode == revert.ApplyModeSkip {
		// Standalone server with no per-request target.
		// RevertRevision will look up stored config per entity type and error if
		// none is found, so we pass through without forcing skip.
		// (ApplyModeWrite is the default; stored config resolution happens inside
		// the reverter when neither a per-request target nor a registered applier
		// is present.)
	}

	switch revert.Strategy(req.Strategy) {
	case revert.StrategyFieldLevel:
		opts = append(opts, revert.WithStrategy(revert.StrategyFieldLevel))
	case revert.StrategyForceOverwrite:
		opts = append(opts, revert.WithStrategy(revert.StrategyForceOverwrite))
	default:
		opts = append(opts, revert.WithStrategy(revert.StrategyBlockOnConflict))
	}
	opts = append(opts, revert.WithReason(req.Reason))

	newRev, err := h.reverter.RevertRevision(r.Context(), revID, opts...)
	if err != nil {
		if isNotFound(err) {
			jsonError(w, err.Error(), http.StatusNotFound)
			return
		}
		if strings.Contains(err.Error(), "conflict") {
			jsonError(w, err.Error(), http.StatusConflict)
			return
		}
		if strings.Contains(err.Error(), "no repository or applier") ||
			strings.Contains(err.Error(), "no webhook target") {
			jsonError(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, newRev)
}

// ────────────────────────────────────────────────────────────────
// GET /entities
// ────────────────────────────────────────────────────────────────

// GetEntities returns a paginated, sortable, filterable list of all distinct
// (entity_type, entity_id) pairs that have at least one audit record.
//
// Query params:
//
//	entity_type=<type>         — filter to one entity type
//	actor_id=<id>              — filter by last actor
//	op=<create|update|delete>  — filter by last operation
//	from=<RFC3339>             — last_changed_at >= from
//	to=<RFC3339>               — last_changed_at <= to
//	sort=<field>               — "last_changed_at" (default) | "entity_type" | "entity_id"
//	dir=<asc|desc>             — sort direction (default "desc")
//	limit=<int>                — page size (default 20, max 200)
//	cursor=<opaque>            — pagination cursor from previous response
func (h *Handler) GetEntities(w http.ResponseWriter, r *http.Request) {
	q := timeline.EntityQuery{
		EntityType: r.URL.Query().Get("entity_type"),
		ActorID:    r.URL.Query().Get("actor_id"),
		Op:         r.URL.Query().Get("op"),
		SortField:  r.URL.Query().Get("sort"),
		SortDir:    r.URL.Query().Get("dir"),
		Cursor:     r.URL.Query().Get("cursor"),
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			q.Limit = n
		}
	}
	if f := r.URL.Query().Get("from"); f != "" {
		if t, err := time.Parse(time.RFC3339, f); err == nil {
			q.From = t
		}
	}
	if t := r.URL.Query().Get("to"); t != "" {
		if ts, err := time.Parse(time.RFC3339, t); err == nil {
			q.To = ts
		}
	}

	result, err := h.timeline.ListEntities(r.Context(), q)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, result)
}

// ────────────────────────────────────────────────────────────────
// GET /webhook-targets/{entity_type}
// PUT /webhook-targets/{entity_type}
// DELETE /webhook-targets/{entity_type}
// ────────────────────────────────────────────────────────────────

// webhookTargetRequest is the JSON body for PUT /webhook-targets/{entity_type}.
type webhookTargetRequest struct {
	URL         string `json:"url"`
	Method      string `json:"method"`
	Auth        string `json:"auth"`
	TimeoutSecs int    `json:"timeout_secs"`
}

// GetWebhookTarget returns the stored webhook target config for an entity type.
func (h *Handler) GetWebhookTarget(w http.ResponseWriter, r *http.Request) {
	entityType := pathSegment(r, "/webhook-targets/")
	if entityType == "" {
		jsonError(w, "missing entity_type in path", http.StatusBadRequest)
		return
	}

	cfg, err := h.store.GetWebhookTargetConfig(r.Context(), entityType)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if cfg == nil {
		jsonError(w, "no webhook target configured for entity type "+entityType, http.StatusNotFound)
		return
	}
	// Mask the auth value in the response — return only whether it is set.
	out := map[string]any{
		"id":           cfg.ID,
		"entity_type":  cfg.EntityType,
		"url":          cfg.URL,
		"method":       cfg.Method,
		"auth_set":     cfg.Auth != "",
		"timeout_secs": cfg.TimeoutSecs,
		"created_at":   cfg.CreatedAt,
		"updated_at":   cfg.UpdatedAt,
	}
	jsonOK(w, out)
}

// UpsertWebhookTarget creates or updates the webhook target config for an entity type.
func (h *Handler) UpsertWebhookTarget(w http.ResponseWriter, r *http.Request) {
	entityType := pathSegment(r, "/webhook-targets/")
	if entityType == "" {
		jsonError(w, "missing entity_type in path", http.StatusBadRequest)
		return
	}

	var req webhookTargetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if err.Error() == "EOF" {
			jsonError(w, "request body is required", http.StatusBadRequest)
			return
		}
		jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Validate.
	if strings.TrimSpace(req.URL) == "" {
		jsonError(w, "url is required", http.StatusBadRequest)
		return
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = "POST"
	}
	if _, ok := validMethods[method]; !ok {
		jsonError(w, "method must be POST, PUT, or PATCH", http.StatusBadRequest)
		return
	}
	timeout := req.TimeoutSecs
	if timeout <= 0 {
		timeout = 30
	}

	cfg := store.WebhookTargetConfig{
		ID:          uuid.New().String(),
		EntityType:  entityType,
		URL:         strings.TrimSpace(req.URL),
		Method:      method,
		Auth:        req.Auth,
		TimeoutSecs: timeout,
	}
	if err := h.store.SaveWebhookTargetConfig(r.Context(), cfg); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Return the saved config (masking auth).
	out := map[string]any{
		"entity_type":  cfg.EntityType,
		"url":          cfg.URL,
		"method":       cfg.Method,
		"auth_set":     cfg.Auth != "",
		"timeout_secs": cfg.TimeoutSecs,
	}
	jsonOK(w, out)
}

// DeleteWebhookTarget removes the webhook target config for an entity type.
func (h *Handler) DeleteWebhookTarget(w http.ResponseWriter, r *http.Request) {
	entityType := pathSegment(r, "/webhook-targets/")
	if entityType == "" {
		jsonError(w, "missing entity_type in path", http.StatusBadRequest)
		return
	}

	if err := h.store.DeleteWebhookTargetConfig(r.Context(), entityType); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"entity_type": entityType, "deleted": true})
}

// ────────────────────────────────────────────────────────────────
// health check
// ────────────────────────────────────────────────────────────────

// HealthCheck returns 200 OK with a simple JSON body.
func HealthCheck(w http.ResponseWriter, _ *http.Request) {
	jsonOK(w, map[string]string{"status": "ok"})
}

// ────────────────────────────────────────────────────────────────
// helpers
// ────────────────────────────────────────────────────────────────

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"message": "ok",
		"data":    v,
	})
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": false,
		"message": msg,
		"data":    nil,
	})
}

func isNotFound(err error) bool {
	return strings.Contains(err.Error(), "not found")
}

// pathEntityTypeID extracts {type} and {id} from paths like
// /entities/invoice/inv-1/timeline or /entities/invoice/inv-1/diff
// Path structure: /entities/<type>/<id>/...
func pathEntityTypeID(r *http.Request) (entityType, entityID string) {
	// Strip leading slash and split.
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 5)
	// parts[0]="entities", parts[1]=type, parts[2]=id, parts[3]=action
	if len(parts) < 3 {
		return "", ""
	}
	return parts[1], parts[2]
}

// pathSegment extracts the segment after the given prefix.
// e.g. pathSegment(r, "/revisions/") on /revisions/abc-123 → "abc-123"
func pathSegment(r *http.Request, prefix string) string {
	path := strings.TrimPrefix(r.URL.Path, prefix)
	// Remove any trailing path components.
	if idx := strings.Index(path, "/"); idx >= 0 {
		path = path[:idx]
	}
	return path
}

// pathRevertRevisionID extracts the revision ID from paths like
// /revisions/{id}/revert  or  /revisions/{id}/revert/preview
func pathRevertRevisionID(r *http.Request, stripSuffix string) string {
	path := strings.TrimPrefix(r.URL.Path, "/revisions/")
	path = strings.TrimSuffix(path, "/revert"+stripSuffix)
	return strings.TrimSuffix(path, "/revert")
}
