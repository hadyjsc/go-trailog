package trailog

import (
	"context"
	"fmt"
	"sync"

	"github.com/trailog/trailog/store"
)

// dispatcher is the internal write-path abstraction: sync writes directly to the
// store; async writes via a buffered worker pool.
type dispatcher interface {
	dispatch(ctx context.Context, rev store.Revision) error
	close()
}

// ────────────────────────────────────────────────────────────────
// Sync dispatcher — writes inline, same goroutine as the caller.
// ────────────────────────────────────────────────────────────────

type syncDispatcher struct {
	st store.Store
}

func newSyncDispatcher(s store.Store) *syncDispatcher {
	return &syncDispatcher{st: s}
}

func (d *syncDispatcher) dispatch(ctx context.Context, rev store.Revision) error {
	if err := d.st.SaveRevision(ctx, rev); err != nil {
		return fmt.Errorf("trailog/dispatcher: save revision: %w", err)
	}
	return nil
}

func (d *syncDispatcher) close() {}

// ────────────────────────────────────────────────────────────────
// Async dispatcher — non-blocking; workers drain a buffered channel.
// ────────────────────────────────────────────────────────────────

type asyncDispatcher struct {
	st      store.Store
	queue   chan store.Revision
	workers int
	wg      sync.WaitGroup
	once    sync.Once
	errFn   func(err error) // called on persistent write failures
}

func newAsyncDispatcher(s store.Store, bufferSize, workers int, errFn func(error)) *asyncDispatcher {
	if bufferSize <= 0 {
		bufferSize = 512
	}
	if workers <= 0 {
		workers = 4
	}
	d := &asyncDispatcher{
		st:      s,
		queue:   make(chan store.Revision, bufferSize),
		workers: workers,
		errFn:   errFn,
	}
	d.start()
	return d
}

func (d *asyncDispatcher) start() {
	for i := 0; i < d.workers; i++ {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			for rev := range d.queue {
				if err := d.st.SaveRevision(context.Background(), rev); err != nil && d.errFn != nil {
					d.errFn(err)
				}
			}
		}()
	}
}

// dispatch enqueues the revision. If the queue is full it falls back to a sync write.
func (d *asyncDispatcher) dispatch(_ context.Context, rev store.Revision) error {
	select {
	case d.queue <- rev:
		return nil
	default:
		// Queue full — fall back to synchronous write to avoid dropping audit records.
		return d.st.SaveRevision(context.Background(), rev)
	}
}

// close drains the queue and waits for all workers to finish.
func (d *asyncDispatcher) close() {
	d.once.Do(func() {
		close(d.queue)
		d.wg.Wait()
	})
}
