package revert_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	trailog "github.com/hadyjsc/go-trailog"
	"github.com/hadyjsc/go-trailog/revert"
	"github.com/hadyjsc/go-trailog/store"
	"github.com/hadyjsc/go-trailog/store/memory"
)

// ────────────────────────────────────────────────────────────────
// helpers
// ────────────────────────────────────────────────────────────────

func seedRevision(t *testing.T, tl *trailog.Trailog, entityType, entityID string) string {
	t.Helper()
	ctx := trailog.WithActor(context.Background(), trailog.Actor{ID: "u1", Type: "user", Name: "Alice"})
	before := map[string]any{"name": "before", "code": "B01"}
	after := map[string]any{"name": "after", "code": "B01"}
	if err := tl.Recorder().RecordUpdate(ctx,
		trailog.Entity{Type: entityType, ID: entityID},
		before, after,
		trailog.WithReason("initial update"),
	); err != nil {
		t.Fatalf("RecordUpdate: %v", err)
	}
	res, _, err := tl.Store().ListRevisions(ctx, store.TimelineFilter{
		EntityType: entityType, EntityID: entityID, Limit: 1,
	})
	if err != nil || len(res) == 0 {
		t.Fatalf("ListRevisions: %v (len=%d)", err, len(res))
	}
	return res[0].ID
}

// ────────────────────────────────────────────────────────────────
// Test: stored webhook config is resolved automatically
// ────────────────────────────────────────────────────────────────

func TestRevertRevision_StoredWebhookConfig(t *testing.T) {
	var received []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		received = append(received, p)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	mem := memory.New()
	tl, err := trailog.New(trailog.WithStore(mem))
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()

	const entityType = "bank"
	const entityID = "HES0001"
	revID := seedRevision(t, tl, entityType, entityID)

	ctx := context.Background()
	if err := mem.SaveWebhookTargetConfig(ctx, store.WebhookTargetConfig{
		ID: "cfg-1", EntityType: entityType,
		URL: srv.URL + "/revert/:id", Method: "PUT",
		Auth: "Bearer test-token", TimeoutSecs: 5,
	}); err != nil {
		t.Fatalf("SaveWebhookTargetConfig: %v", err)
	}

	// No per-request target — reverter must pick up stored config.
	newRev, err := tl.Reverter().RevertRevision(ctx, revID)
	if err != nil {
		t.Fatalf("RevertRevision: %v", err)
	}
	if len(received) == 0 {
		t.Fatal("expected webhook to be called")
	}
	if received[0]["revision_id"] != newRev.ID {
		t.Errorf("payload revision_id = %v, want %v", received[0]["revision_id"], newRev.ID)
	}
	if received[0]["entity_type"] != entityType {
		t.Errorf("payload entity_type = %v, want %v", received[0]["entity_type"], entityType)
	}
}

// ────────────────────────────────────────────────────────────────
// Test: rollback on webhook failure deletes the audit revision
// ────────────────────────────────────────────────────────────────

func TestRevertRevision_RollbackOnWebhookFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	mem := memory.New()
	tl, err := trailog.New(trailog.WithStore(mem))
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()

	const entityType = "bank"
	const entityID = "HES0002"
	revID := seedRevision(t, tl, entityType, entityID)

	ctx := context.Background()
	revsBefore, _, _ := tl.Store().ListRevisions(ctx, store.TimelineFilter{
		EntityType: entityType, EntityID: entityID, Limit: 100,
	})
	countBefore := len(revsBefore)

	_, err = tl.Reverter().RevertRevision(ctx, revID,
		revert.WithWebhookApplier(revert.WebhookTarget{
			URL:     srv.URL + "/revert/:id",
			Method:  "POST",
			Timeout: 5,
		}),
	)
	if err == nil {
		t.Fatal("expected error from failing webhook, got nil")
	}

	// Audit revision must have been rolled back.
	revsAfter, _, _ := tl.Store().ListRevisions(ctx, store.TimelineFilter{
		EntityType: entityType, EntityID: entityID, Limit: 100,
	})
	if len(revsAfter) != countBefore {
		t.Errorf("revision count after failed revert = %d, want %d (rollback failed)",
			len(revsAfter), countBefore)
	}
}

// ────────────────────────────────────────────────────────────────
// Test: per-request target takes precedence over stored config
// ────────────────────────────────────────────────────────────────

func TestRevertRevision_PerRequestTargetOverridesStored(t *testing.T) {
	var perRequestHit, storedHit int
	perRequestSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		perRequestHit++
		w.WriteHeader(http.StatusOK)
	}))
	defer perRequestSrv.Close()

	storedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		storedHit++
		w.WriteHeader(http.StatusOK)
	}))
	defer storedSrv.Close()

	mem := memory.New()
	tl, err := trailog.New(trailog.WithStore(mem))
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()

	const entityType = "order"
	const entityID = "ORD001"
	revID := seedRevision(t, tl, entityType, entityID)

	ctx := context.Background()
	// Store a config pointing at storedSrv.
	_ = mem.SaveWebhookTargetConfig(ctx, store.WebhookTargetConfig{
		ID: "cfg-o", EntityType: entityType,
		URL: storedSrv.URL + "/:id", Method: "POST", TimeoutSecs: 5,
	})

	// Pass a per-request target pointing at perRequestSrv.
	if _, err := tl.Reverter().RevertRevision(ctx, revID,
		revert.WithWebhookApplier(revert.WebhookTarget{
			URL:     perRequestSrv.URL + "/revert/:id",
			Method:  "PUT",
			Timeout: 5,
		}),
	); err != nil {
		t.Fatalf("RevertRevision: %v", err)
	}

	if perRequestHit == 0 {
		t.Error("per-request webhook was not called")
	}
	if storedHit > 0 {
		t.Error("stored webhook should not have been called when per-request target is present")
	}
}

// ────────────────────────────────────────────────────────────────
// Test: webhook method defaults to POST when empty
// ────────────────────────────────────────────────────────────────

func TestRevertRevision_WebhookMethodDefault(t *testing.T) {
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	mem := memory.New()
	tl, err := trailog.New(trailog.WithStore(mem))
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()

	const entityType = "product"
	const entityID = "P001"
	revID := seedRevision(t, tl, entityType, entityID)

	ctx := context.Background()
	_ = mem.SaveWebhookTargetConfig(ctx, store.WebhookTargetConfig{
		ID: "cfg-p", EntityType: entityType,
		URL: srv.URL + "/:id", Method: "", TimeoutSecs: 5,
	})

	if _, err := tl.Reverter().RevertRevision(ctx, revID); err != nil {
		t.Fatalf("RevertRevision: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("expected method POST, got %s", gotMethod)
	}
}
