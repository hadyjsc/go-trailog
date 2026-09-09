// Package outbox implements the transactional outbox pattern for reliable
// async audit writes (§9, §13 of TRD).
//
// How it works:
//  1. When dispatch() is called, it writes a compact JSON "intent" row into the
//     audit_outbox table inside whatever DB transaction the caller provides.
//     This ensures the audit intent is committed atomically with the business write.
//  2. A background Flusher goroutine polls for un-flushed outbox rows, writes
//     them to the audit Store, then marks them delivered.
//
// This avoids losing audit records if the process crashes right after a business
// commit but before the async worker drains the in-memory channel.
package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/trailog/trailog/store"
)

// ────────────────────────────────────────────────────────────────
// Schema (create via migration, not here)
//
// CREATE TABLE audit_outbox (
//     id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
//     payload     JSONB NOT NULL,
//     created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
//     delivered   BOOLEAN NOT NULL DEFAULT false,
//     delivered_at TIMESTAMPTZ
// );
// CREATE INDEX idx_outbox_undelivered ON audit_outbox (delivered, created_at)
//     WHERE delivered = false;
// ────────────────────────────────────────────────────────────────

// Dispatcher implements the trailog internal dispatcher interface via the outbox.
// It writes intent rows into the audit_outbox table instead of directly to the
// audit store, so the write is part of the caller's DB transaction.
type Dispatcher struct {
	db    *sql.DB
	store store.Store

	// Flusher config.
	pollInterval time.Duration
	batchSize    int
	errFn        func(error)

	stopCh chan struct{}
	once   sync.Once
	wg     sync.WaitGroup
}

// Option configures the outbox Dispatcher.
type Option func(*Dispatcher)

// WithPollInterval sets how often the flusher checks for pending rows (default 2s).
func WithPollInterval(d time.Duration) Option {
	return func(o *Dispatcher) { o.pollInterval = d }
}

// WithBatchSize sets how many outbox rows are processed per poll cycle (default 50).
func WithBatchSize(n int) Option {
	return func(o *Dispatcher) { o.batchSize = n }
}

// WithErrorHandler sets a callback for write errors encountered by the flusher.
func WithErrorHandler(fn func(error)) Option {
	return func(o *Dispatcher) { o.errFn = fn }
}

// New creates a Dispatcher wired to the given *sql.DB (for outbox writes) and
// Store (for final audit writes by the flusher). Call Start() to begin flushing.
func New(db *sql.DB, s store.Store, opts ...Option) *Dispatcher {
	d := &Dispatcher{
		db:           db,
		store:        s,
		pollInterval: 2 * time.Second,
		batchSize:    50,
		stopCh:       make(chan struct{}),
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// Start launches the background flusher goroutine.
func (d *Dispatcher) Start() {
	d.wg.Add(1)
	go d.flush()
}

// Stop gracefully shuts down the flusher after draining remaining rows.
func (d *Dispatcher) Stop() {
	d.once.Do(func() { close(d.stopCh) })
	d.wg.Wait()
}

// Dispatch writes a revision as a pending outbox row. It MUST be called
// within the same *sql.Tx as the business write to get the atomicity guarantee.
// If tx is nil it writes outside a transaction (fallback, less safe).
func (d *Dispatcher) Dispatch(ctx context.Context, tx *sql.Tx, rev store.Revision) error {
	b, err := json.Marshal(rev)
	if err != nil {
		return fmt.Errorf("trailog/outbox: marshal revision: %w", err)
	}

	const q = `INSERT INTO audit_outbox (id, payload) VALUES ($1, $2)`
	id := uuid.New().String()

	if tx != nil {
		_, err = tx.ExecContext(ctx, q, id, string(b))
	} else {
		_, err = d.db.ExecContext(ctx, q, id, string(b))
	}
	if err != nil {
		return fmt.Errorf("trailog/outbox: insert outbox row: %w", err)
	}
	return nil
}

// ────────────────────────────────────────────────────────────────
// Flusher
// ────────────────────────────────────────────────────────────────

func (d *Dispatcher) flush() {
	defer d.wg.Done()
	ticker := time.NewTicker(d.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-d.stopCh:
			// Drain remaining rows before exiting.
			_ = d.flushBatch(context.Background())
			return
		case <-ticker.C:
			if err := d.flushBatch(context.Background()); err != nil && d.errFn != nil {
				d.errFn(err)
			}
		}
	}
}

func (d *Dispatcher) flushBatch(ctx context.Context) error {
	rows, err := d.pendingRows(ctx)
	if err != nil {
		return err
	}

	for _, row := range rows {
		var rev store.Revision
		if err := json.Unmarshal(row.payload, &rev); err != nil {
			d.markDelivered(ctx, row.id, true) // poison row — mark done so it doesn't block
			if d.errFn != nil {
				d.errFn(fmt.Errorf("trailog/outbox: unmarshal row %s: %w", row.id, err))
			}
			continue
		}

		if err := d.store.SaveRevision(ctx, rev); err != nil {
			if d.errFn != nil {
				d.errFn(fmt.Errorf("trailog/outbox: save revision %s: %w", row.id, err))
			}
			continue // leave row for next cycle
		}

		d.markDelivered(ctx, row.id, false)
	}
	return nil
}

type outboxRow struct {
	id      string
	payload []byte
}

func (d *Dispatcher) pendingRows(ctx context.Context) ([]outboxRow, error) {
	const q = `
		SELECT id, payload FROM audit_outbox
		WHERE delivered = false
		ORDER BY created_at ASC
		LIMIT $1
		FOR UPDATE SKIP LOCKED`

	rows, err := d.db.QueryContext(ctx, q, d.batchSize)
	if err != nil {
		return nil, fmt.Errorf("trailog/outbox: query pending: %w", err)
	}
	defer rows.Close()

	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		var payloadStr string
		if err := rows.Scan(&r.id, &payloadStr); err != nil {
			return nil, err
		}
		r.payload = []byte(payloadStr)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *Dispatcher) markDelivered(ctx context.Context, id string, _ bool) {
	const q = `UPDATE audit_outbox SET delivered = true, delivered_at = now() WHERE id = $1`
	_, _ = d.db.ExecContext(ctx, q, id)
}
