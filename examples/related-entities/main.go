// Package main demonstrates Level 3 (explicit multi-table grouping) trailog
// integration: an order service that touches Order, OrderItem, and Payment
// tables in a single revision — the "feature A edits tables A, B, C, D" scenario.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/hadyjsc/go-trailog"
	"github.com/hadyjsc/go-trailog/revert"
	"github.com/hadyjsc/go-trailog/timeline"
)

// ────────────────────────────────────────────────────────────────
// Domain models
// ────────────────────────────────────────────────────────────────

type Order struct {
	ID     string  `json:"id"     trailog:"id"`
	Total  float64 `json:"total"  trailog:"track"`
	Status string  `json:"status" trailog:"track"`
}

type OrderItem struct {
	ID      string  `json:"id"       trailog:"id"`
	OrderID string  `json:"order_id" trailog:"track"`
	SKU     string  `json:"sku"      trailog:"track"`
	Qty     int     `json:"qty"      trailog:"track"`
	Price   float64 `json:"price"    trailog:"track"`
}

type Payment struct {
	ID      string  `json:"id"       trailog:"id"`
	OrderID string  `json:"order_id" trailog:"track"`
	Amount  float64 `json:"amount"   trailog:"track"`
	Method  string  `json:"method"   trailog:"track"`
}

// ────────────────────────────────────────────────────────────────
// Fake in-memory databases
// ────────────────────────────────────────────────────────────────

type store[T any] struct{ rows map[string]T }

func newStore[T any]() *store[T] { return &store[T]{rows: make(map[string]T)} }
func (s *store[T]) get(id string) (T, bool) {
	v, ok := s.rows[id]
	return v, ok
}
func (s *store[T]) save(id string, v T) { s.rows[id] = v }
func (s *store[T]) delete(id string)    { delete(s.rows, id) }

// ────────────────────────────────────────────────────────────────
// Repository adapters
// ────────────────────────────────────────────────────────────────

type orderRepo struct{ db *store[Order] }

func (r orderRepo) Load(_ context.Context, id string) (map[string]any, error) {
	v, ok := r.db.get(id)
	if !ok {
		return nil, fmt.Errorf("order %s not found", id)
	}
	return toMap(v), nil
}
func (r orderRepo) Save(_ context.Context, id string, m map[string]any) error {
	r.db.save(id, Order{ID: id, Total: toFloat(m["total"]), Status: toString(m["status"])})
	return nil
}
func (r orderRepo) Delete(_ context.Context, id string) error { r.db.delete(id); return nil }

type itemRepo struct{ db *store[OrderItem] }

func (r itemRepo) Load(_ context.Context, id string) (map[string]any, error) {
	v, ok := r.db.get(id)
	if !ok {
		return nil, fmt.Errorf("order_item %s not found", id)
	}
	return toMap(v), nil
}
func (r itemRepo) Save(_ context.Context, id string, m map[string]any) error {
	r.db.save(id, OrderItem{
		ID: id, OrderID: toString(m["order_id"]),
		SKU: toString(m["sku"]), Qty: toInt(m["qty"]), Price: toFloat(m["price"]),
	})
	return nil
}
func (r itemRepo) Delete(_ context.Context, id string) error { r.db.delete(id); return nil }

type paymentRepo struct{ db *store[Payment] }

func (r paymentRepo) Load(_ context.Context, id string) (map[string]any, error) {
	v, ok := r.db.get(id)
	if !ok {
		return nil, fmt.Errorf("payment %s not found", id)
	}
	return toMap(v), nil
}
func (r paymentRepo) Save(_ context.Context, id string, m map[string]any) error {
	r.db.save(id, Payment{ID: id, OrderID: toString(m["order_id"]), Amount: toFloat(m["amount"]), Method: toString(m["method"])})
	return nil
}
func (r paymentRepo) Delete(_ context.Context, id string) error { r.db.delete(id); return nil }

// ────────────────────────────────────────────────────────────────
// Order service — the "existing feature" with +3 lines for trailog
// ────────────────────────────────────────────────────────────────

type OrderService struct {
	orders   *store[Order]
	items    *store[OrderItem]
	payments *store[Payment]
	rec      trailog.Recorder
}

