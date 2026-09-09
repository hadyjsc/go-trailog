// Package memory provides an in-memory Store implementation for use in unit tests.
// It is NOT safe for production use (no persistence, no partitioning).
package memory

import (
	"context"
	"fmt"
	"sync"

	"github.com/hadyjsc/go-trailog/store"
)

// Store is an in-memory implementation of store.Store, suitable for tests.
type Store struct {
	mu         sync.RWMutex
	revisions  map[string]*store.Revision      // id → revision (without changes)
	changes    map[string][]store.EntityChange // revision_id → changes
	revertLogs map[string]*store.RevertLog     // id → revert log
	// Index: (entityType+":"+entityID) → []revisionID (insertion order)
	entityIndex map[string][]string
}

// New returns an empty in-memory Store.
func New() *Store {
	return &Store{
		revisions:   make(map[string]*store.Revision),
		changes:     make(map[string][]store.EntityChange),
		revertLogs:  make(map[string]*store.RevertLog),
		entityIndex: make(map[string][]string),
	}
}

// Ping is a no-op for the in-memory store.
func (s *Store) Ping(_ context.Context) error { return nil }

// Close is a no-op for the in-memory store.
func (s *Store) Close() error { return nil }

// SaveRevision stores the revision and indexes its entity changes.
func (s *Store) SaveRevision(_ context.Context, rev store.Revision) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Store a copy of the revision without the embedded changes slice.
	r := rev
	r.Changes = nil
	s.revisions[rev.ID] = &r
	s.changes[rev.ID] = rev.Changes

	// Build entity index.
	for _, ec := range rev.Changes {
		key := ec.EntityType + ":" + ec.EntityID
		s.entityIndex[key] = append(s.entityIndex[key], rev.ID)
	}
	return nil
}

// GetRevision returns the full revision with entity changes and field diffs.
func (s *Store) GetRevision(_ context.Context, id string) (*store.Revision, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rev, ok := s.revisions[id]
	if !ok {
		return nil, fmt.Errorf("trailog/store/memory: revision %q not found", id)
	}
	out := *rev
	out.Changes = s.changes[id]
	return &out, nil
}

// ListRevisions returns a cursor-paginated, newest-first list of revisions.
func (s *Store) ListRevisions(_ context.Context, f store.TimelineFilter) ([]store.Revision, string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 20
	}

	// Collect revision IDs that touch the primary entity (or any related entity).
	revIDSet := make(map[string]struct{})
	primaryKey := f.EntityType + ":" + f.EntityID
	for _, rid := range s.entityIndex[primaryKey] {
		revIDSet[rid] = struct{}{}
	}
	for _, rel := range f.RelatedIDs {
		key := rel.Type + ":" + rel.ID
		for _, rid := range s.entityIndex[key] {
			revIDSet[rid] = struct{}{}
		}
	}

	// Collect and sort by occurred_at DESC.
	var matched []*store.Revision
	for rid := range revIDSet {
		rev := s.revisions[rid]
		if rev == nil {
			continue
		}
		if f.ActorID != "" && rev.ActorID != f.ActorID {
			continue
		}
		if !f.From.IsZero() && rev.OccurredAt.Before(f.From) {
			continue
		}
		if !f.To.IsZero() && rev.OccurredAt.After(f.To) {
			continue
		}
		if len(f.Actions) > 0 && !contains(f.Actions, rev.Action) {
			continue
		}
		matched = append(matched, rev)
	}

	// Sort newest first.
	sortRevisions(matched)

	// Apply cursor.
	if f.Cursor != "" {
		for i, rev := range matched {
			if rev.ID == f.Cursor {
				matched = matched[i+1:]
				break
			}
		}
	}

	// Apply limit.
	nextCursor := ""
	if len(matched) > limit {
		nextCursor = matched[limit-1].ID
		matched = matched[:limit]
	}

	var out []store.Revision
	for _, rev := range matched {
		full := *rev
		full.Changes = s.changes[rev.ID]
		out = append(out, full)
	}
	return out, nextCursor, nil
}

// GetEntityChanges returns all entity changes for the given entity, newest first.
func (s *Store) GetEntityChanges(_ context.Context, entityType, entityID string) ([]store.EntityChange, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	key := entityType + ":" + entityID
	var out []store.EntityChange
	for _, rid := range s.entityIndex[key] {
		for _, ec := range s.changes[rid] {
			if ec.EntityType == entityType && ec.EntityID == entityID {
				out = append(out, ec)
			}
		}
	}
	// Reverse to get newest first (index stores insertion order = oldest first).
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// LatestSnapshot returns the SnapshotAfter of the most recent entity change.
func (s *Store) LatestSnapshot(_ context.Context, entityType, entityID string) (map[string]any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	key := entityType + ":" + entityID
	ids := s.entityIndex[key]
	if len(ids) == 0 {
		return nil, nil
	}
	// Last entry is most recent.
	lastRevID := ids[len(ids)-1]
	for _, ec := range s.changes[lastRevID] {
		if ec.EntityType == entityType && ec.EntityID == entityID {
			return ec.SnapshotAfter, nil
		}
	}
	return nil, nil
}

// SaveRevertLog stores a revert log entry.
func (s *Store) SaveRevertLog(_ context.Context, log store.RevertLog) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revertLogs[log.ID] = &log
	return nil
}

// LastRevisionHash returns the hash of the most recently saved revision.
func (s *Store) LastRevisionHash(_ context.Context) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var latest *store.Revision
	for _, rev := range s.revisions {
		if latest == nil || rev.OccurredAt.After(latest.OccurredAt) {
			latest = rev
		}
	}
	if latest == nil {
		return "", nil
	}
	return latest.Hash, nil
}

// ────────────────────────────────────────────────────────────────
// helpers
// ────────────────────────────────────────────────────────────────

func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// sortRevisions sorts a slice newest-first by OccurredAt (insertion sort for small slices).
func sortRevisions(revs []*store.Revision) {
	for i := 1; i < len(revs); i++ {
		for j := i; j > 0 && revs[j].OccurredAt.After(revs[j-1].OccurredAt); j-- {
			revs[j], revs[j-1] = revs[j-1], revs[j]
		}
	}
}
