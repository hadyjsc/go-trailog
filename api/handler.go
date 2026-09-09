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

	"github.com/trailog/trailog/revert"
	"github.com/trailog/trailog/timeline"
)

// Handler holds the service dependencies injected at construction time.
type Handler struct {
	timeline *timeline.Service
	reverter *revert.Reverter
}

// NewHandler creates an API Handler wired to the given services.
func NewHandler(tl *timeline.Service, rv *revert.Reverter) *Handler {
	return &Handler{timeline: tl, reverter: rv}
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
type revertRequest struct {
	Strategy string `json:"strategy"` // "block" | "field_level" | "force"
	Reason   string `json:"reason"`
}

// ExecuteRevert applies the revert and returns the new Revision.
func (h *Handler) ExecuteRevert(w http.ResponseWriter, r *http.Request) {
	revID := pathRevertRevisionID(r, "")
	if revID == "" {
		jsonError(w, "missing revision id in path", http.StatusBadRequest)
		return
	}

	var req revertRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
		jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	opts := []revert.RevertOption{}
	switch revert.Strategy(req.Strategy) {
	case revert.StrategyFieldLevel:
		opts = append(opts, revert.WithStrategy(revert.StrategyFieldLevel))
	case revert.StrategyForceOverwrite:
		opts = append(opts, revert.WithStrategy(revert.StrategyForceOverwrite))
	default:
		opts = append(opts, revert.WithStrategy(revert.StrategyBlockOnConflict))
	}
	if req.Reason != "" {
		opts = append(opts, revert.WithReason(req.Reason))
	}

	newRev, err := h.reverter.RevertRevision(r.Context(), revID, opts...)
	if err != nil {
		if isNotFound(err) {
			jsonError(w, err.Error(), http.StatusNotFound)
			return
		}
		// Conflict errors from StrategyBlockOnConflict → 409.
		if strings.Contains(err.Error(), "conflict") {
			jsonError(w, err.Error(), http.StatusConflict)
			return
		}
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, newRev)
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
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
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
