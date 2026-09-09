package timeline

import (
	"time"

	"github.com/trailog/trailog/store"
)

// The types below are the timeline-package's public-facing view models.
// They mirror the store types but are shaped for JSON API responses.

// Revision is the API-layer view of an audit_revision row, with embedded changes.
type Revision struct {
	ID            string         `json:"id"`
	CorrelationID string         `json:"correlation_id"`
	Actor         Actor          `json:"actor"`
	Action        string         `json:"action"`
	Reason        string         `json:"reason,omitempty"`
	OccurredAt    time.Time      `json:"occurred_at"`
	Metadata      map[string]any `json:"metadata,omitempty"`
	Changes       []EntityChange `json:"changes,omitempty"`
	PrevHash      string         `json:"prev_hash,omitempty"`
	Hash          string         `json:"hash,omitempty"`
}

// Actor is the API-layer view of an actor.
type Actor struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
	IP    string `json:"ip,omitempty"`
}

// EntityChange is the API-layer view of one entity changed within a revision.
type EntityChange struct {
	ID             string         `json:"id"`
	RevisionID     string         `json:"revision_id"`
	Entity         EntityRef      `json:"entity"`
	Op             string         `json:"op"`
	SnapshotBefore map[string]any `json:"snapshot_before,omitempty"`
	SnapshotAfter  map[string]any `json:"snapshot_after,omitempty"`
	Fields         []FieldDiff    `json:"fields,omitempty"`
}

// EntityRef is a {type, id} pair.
type EntityRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// FieldDiff is the API-layer view of one changed field.
type FieldDiff struct {
	Field    string `json:"field"`
	OldValue any    `json:"old_value"`
	NewValue any    `json:"new_value"`
	Type     string `json:"type,omitempty"`
}

// ────────────────────────────────────────────────────────────────
// conversion helpers (store → timeline view models)
// ────────────────────────────────────────────────────────────────

func toRevisions(revs []store.Revision) []Revision {
	out := make([]Revision, len(revs))
	for i, r := range revs {
		out[i] = toRevision(r)
	}
	return out
}

func toRevision(r store.Revision) Revision {
	rev := Revision{
		ID:            r.ID,
		CorrelationID: r.CorrelationID,
		Actor: Actor{
			ID:    r.ActorID,
			Type:  r.ActorType,
			Name:  r.ActorName,
			Email: r.ActorEmail,
			IP:    r.ActorIP,
		},
		Action:     r.Action,
		Reason:     r.Reason,
		OccurredAt: r.OccurredAt,
		Metadata:   r.Metadata,
		PrevHash:   r.PrevHash,
		Hash:       r.Hash,
	}

	for _, ec := range r.Changes {
		rev.Changes = append(rev.Changes, toEntityChange(ec))
	}
	return rev
}

func toEntityChange(ec store.EntityChange) EntityChange {
	out := EntityChange{
		ID:         ec.ID,
		RevisionID: ec.RevisionID,
		Entity:     EntityRef{Type: ec.EntityType, ID: ec.EntityID},
		Op:         ec.Op,
		SnapshotBefore: ec.SnapshotBefore,
		SnapshotAfter:  ec.SnapshotAfter,
	}
	for _, fd := range ec.Fields {
		out.Fields = append(out.Fields, FieldDiff{
			Field:    fd.FieldName,
			OldValue: fd.OldValue,
			NewValue: fd.NewValue,
			Type:     fd.ValueType,
		})
	}
	return out
}
