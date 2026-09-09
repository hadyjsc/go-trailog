// Package store defines the storage interface and shared query/filter types
// for the trailog audit history library.
package store

import (
	"context"
	"time"
)

// Revision mirrors the domain type but is store-package-local to avoid
// circular imports. The top-level trailog package maps between the two.
type Revision struct {
	ID            string
	CorrelationID string
	ActorID       string
	ActorType     string
	ActorName     string
	ActorEmail    string
	ActorIP       string
	ActorExtra    map[string]any
	Action        string
	Reason        string
	OccurredAt    time.Time
	Metadata      map[string]any
	PrevHash      string
	Hash          string
	Changes       []EntityChange
}

// EntityChange is one row/object touched within a revision.
type EntityChange struct {
	ID             string
	RevisionID     string
	EntityType     string
	EntityID       string
	Op             string // "create" | "update" | "delete"
	SnapshotBefore map[string]any
	SnapshotAfter  map[string]any
	Fields         []FieldDiff
}

// FieldDiff holds the before/after value for one field within an EntityChange.
type FieldDiff struct {
	ID             string
	EntityChangeID string
	FieldName      string
	OldValue       any
	NewValue       any
	ValueType      string
}

// RevertLog records the traceability link between a revert revision and its target.
type RevertLog struct {
	ID               string
	RevertRevisionID string
	TargetRevisionID string
	Strategy         string
	HadConflicts     bool
	ConflictDetail   map[string]any
}

// TimelineFilter parameterises a timeline query.
type TimelineFilter struct {
	EntityType     string
	EntityID       string
	IncludeRelated bool
	RelatedIDs     []EntityRef // pre-resolved related entities (populated by caller)
	ActorID        string
	From           time.Time
	To             time.Time
	Actions        []string
	Limit          int
	Cursor         string // revision ID of the last item on the previous page
}

// EntityRef is a (type, id) pair used in filter lists.
type EntityRef struct {
	Type string
	ID   string
}

// Store is the persistence interface every storage backend must implement.
// All methods are expected to be goroutine-safe.
type Store interface {
	// SaveRevision persists a full Revision with all its EntityChanges,
	// FieldDiffs, and optionally a RevertLog row, in one atomic operation.
	SaveRevision(ctx context.Context, rev Revision) error

	// GetRevision returns the full Revision (with EntityChanges and FieldDiffs)
	// for a given ID.
	GetRevision(ctx context.Context, id string) (*Revision, error)

	// ListRevisions returns a page of Revisions matching the filter, newest first.
	// Cursor-based pagination: pass the ID of the last revision seen as Cursor.
	ListRevisions(ctx context.Context, f TimelineFilter) ([]Revision, string, error)

	// GetEntityChanges returns all EntityChange rows (with FieldDiffs) for a
	// given entity type/id, across all revisions, newest first.
	GetEntityChanges(ctx context.Context, entityType, entityID string) ([]EntityChange, error)

	// LatestSnapshot returns the most recent SnapshotAfter for the given entity,
	// used by the revert engine to compare current state.
	LatestSnapshot(ctx context.Context, entityType, entityID string) (map[string]any, error)

	// SaveRevertLog persists a RevertLog row (called by the Reverter after a
	// successful revert).
	SaveRevertLog(ctx context.Context, log RevertLog) error

	// LastRevisionHash returns the hash field of the most recently written
	// Revision, used to build the tamper-evidence chain.
	LastRevisionHash(ctx context.Context) (string, error)

	// Ping verifies the store is reachable (used at startup).
	Ping(ctx context.Context) error

	// Close releases any held resources.
	Close() error
}
