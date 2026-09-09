package trailog

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/trailog/trailog/diff"
	"github.com/trailog/trailog/store"
)

// recordOptions holds per-call overrides applied via Option functions.
type recordOptions struct {
	reason     string
	metadata   map[string]any
	fieldMask  []string // fields to exclude at call site
}

// Option is a functional option for Record* calls.
type Option func(*recordOptions)

// WithReason attaches a human-readable reason/message to the revision,
// analogous to a git commit message.
func WithReason(msg string) Option {
	return func(o *recordOptions) { o.reason = msg }
}

// WithMetadata attaches arbitrary key-value metadata to the revision
// (e.g. request ID, source system, IP).
func WithMetadata(kv map[string]any) Option {
	return func(o *recordOptions) { o.metadata = kv }
}

// WithFieldMask excludes specific field names from the diff at call site,
// in addition to any struct-tag-level masking.
func WithFieldMask(fields ...string) Option {
	return func(o *recordOptions) { o.fieldMask = fields }
}

// Recorder is the primary write-path interface. All Record* methods produce
// audit diffs and persist them via the configured Store.
type Recorder interface {
	RecordCreate(ctx context.Context, entity Entity, after any, opts ...Option) error
	RecordUpdate(ctx context.Context, entity Entity, before, after any, opts ...Option) error
	RecordDelete(ctx context.Context, entity Entity, before any, opts ...Option) error
	RecordCustom(ctx context.Context, entity Entity, action string, before, after any, opts ...Option) error

	// WithinRevision opens an explicit multi-table revision scope. All Record*
	// calls made using the returned context are buffered until Commit() is called.
	// See RevisionHandle for details.
	WithinRevision(ctx context.Context, action, reason string, opts ...Option) (context.Context, RevisionHandle)
}

// recorder is the concrete implementation of Recorder.
type recorder struct {
	st         store.Store
	dispatcher dispatcher
}

func newRecorder(s store.Store, d dispatcher) *recorder {
	return &recorder{st: s, dispatcher: d}
}

// RecordCreate records a create event for the given entity.
func (r *recorder) RecordCreate(ctx context.Context, entity Entity, after any, opts ...Option) error {
	return r.record(ctx, entity, "create", nil, after, opts)
}

// RecordUpdate records an update event. before and after must be the same type.
func (r *recorder) RecordUpdate(ctx context.Context, entity Entity, before, after any, opts ...Option) error {
	return r.record(ctx, entity, "update", before, after, opts)
}

// RecordDelete records a delete event for the given entity.
func (r *recorder) RecordDelete(ctx context.Context, entity Entity, before any, opts ...Option) error {
	return r.record(ctx, entity, "delete", before, nil, opts)
}

// RecordCustom records a custom-action event (e.g. "approve", "archive").
func (r *recorder) RecordCustom(ctx context.Context, entity Entity, action string, before, after any, opts ...Option) error {
	return r.record(ctx, entity, action, before, after, opts)
}

// WithinRevision opens an explicit revision scope and returns a new context
// with the revision buffered into it, plus a RevisionHandle to commit or discard.
func (r *recorder) WithinRevision(ctx context.Context, action, reason string, opts ...Option) (context.Context, RevisionHandle) {
	o := applyOpts(opts)
	if o.reason != "" && reason == "" {
		reason = o.reason
	}

	revID := uuid.New().String()
	handle := newRevisionHandle(revID, action, reason, o.metadata, r.st, r.dispatcher)
	ctx = contextWithRevisionHandle(ctx, handle)
	return ctx, handle
}

