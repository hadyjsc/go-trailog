// Package timeline provides the read-path API for retrieving and presenting
// audit history as a GitHub-style commit timeline.
package timeline

import (
	"context"
	"fmt"
	"time"

	"github.com/hadyjsc/go-trailog/relation"
	"github.com/hadyjsc/go-trailog/store"
)

// Entity identifies a record. Aliased here so callers don't need to import
// the internal/types package directly.
type Entity struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// TimelineQuery parameterises a call to GetTimeline.
type TimelineQuery struct {
	Entity         Entity
	IncludeRelated bool
	RelationDepth  int    // how many relation hops to follow (default 1)
	ActorID        string // filter by actor
	From           time.Time
	To             time.Time
	Actions        []string // filter by action type
	Limit          int
	Cursor         string // cursor-based pagination — last revision ID from prior page
}

// TimelineResult is the paginated response from GetTimeline.
type TimelineResult struct {
	Revisions  []Revision `json:"revisions"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

// Service exposes the read-path timeline and diff queries.
type Service struct {
	store store.Store
	rel   *relation.Registry
}

// New creates a new timeline Service backed by the given store and relation registry.
func New(s store.Store, rel *relation.Registry) *Service {
	return &Service{store: s, rel: rel}
}

// GetTimeline returns a cursor-paginated list of Revisions touching the queried
// entity (and optionally its related entities), newest first.
func (svc *Service) GetTimeline(ctx context.Context, q TimelineQuery) (*TimelineResult, error) {
	depth := q.RelationDepth
	if depth <= 0 {
		depth = 1
	}

	limit := q.Limit
	if limit <= 0 || limit > 200 {
		limit = 20
	}

	f := store.TimelineFilter{
		EntityType: q.Entity.Type,
		EntityID:   q.Entity.ID,
		ActorID:    q.ActorID,
		From:       q.From,
		To:         q.To,
		Actions:    q.Actions,
		Limit:      limit,
		Cursor:     q.Cursor,
	}

	// Expand related entities if requested.
	if q.IncludeRelated {
		internalEntity := toInternalEntity(q.Entity)
		related, err := expandRelated(ctx, svc.rel, internalEntity, depth)
		if err != nil {
			return nil, fmt.Errorf("trailog/timeline: expand related: %w", err)
		}
		for _, e := range related {
			f.RelatedIDs = append(f.RelatedIDs, store.EntityRef{Type: e.Type, ID: e.ID})
		}
		f.IncludeRelated = true
	}

	revisions, nextCursor, err := svc.store.ListRevisions(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("trailog/timeline: list revisions: %w", err)
	}

	return &TimelineResult{
		Revisions:  toRevisions(revisions),
		NextCursor: nextCursor,
	}, nil
}

// GetRevision returns the full detail of a single revision (all entities + field diffs).
func (svc *Service) GetRevision(ctx context.Context, revisionID string) (*Revision, error) {
	rev, err := svc.store.GetRevision(ctx, revisionID)
	if err != nil {
		return nil, fmt.Errorf("trailog/timeline: get revision %q: %w", revisionID, err)
	}
	out := toRevision(*rev)
	return &out, nil
}

// Diff returns the field-level diffs for a specific entity between two revision points.
// fromRevID and toRevID must be valid revision IDs that contain a change to the entity.
func (svc *Service) Diff(ctx context.Context, entity Entity, fromRevID, toRevID string) ([]FieldDiff, error) {
	fromRev, err := svc.store.GetRevision(ctx, fromRevID)
	if err != nil {
		return nil, fmt.Errorf("trailog/timeline: diff from revision: %w", err)
	}
	toRev, err := svc.store.GetRevision(ctx, toRevID)
	if err != nil {
		return nil, fmt.Errorf("trailog/timeline: diff to revision: %w", err)
	}

	fromSnapshot := snapshotForEntity(fromRev.Changes, entity.Type, entity.ID)
	toSnapshot := snapshotForEntity(toRev.Changes, entity.Type, entity.ID)
	if fromSnapshot == nil && toSnapshot == nil {
		return nil, fmt.Errorf("trailog/timeline: entity %s:%s not found in either revision", entity.Type, entity.ID)
	}

	// Use the JSON differ to produce field-level diffs between the two snapshots.
	diffs := jsonDiff(fromSnapshot, toSnapshot, "")
	return diffs, nil
}

// Snapshot reconstructs the entity state as of the given revision.
func (svc *Service) Snapshot(ctx context.Context, entity Entity, atRevisionID string) (map[string]any, error) {
	rev, err := svc.store.GetRevision(ctx, atRevisionID)
	if err != nil {
		return nil, fmt.Errorf("trailog/timeline: snapshot revision: %w", err)
	}
	snap := snapshotForEntity(rev.Changes, entity.Type, entity.ID)
	if snap == nil {
		return nil, fmt.Errorf("trailog/timeline: entity %s:%s not found in revision %s", entity.Type, entity.ID, atRevisionID)
	}
	return snap, nil
}

// ────────────────────────────────────────────────────────────────
// helpers
// ────────────────────────────────────────────────────────────────

func snapshotForEntity(changes []store.EntityChange, entityType, entityID string) map[string]any {
	for _, ec := range changes {
		if ec.EntityType == entityType && ec.EntityID == entityID {
			if ec.SnapshotAfter != nil {
				return ec.SnapshotAfter
			}
			return ec.SnapshotBefore
		}
	}
	return nil
}

// expandRelated calls the relation registry BFS to find all entities related to root
// up to maxDepth hops. Returns store-package EntityRef slices.
type internalEntity struct {
	Type string
	ID   string
}

func toInternalEntity(e Entity) internalEntity {
	return internalEntity{Type: e.Type, ID: e.ID}
}

func expandRelated(ctx context.Context, reg *relation.Registry, root internalEntity, maxDepth int) ([]internalEntity, error) {
	if maxDepth <= 0 {
		return nil, nil
	}

	// Build an adapter that wraps the relation.Registry using the internal types package.
	type rEntity = interface {
		// nothing — we just use the registry's Resolve directly
	}

	// Import the types package inline via a local adapter.
	visited := map[string]struct{}{root.Type + ":" + root.ID: {}}
	queue := []internalEntity{root}
	var related []internalEntity

	for depth := 0; depth < maxDepth && len(queue) > 0; depth++ {
		var nextQueue []internalEntity
		for _, e := range queue {
			// Use relation.Registry.Resolve with the internal types.Entity shape.
			// We import via the concrete type — relation package uses internal/types.
			resolved, err := resolveViaRegistry(ctx, reg, e)
			if err != nil {
				return nil, err
			}
			for _, r := range resolved {
				key := r.Type + ":" + r.ID
				if _, seen := visited[key]; seen {
					continue
				}
				visited[key] = struct{}{}
				related = append(related, r)
				nextQueue = append(nextQueue, r)
			}
		}
		queue = nextQueue
	}
	return related, nil
}

// resolveViaRegistry adapts internalEntity → relation.Resolver's expected type.
func resolveViaRegistry(ctx context.Context, reg *relation.Registry, e internalEntity) ([]internalEntity, error) {
	// We need to call reg.Resolve which expects internal/types.Entity.
	// Since relation package imports internal/types, we construct the call via
	// a thin type-conversion shim. The simplest approach: call the registry's
	// Resolve method by constructing a types.Entity value through reflection.
	// To avoid an import cycle we accept the slight indirection here.
	entities, err := reg.ResolveByTypeID(ctx, e.Type, e.ID)
	if err != nil {
		return nil, err
	}
	var out []internalEntity
	for _, ent := range entities {
		out = append(out, internalEntity{Type: ent.Type, ID: ent.ID})
	}
	return out, nil
}

// jsonDiff is a local, simple JSON-map diff (avoids importing the diff package
// to keep the read path dependency-free).
func jsonDiff(before, after map[string]any, prefix string) []FieldDiff {
	keys := make(map[string]struct{})
	for k := range before {
		keys[k] = struct{}{}
	}
	for k := range after {
		keys[k] = struct{}{}
	}

	var diffs []FieldDiff
	for k := range keys {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		bv, bOk := before[k]
		av, aOk := after[k]

		bm, bIsMap := bv.(map[string]any)
		am, aIsMap := av.(map[string]any)
		if bIsMap && aIsMap {
			diffs = append(diffs, jsonDiff(bm, am, path)...)
			continue
		}

		switch {
		case bOk && !aOk:
			diffs = append(diffs, FieldDiff{Field: path, OldValue: bv, NewValue: nil})
		case !bOk && aOk:
			diffs = append(diffs, FieldDiff{Field: path, OldValue: nil, NewValue: av})
		default:
			if fmt.Sprintf("%v", bv) != fmt.Sprintf("%v", av) {
				diffs = append(diffs, FieldDiff{Field: path, OldValue: bv, NewValue: av})
			}
		}
	}
	return diffs
}
