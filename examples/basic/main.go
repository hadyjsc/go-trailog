// Package main demonstrates Level 2 (one-liner) trailog integration:
// a simple invoice service that records create/update/delete events
// against a single entity type, with timeline and revert queries.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/hadyjsc/go-trailog"
	"github.com/hadyjsc/go-trailog/revert"
	"github.com/hadyjsc/go-trailog/timeline"
)

// ────────────────────────────────────────────────────────────────
// Domain model
// ────────────────────────────────────────────────────────────────

// Invoice is the application entity being audited.
type Invoice struct {
	ID       string  `json:"id"      trailog:"id"`
	Amount   float64 `json:"amount"  trailog:"track"`
	Status   string  `json:"status"  trailog:"track"`
	Notes    string  `json:"notes"   trailog:"track"`
	APIToken string  `json:"api_token" trailog:"-"` // never tracked
}

// ────────────────────────────────────────────────────────────────
// Fake in-memory "database"
// ────────────────────────────────────────────────────────────────

type invoiceDB struct {
	rows map[string]Invoice
}

func newInvoiceDB() *invoiceDB { return &invoiceDB{rows: make(map[string]Invoice)} }

func (db *invoiceDB) get(id string) (Invoice, bool) {
	inv, ok := db.rows[id]
	return inv, ok
}
func (db *invoiceDB) save(inv Invoice) { db.rows[inv.ID] = inv }
func (db *invoiceDB) delete(id string) { delete(db.rows, id) }

// ────────────────────────────────────────────────────────────────
// Repository adapter (required for revert)
// ────────────────────────────────────────────────────────────────

type invoiceRepoAdapter struct{ db *invoiceDB }

func (a invoiceRepoAdapter) Load(_ context.Context, id string) (map[string]any, error) {
	inv, ok := a.db.get(id)
	if !ok {
		return nil, fmt.Errorf("invoice %s not found", id)
	}
	return toMap(inv), nil
}
func (a invoiceRepoAdapter) Save(_ context.Context, id string, entity map[string]any) error {
	inv := Invoice{
		ID:     id,
		Amount: toFloat(entity["amount"]),
		Status: toString(entity["status"]),
		Notes:  toString(entity["notes"]),
	}
	a.db.save(inv)
	return nil
}
func (a invoiceRepoAdapter) Delete(_ context.Context, id string) error {
	a.db.delete(id)
	return nil
}

// ────────────────────────────────────────────────────────────────
// Service
// ────────────────────────────────────────────────────────────────

type InvoiceService struct {
	db  *invoiceDB
	rec trailog.Recorder
}

func (s *InvoiceService) Create(ctx context.Context, inv Invoice) error {
	s.db.save(inv)
	return s.rec.RecordCreate(ctx, trailog.Entity{Type: "invoice", ID: inv.ID}, inv)
}

func (s *InvoiceService) UpdateStatus(ctx context.Context, id, status, reason string) error {
	before, ok := s.db.get(id)
	if !ok {
		return fmt.Errorf("invoice %s not found", id)
	}
	after := before
	after.Status = status
	s.db.save(after)
	return s.rec.RecordUpdate(ctx,
		trailog.Entity{Type: "invoice", ID: id},
		before, after,
		trailog.WithReason(reason),
	)
}

func (s *InvoiceService) Delete(ctx context.Context, id string) error {
	before, ok := s.db.get(id)
	if !ok {
		return fmt.Errorf("invoice %s not found", id)
	}
	s.db.delete(id)
	return s.rec.RecordDelete(ctx, trailog.Entity{Type: "invoice", ID: id}, before)
}

// ────────────────────────────────────────────────────────────────
// Main
// ────────────────────────────────────────────────────────────────

func main() {
	// 1. Build Trailog with an in-memory store (swap for WithPostgresStore in prod).
	db := newInvoiceDB()

	tl, err := trailog.New(
		trailog.WithMemoryStore(),
		trailog.WithRepository("invoice", invoiceRepoAdapter{db}),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer tl.Close()

	// 2. Set up actor context (normally done by HTTP middleware).
	ctx := trailog.WithActor(context.Background(), trailog.Actor{
		ID:   "user-42",
		Type: "user",
		Name: "Alice",
	})

	svc := &InvoiceService{db: db, rec: tl.Recorder()}
	tl.Repos().Register("invoice", invoiceRepoAdapter{db})

	// 3. Create an invoice.
	inv := Invoice{ID: "inv-1", Amount: 100_000, Status: "draft", APIToken: "secret"}
	must(svc.Create(ctx, inv))
	fmt.Println("✓ created invoice inv-1")

	// 4. Update status twice.
	must(svc.UpdateStatus(ctx, "inv-1", "sent", "sent to customer"))
	must(svc.UpdateStatus(ctx, "inv-1", "paid", "payment received"))
	fmt.Println("✓ updated status: draft → sent → paid")

	// 5. Print the timeline.
	result, err := tl.Timeline().GetTimeline(ctx, timeline.TimelineQuery{
		Entity: timeline.Entity{Type: "invoice", ID: "inv-1"},
		Limit:  10,
	})
	must(err)
	fmt.Printf("\n── Timeline for invoice inv-1 (%d revisions) ──\n", len(result.Revisions))
	printJSON(result)

	// 6. Revert the most recent change.
	if len(result.Revisions) == 0 {
		fmt.Println("no revisions to revert")
		os.Exit(0)
	}
	latestRevID := result.Revisions[0].ID
	plan, err := tl.Reverter().PreviewRevert(ctx, latestRevID)
	must(err)
	fmt.Printf("\n── Revert preview for revision %s ──\n", latestRevID)
	printJSON(plan)

	newRev, err := tl.Reverter().RevertRevision(ctx, latestRevID,
		revert.WithStrategy(revert.StrategyFieldLevel),
		revert.WithReason("rolling back accidental status change"),
	)
	must(err)
	fmt.Printf("\n✓ Reverted — new revision ID: %s\n", newRev.ID)
	inv, _ = db.get("inv-1")
	fmt.Printf("  invoice status is now: %q\n", inv.Status)
}

// ────────────────────────────────────────────────────────────────
// helpers
// ────────────────────────────────────────────────────────────────

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func printJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

func toMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func toString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func toFloat(v any) float64 {
	if v == nil {
		return 0
	}
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

// Ensure time package is used (imported for potential future use).
var _ = time.Now
