package trailog

import "time"

// Revision is one atomic unit of change — a "commit" in the GitHub mental model.
// It may touch one or many tables/entities in the same logical action.
type Revision struct {
	ID            string         `json:"id"`
	CorrelationID string         `json:"correlation_id"`
	Actor         Actor          `json:"actor"`
	Action        string         `json:"action"`   // "create" | "update" | "delete" | "revert" | custom
	Reason        string         `json:"reason,omitempty"`
	OccurredAt    time.Time      `json:"occurred_at"`
	Metadata      map[string]any `json:"metadata,omitempty"`
	Changes       []EntityChange `json:"changes,omitempty"`
	// Hash chaining fields (optional tamper-evidence)
	PrevHash string `json:"prev_hash,omitempty"`
	Hash     string `json:"hash,omitempty"`
}

// EntityChange is one row/object touched within a revision —
// a "file changed" in the GitHub mental model.
type EntityChange struct {
	ID             string      `json:"id"`
	RevisionID     string      `json:"revision_id"`
	Entity         Entity      `json:"entity"`
	Op             string      `json:"op"` // "create" | "update" | "delete"
	SnapshotBefore any         `json:"snapshot_before,omitempty"`
	SnapshotAfter  any         `json:"snapshot_after,omitempty"`
	Fields         []FieldDiff `json:"fields,omitempty"`
}

// FieldDiff holds the old and new value for a single field — a "diff line".
// Field supports dot-path notation for nested fields (e.g. "address.city").
type FieldDiff struct {
	Field    string `json:"field"`
	OldValue any    `json:"old_value"`
	NewValue any    `json:"new_value"`
	Type     string `json:"type,omitempty"` // "string","number","bool","json","array"
}

// Entity identifies a specific record by its type and ID.
type Entity struct {
	Type string `json:"type"` // e.g. "invoice", "order"
	ID   string `json:"id"`
}

// String returns a human-readable "type:id" representation.
func (e Entity) String() string {
	return e.Type + ":" + e.ID
}
