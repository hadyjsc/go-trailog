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

// WebhookTargetConfig stores the per-entity-type webhook endpoint configuration
// used by the revert engine when no per-request target is supplied.
type WebhookTargetConfig struct {
	ID          string    `json:"id"`
	EntityType  string    `json:"entity_type"`
	URL         string    `json:"url"`
	Method      string    `json:"method"`       // POST | PUT | PATCH
	Auth        string    `json:"auth"`         // verbatim Authorization header value
	TimeoutSecs int       `json:"timeout_secs"` // default 30
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
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

// ListEntitiesFilter parameterises a call to ListEntities.
type ListEntitiesFilter struct {
	// Narrow by entity type (exact match). Empty = all types.
	EntityType string
	// Narrow by actor that last touched the entity.
	ActorID string
	// Narrow by the op that last touched the entity: "create", "update", "delete".
	Op string
	// Time range applied to the latest change's occurred_at.
	From time.Time
	To   time.Time

	// SortField controls the ORDER BY column.
	// Allowed values: "entity_type" | "entity_id" | "last_changed_at" (default).
	SortField string
	// SortDir is "asc" or "desc" (default "desc").
	SortDir string

	// Cursor-based pagination. Pass the cursor returned in the previous response.
	Limit  int
	Cursor string // opaque — encodes "<last_occurred_at>|<last_entity_id>"
}

// EntitySummary is one row returned by ListEntities: the distinct entity plus
// metadata from its most recent audit record.
type EntitySummary struct {
	EntityType    string    `json:"entity_type"`
	EntityID      string    `json:"entity_id"`
	LastChangedAt time.Time `json:"last_changed_at"`
	LastOp        string    `json:"last_op"`
	LastActorID   string    `json:"last_actor_id"`
	LastActorName string    `json:"last_actor_name"`
	RevisionCount int       `json:"revision_count"`
}

// ListEntitiesResult is the paginated response from ListEntities.
type ListEntitiesResult struct {
	Entities   []EntitySummary `json:"entities"`
	NextCursor string          `json:"next_cursor,omitempty"`
	Total      int             `json:"total"` // total matching rows (unpaged)
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
	// ListEntities returns a paginated list of distinct (entity_type, entity_id) pairs
	// that have at least one audit record, enriched with metadata from the most recent
	// change. Supports filtering, sorting, and cursor-based pagination.
	ListEntities(ctx context.Context, f ListEntitiesFilter) (*ListEntitiesResult, error)

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

	// DeleteRevision removes a revision and all its EntityChanges and FieldDiffs.
	// Used exclusively by the revert engine to roll back an audit record when the
	// downstream webhook call fails after SaveRevision was already committed.
	DeleteRevision(ctx context.Context, id string) error

	// GetWebhookTargetConfig returns the stored webhook target for an entity type,
	// or nil if none is configured.
	GetWebhookTargetConfig(ctx context.Context, entityType string) (*WebhookTargetConfig, error)

	// SaveWebhookTargetConfig upserts (insert-or-update) the webhook target
	// configuration for an entity type.
	SaveWebhookTargetConfig(ctx context.Context, cfg WebhookTargetConfig) error

	// DeleteWebhookTargetConfig removes the webhook target configuration for an
	// entity type. Returns nil if no row existed.
	DeleteWebhookTargetConfig(ctx context.Context, entityType string) error

	// Ping verifies the store is reachable (used at startup).
	Ping(ctx context.Context) error

	// Close releases any held resources.
	Close() error
}
