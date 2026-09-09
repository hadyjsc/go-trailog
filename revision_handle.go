package trailog

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/trailog/trailog/store"
)

// revisionHandleContextKey is the context key for an in-flight RevisionHandle.
type revisionHandleContextKey struct{}

// RevisionHandle controls a buffered multi-table revision opened via WithinRevision.
// Record* calls made with the returned context are held in memory until either
// Commit (which flushes them atomically) or Discard (which drops them).
type RevisionHandle interface {
	// Commit flushes the buffered revision and all entity changes to the audit store.
	// Call this AFTER your business DB transaction has committed successfully.
	Commit(ctx context.Context) error

	// Discard drops all buffered changes. This is safe to call via defer even
	// after a successful Commit — it becomes a no-op once committed.
	Discard()
}

// revisionHandle is the concrete implementation.
type revisionHandle struct {
	mu         sync.Mutex
	revisionID string
	action     string
	reason     string
	metadata   map[string]any
	changes    []store.EntityChange
	committed  bool
	discarded  bool

	st   store.Store
	disp dispatcher
}

func newRevisionHandle(revID, action, reason string, meta map[string]any, s store.Store, d dispatcher) *revisionHandle {
	return &revisionHandle{
		revisionID: revID,
		action:     action,
		reason:     reason,
		metadata:   meta,
		st:         s,
		disp:       d,
	}
}

// buffer adds an EntityChange to the in-memory buffer.
// Called by recorder.record() when it detects an active revision handle in ctx.
func (h *revisionHandle) buffer(ec store.EntityChange) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.committed || h.discarded {
		return
	}
	ec.RevisionID = h.revisionID
	h.changes = append(h.changes, ec)
}

// Commit assembles the full Revision and hands it to the dispatcher.
func (h *revisionHandle) Commit(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.discarded {
		return fmt.Errorf("trailog: cannot commit — revision already discarded")
	}
	if h.committed {
		return nil // idempotent
	}

	// Nothing buffered — skip to avoid empty revision noise.
	if len(h.changes) == 0 {
		h.committed = true
		return nil
	}

	actor, _ := ActorFromContext(ctx)
	corrID, _ := CorrelationIDFromContext(ctx)
	if corrID == "" {
		corrID = uuid.New().String()
	}

	rev := buildRevision(h.revisionID, corrID, h.action, h.reason, h.metadata, actor, h.changes)

	// Hash chaining.
	prevHash, _ := h.st.LastRevisionHash(ctx)
	rev.PrevHash = prevHash
	rev.Hash = computeRevisionHash(rev)

	if err := h.disp.dispatch(ctx, rev); err != nil {
		return fmt.Errorf("trailog: commit revision %s: %w", h.revisionID, err)
	}

	h.committed = true
	return nil
}

// Discard drops all buffered changes. Safe to call multiple times (idempotent).
func (h *revisionHandle) Discard() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.committed {
		h.discarded = true
		h.changes = nil
	}
}

// contextWithRevisionHandle stores the handle in a context.
func contextWithRevisionHandle(ctx context.Context, h *revisionHandle) context.Context {
	return context.WithValue(ctx, revisionHandleContextKey{}, h)
}

// revisionHandleFromContext retrieves the active RevisionHandle from context.
func revisionHandleFromContext(ctx context.Context) (*revisionHandle, bool) {
	h, ok := ctx.Value(revisionHandleContextKey{}).(*revisionHandle)
	return h, ok && h != nil
}