// record is the shared implementation for all Record* variants.
func (r *recorder) record(ctx context.Context, entity Entity, action string, before, after any, opts []Option) error {
	o := applyOpts(opts)

	// Compute field-level diffs.
	fieldDiffs, err := computeDiffs(before, after, o.fieldMask)
	if err != nil {
		return fmt.Errorf("trailog: diff for %s: %w", entity, err)
	}

	// If nothing changed, skip — avoids noise in the audit log.
	if action == "update" && len(fieldDiffs) == 0 {
		return nil
	}

	ec := buildEntityChange(entity, action, before, after, fieldDiffs)

	// If we're inside an explicit WithinRevision scope, buffer the change.
	if handle, ok := revisionHandleFromContext(ctx); ok {
		handle.buffer(ec)
		return nil
	}

	// Fallback: auto-revision per call (or merged by correlationID via upsert).
	actor, _ := ActorFromContext(ctx)
	corrID, _ := CorrelationIDFromContext(ctx)
	if corrID == "" {
		corrID = uuid.New().String()
	}

	rev := buildRevision(uuid.New().String(), corrID, action, o.reason, o.metadata, actor, []store.EntityChange{ec})
	return r.dispatcher.dispatch(ctx, rev)
}

// ────────────────────────────────────────────────────────────────
// helpers
// ────────────────────────────────────────────────────────────────

func applyOpts(opts []Option) *recordOptions {
	o := &recordOptions{}
	for _, fn := range opts {
		fn(o)
	}
	return o
}

func computeDiffs(before, after any, mask []string) ([]store.FieldDiff, error) {
	rawDiffs, err := diff.Struct(before, after)
	if err != nil {
		// Fall back to JSON diff if struct diff fails (e.g. map[string]any input).
		bMap := toMap(before)
		aMap := toMap(after)
		jsonDiffs := diff.JSON(bMap, aMap)
		rawDiffs = make([]diff.FieldDiff, len(jsonDiffs))
		for i, d := range jsonDiffs {
			rawDiffs[i] = diff.FieldDiff(d)
		}
	}

	maskSet := make(map[string]struct{}, len(mask))
	for _, f := range mask {
		maskSet[f] = struct{}{}
	}

	var out []store.FieldDiff
	for _, d := range rawDiffs {
		if _, masked := maskSet[d.Field]; masked {
			continue
		}
		out = append(out, store.FieldDiff{
			ID:        uuid.New().String(),
			FieldName: d.Field,
			OldValue:  d.OldValue,
			NewValue:  d.NewValue,
			ValueType: d.Type,
		})
	}
	return out, nil
}

func buildEntityChange(entity Entity, op string, before, after any, diffs []store.FieldDiff) store.EntityChange {
	ecID := uuid.New().String()

	ec := store.EntityChange{
		ID:         ecID,
		EntityType: entity.Type,
		EntityID:   entity.ID,
		Op:         op,
		Fields:     diffs,
	}
	if before != nil {
		ec.SnapshotBefore = toMap(before)
	}
	if after != nil {
		ec.SnapshotAfter = toMap(after)
	}
	// Populate the EntityChangeID on each FieldDiff row.
	for i := range ec.Fields {
		ec.Fields[i].EntityChangeID = ecID
	}
	return ec
}

func buildRevision(id, corrID, action, reason string, meta map[string]any, actor Actor, changes []store.EntityChange) store.Revision {
	rev := store.Revision{
		ID:            id,
		CorrelationID: corrID,
		ActorID:       actor.ID,
		ActorType:     actor.Type,
		ActorName:     actor.Name,
		ActorEmail:    actor.Email,
		ActorIP:       actor.IP,
		ActorExtra:    actor.Extra,
		Action:        action,
		Reason:        reason,
		OccurredAt:    time.Now().UTC(),
		Metadata:      meta,
		Changes:       changes,
	}
	// Wire RevisionID into each EntityChange.
	for i := range rev.Changes {
		rev.Changes[i].RevisionID = id
	}
	return rev
}

// computeRevisionHash computes the tamper-evidence hash for a revision.
func computeRevisionHash(rev store.Revision) string {
	b, _ := json.Marshal(struct {
		ID         string    `json:"id"`
		Action     string    `json:"action"`
		OccurredAt time.Time `json:"occurred_at"`
		PrevHash   string    `json:"prev_hash"`
	}{
		ID:         rev.ID,
		Action:     rev.Action,
		OccurredAt: rev.OccurredAt,
		PrevHash:   rev.PrevHash,
	})
	h := sha256.Sum256(b)
	return fmt.Sprintf("%x", h)
}

// toMap converts any value to map[string]any via JSON round-trip.
func toMap(v any) map[string]any {
	if v == nil {
		return nil
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}