// UpdateOrder mirrors the TRD §15.2 example: one revision covers order +
// item updates/creates/deletes + payment adjustment.
func (s *OrderService) UpdateOrder(ctx context.Context, orderID string) error {
	// ── 1. Open explicit revision scope ──────────────────────────
	ctx, rev := s.rec.WithinRevision(ctx, "update_order", "customer edited order #"+orderID)
	defer rev.Discard()

	// ── 2. Existing business logic ────────────────────────────────
	beforeOrder, _ := s.orders.get(orderID)
	afterOrder := beforeOrder
	afterOrder.Total = 250_000
	afterOrder.Status = "confirmed"
	s.orders.save(orderID, afterOrder)

	// Item update.
	beforeItem, _ := s.items.get("item-1")
	afterItem := beforeItem
	afterItem.Qty = 3
	s.items.save("item-1", afterItem)

	// New item created.
	newItem := OrderItem{ID: "item-2", OrderID: orderID, SKU: "SKU-B", Qty: 1, Price: 50_000}
	s.items.save("item-2", newItem)

	// Payment adjustment.
	beforePay, _ := s.payments.get("pay-1")
	afterPay := beforePay
	afterPay.Amount = 250_000
	s.payments.save("pay-1", afterPay)

	// ── 3. Audit — one Record* call per touched entity ────────────
	s.rec.RecordUpdate(ctx, trailog.Entity{Type: "order", ID: orderID}, beforeOrder, afterOrder)
	s.rec.RecordUpdate(ctx, trailog.Entity{Type: "order_item", ID: "item-1"}, beforeItem, afterItem)
	s.rec.RecordCreate(ctx, trailog.Entity{Type: "order_item", ID: "item-2"}, newItem)
	s.rec.RecordUpdate(ctx, trailog.Entity{Type: "payment", ID: "pay-1"}, beforePay, afterPay)

	// ── 4. Commit after business logic succeeds ───────────────────
	return rev.Commit(ctx)
}

// ────────────────────────────────────────────────────────────────
// Main
// ────────────────────────────────────────────────────────────────

func main() {
	orders := newStore[Order]()
	items := newStore[OrderItem]()
	payments := newStore[Payment]()

	// Seed initial data.
	orders.save("ord-1", Order{ID: "ord-1", Total: 100_000, Status: "pending"})
	items.save("item-1", OrderItem{ID: "item-1", OrderID: "ord-1", SKU: "SKU-A", Qty: 2, Price: 50_000})
	payments.save("pay-1", Payment{ID: "pay-1", OrderID: "ord-1", Amount: 100_000, Method: "bank_transfer"})

	// Build Trailog — declare relations so timeline can follow order→items/payment.
	tl, err := trailog.New(
		trailog.WithMemoryStore(),
		trailog.WithRelation("order", "order_item", "has_items", trailog.ChildDependsOnParent),
		trailog.WithRelation("order", "payment", "has_payment", trailog.ChildDependsOnParent),
		trailog.WithRepository("order", orderRepo{orders}),
		trailog.WithRepository("order_item", itemRepo{items}),
		trailog.WithRepository("payment", paymentRepo{payments}),
	)
	must(err)
	defer tl.Close()

	// Static relation declarations (registered via WithRelation above) are sufficient
	// for type-level timeline grouping in this demo. Dynamic resolvers would be
	// registered here via tl.Relations().RegisterResolver(...) for runtime ID lookups.

	ctx := trailog.WithActor(context.Background(), trailog.Actor{
		ID: "user-7", Type: "user", Name: "Budi",
	})

	svc := &OrderService{
		orders:   orders,
		items:    items,
		payments: payments,
		rec:      tl.Recorder(),
	}

	// Run the multi-table update.
	must(svc.UpdateOrder(ctx, "ord-1"))
	fmt.Println("✓ UpdateOrder executed — order, 2 items, payment touched in ONE revision")

	// Query the timeline for the order.
	result, err := tl.Timeline().GetTimeline(ctx, timeline.TimelineQuery{
		Entity:         timeline.Entity{Type: "order", ID: "ord-1"},
		IncludeRelated: false, // order changes only for now
		Limit:          10,
	})
	must(err)
	fmt.Printf("\n── Order timeline (%d revision(s)) ──\n", len(result.Revisions))
	printJSON(result)

	if len(result.Revisions) == 0 {
		log.Fatal("expected at least one revision")
	}

	// Preview a revert of the first revision.
	revID := result.Revisions[0].ID
	plan, err := tl.Reverter().PreviewRevert(ctx, revID)
	must(err)
	fmt.Printf("\n── Revert preview for revision %s ──\n", revID)
	fmt.Printf("   %d action(s), %d conflict(s)\n", len(plan.Actions), len(plan.Conflicts))

	// Execute the revert.
	newRev, err := tl.Reverter().RevertRevision(ctx, revID,
		revert.WithStrategy(revert.StrategyForceOverwrite),
		revert.WithReason("reverting bad order update"),
	)
	must(err)
	fmt.Printf("\n✓ Reverted — new revision: %s\n", newRev.ID)

	ord, _ := orders.get("ord-1")
	fmt.Printf("  order status restored to: %q, total: %.0f\n", ord.Status, ord.Total)
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

func toInt(v any) int {
	if v == nil {
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}
