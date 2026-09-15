// Package revert implements the point-in-time restore engine (§7.7, §11 of TRD).
// A revert is git-revert semantics: it creates a NEW revision that undoes the
// target, leaving full history intact.
package revert

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hadyjsc/go-trailog/relation"
	"github.com/hadyjsc/go-trailog/store"
)

// Strategy controls how the reverter handles fields changed after the target revision.
type Strategy string

const (
	// StrategyBlockOnConflict (default) — stops and reports conflicts before writing anything.
	StrategyBlockOnConflict Strategy = "block"
	// StrategyFieldLevel — only restores fields not changed since the target revision.
	StrategyFieldLevel Strategy = "field_level"
	// StrategyForceOverwrite — restores all fields regardless of later changes.
	StrategyForceOverwrite Strategy = "force"
)

// ApplyMode controls whether the reverter writes data back to the application tables.
type ApplyMode int

const (
	// ApplyModeWrite (default) — call repo.Save / repo.Delete / RevertApplier.
	// Returns an error if no repository or applier is registered for the entity type.
	ApplyModeWrite ApplyMode = iota
	// ApplyModeSkip — only persist the audit revision; skip all application-table writes.
	// Use this when the reverter is running inside a standalone audit service that has
	// no access to the application's database schema.
	ApplyModeSkip
	// ApplyModeWebhook — POST the snapshot to an external URL supplied by the caller.
	// Set via WithWebhookApplier. The standalone audit service uses this so the main
	// application handles its own write logic.
	ApplyModeWebhook
)

// WebhookTarget describes the external endpoint that should receive the revert payload.
type WebhookTarget struct {
	// URL is the endpoint of the main service, e.g. "http://localhost:3000/master/banks/:id".
	// The literal ":id" placeholder (if present) is replaced with the entity ID at call time.
	URL string `json:"url"`
	// Method is the HTTP method to use: "POST", "PUT", or "PATCH" (default "POST").
	Method string `json:"method"`
	// Auth is sent as the Authorization header value verbatim (e.g. "Bearer <token>",
	// "ApiKey <secret>", or just a raw secret key). Leave empty to send no auth header.
	Auth string `json:"auth"`
	// Timeout is the HTTP call timeout in seconds (default 30).
	Timeout int `json:"timeout"`
}

// WebhookPayload is the body sent to the WebhookTarget URL.
type WebhookPayload struct {
	// RevisionID is the ID of the newly created revert revision in the audit store.
	RevisionID string `json:"revision_id"`
	// EntityType and EntityID identify the entity being reverted.
	EntityType string `json:"entity_type"`
	EntityID   string `json:"entity_id"`
	// Op is the logical operation: "restore_update", "restore_create", or "restore_delete".
	Op string `json:"op"`
	// ToState is the snapshot the main service should write. Nil for restore_delete.
	ToState map[string]any `json:"to_state,omitempty"`
	// ChangedFields lists only the fields that differ between FromState and ToState.
	ChangedFields []string `json:"changed_fields,omitempty"`
}

// RevertOption configures a revert call.
type RevertOption func(*revertOptions)

type revertOptions struct {
	strategy      Strategy
	cascade       int // relation hops to cascade revert to related entities
	reason        string
	applyMode     ApplyMode
	webhookTarget *WebhookTarget
}

// WithStrategy sets the conflict-resolution strategy.
func WithStrategy(s Strategy) RevertOption {
	return func(o *revertOptions) { o.strategy = s }
}

// WithCascade also reverts related entities via the RelationRegistry up to depth hops.
func WithCascade(depth int) RevertOption {
	return func(o *revertOptions) { o.cascade = depth }
}

// WithReason sets the reason/message stored on the new revert revision.
func WithReason(msg string) RevertOption {
	return func(o *revertOptions) { o.reason = msg }
}

// WithSkipApply puts the reverter into audit-only mode: the revert revision is
// persisted in the audit store, but no writes are made to the application tables.
// This is the correct mode when the reverter is running inside a standalone audit
// service that has no registered EntityRepository or RevertApplier for the entity type.
func WithSkipApply() RevertOption {
	return func(o *revertOptions) { o.applyMode = ApplyModeSkip }
}

// WithWebhookApplier puts the reverter into webhook mode: after persisting the audit
// revision it calls the target URL with a WebhookPayload so the main service can
// execute its own write logic. The revert is considered successful when the webhook
// returns HTTP 2xx; any other status or network error aborts the revert.
func WithWebhookApplier(t WebhookTarget) RevertOption {
	return func(o *revertOptions) {
		o.applyMode = ApplyModeWebhook
		o.webhookTarget = &t
	}
}

// ────────────────────────────────────────────────────────────────
// Plan types
// ────────────────────────────────────────────────────────────────

// RevertPlan is the dry-run result from PreviewRevert.
type RevertPlan struct {
	TargetRevisionID string
	Actions          []RevertAction
	Conflicts        []Conflict
}

// RevertAction describes one entity that would change during the revert.
type RevertAction struct {
	Entity      store.EntityChange // original change being undone
	Op          string             // "restore_update" | "restore_delete" | "restore_create"
	FromState   map[string]any     // current values
	ToState     map[string]any     // values it will become
	HasConflict bool
}

// Conflict describes a field that changed after the target revision.
type Conflict struct {
	EntityType   string
	EntityID     string
	Field        string
	TargetValue  any
	CurrentValue any
	ChangedBy    []string // revision IDs that touched this field after the target
	// Reason is a human-readable explanation. Set for structural conflicts
	// (e.g. create-order violation); empty for ordinary field conflicts.
	Reason string
}

// ────────────────────────────────────────────────────────────────
// EntityRepository — must be registered for each entity type to
// enable actual data writes during a revert.
// ────────────────────────────────────────────────────────────────

// EntityRepository is the adapter that lets the reverter write data back to
// the real application tables.
type EntityRepository interface {
	Load(ctx context.Context, id string) (map[string]any, error)
	Save(ctx context.Context, id string, entity map[string]any) error
	Delete(ctx context.Context, id string) error
}

// RevertContext carries all the information the reverter has about a single
// entity revert operation. It is passed to a RevertApplier so the caller can
// translate the snapshot into a precise write against their own table schema.
type RevertContext struct {
	// EntityType and EntityID identify the row being reverted.
	EntityType string
	EntityID   string

	// Op is the logical revert operation:
	//   "restore_update" — undo an update, write ToState back.
	//   "restore_create" — undo a delete, re-insert ToState.
	//   "restore_delete" — undo a create, delete the row.
	Op string

	// FromState is the current snapshot stored in the audit log (the state
	// the row is in right now, before the revert is applied).
	FromState map[string]any

	// ToState is the snapshot the reverter wants to apply. It reflects any
	// conflict-resolution filtering (StrategyFieldLevel) already applied.
	// For Op == "restore_delete" this will be nil.
	ToState map[string]any

	// ChangedFields contains the field names that are actually being reverted
	// (i.e. the fields that differ between FromState and ToState after conflict
	// filtering). Useful for building a targeted UPDATE SET clause.
	ChangedFields []string
}

// RevertApplier is a callback that applies a revert operation to the real
// application table. Register one per entity type via
// RepositoryRegistry.RegisterApplier when the default repo.Save / repo.Delete
// is not sufficient (e.g. different column names, required transformations,
// extra audit columns that must be updated alongside the revert).
//
// The applier must return nil on success. Returning an error aborts the revert.
//
// If both a RevertApplier and an EntityRepository are registered for the same
// entity type, the RevertApplier takes precedence for write operations; the
// EntityRepository is still used for Load.
type RevertApplier func(ctx context.Context, rc RevertContext) error

// RepositoryRegistry maps entity types to their EntityRepository adapters
// and optional RevertApplier callbacks.
type RepositoryRegistry struct {
	repos    map[string]EntityRepository
	appliers map[string]RevertApplier
}

// NewRepositoryRegistry creates an empty registry.
func NewRepositoryRegistry() *RepositoryRegistry {
	return &RepositoryRegistry{
		repos:    make(map[string]EntityRepository),
		appliers: make(map[string]RevertApplier),
	}
}

// Register associates an entity type with its repository adapter.
func (r *RepositoryRegistry) Register(entityType string, repo EntityRepository) {
	r.repos[entityType] = repo
}

// RegisterApplier registers a custom RevertApplier for an entity type.
// The applier is called instead of repo.Save / repo.Delete when applying a
// revert, giving the caller full control over how snapshot data maps to their
// table schema.
func (r *RepositoryRegistry) RegisterApplier(entityType string, fn RevertApplier) {
	r.appliers[entityType] = fn
}

// Get returns the repository for an entity type, or nil if not registered.
func (r *RepositoryRegistry) Get(entityType string) EntityRepository {
	return r.repos[entityType]
}

// GetApplier returns the RevertApplier for an entity type, or nil if none is registered.
func (r *RepositoryRegistry) GetApplier(entityType string) RevertApplier {
	return r.appliers[entityType]
}

// ────────────────────────────────────────────────────────────────
// Reverter
// ────────────────────────────────────────────────────────────────

// Reverter orchestrates preview and execution of reverts.
type Reverter struct {
	store store.Store
	repos *RepositoryRegistry
	rel   *relation.Registry
}

// New creates a Reverter wired to the given store, repository registry, and
// relation registry.
func New(s store.Store, repos *RepositoryRegistry, rel *relation.Registry) *Reverter {
	return &Reverter{store: s, repos: repos, rel: rel}
}

// PreviewRevert is a dry-run that shows exactly what would change and any
// conflicts, without writing anything.
func (rv *Reverter) PreviewRevert(ctx context.Context, revisionID string, opts ...RevertOption) (*RevertPlan, error) {
	o := applyOpts(opts)
	target, err := rv.store.GetRevision(ctx, revisionID)
	if err != nil {
		return nil, fmt.Errorf("trailog/revert: load target revision: %w", err)
	}

	plan := &RevertPlan{TargetRevisionID: revisionID}
	for _, ec := range target.Changes {
		current, err := rv.store.LatestSnapshot(ctx, ec.EntityType, ec.EntityID)
		if err != nil {
			return nil, fmt.Errorf("trailog/revert: latest snapshot for %s:%s: %w", ec.EntityType, ec.EntityID, err)
		}

		op := inverseOp(ec.Op)
		action := RevertAction{
			Entity:    ec,
			Op:        op,
			FromState: current,
			ToState:   ec.SnapshotBefore,
		}

		// If this entity was created in the target revision, check whether any
		// later revisions have touched it. Reverting a create (which would delete
		// the entity) is only safe when it is the most recent audit record —
		// i.e. no updates or deletes have happened after the create. If later
		// changes exist the caller must revert from the latest change backwards,
		// one revision at a time, until they reach the create.
		if ec.Op == "create" {
			laterChanges, err := rv.store.GetEntityChanges(ctx, ec.EntityType, ec.EntityID)
			if err != nil {
				return nil, fmt.Errorf("trailog/revert: check later changes for %s:%s: %w", ec.EntityType, ec.EntityID, err)
			}
			// GetEntityChanges returns newest-first; any entry whose RevisionID differs
			// from the target means the entity was touched after the create.
			var laterRevIDs []string
			for _, lc := range laterChanges {
				if lc.RevisionID != revisionID {
					laterRevIDs = append(laterRevIDs, lc.RevisionID)
				}
			}
			if len(laterRevIDs) > 0 {
				action.HasConflict = true
				plan.Conflicts = append(plan.Conflicts, Conflict{
					EntityType:   ec.EntityType,
					EntityID:     ec.EntityID,
					Field:        "*",
					TargetValue:  nil,
					CurrentValue: current,
					ChangedBy:    laterRevIDs,
					Reason:       fmt.Sprintf("cannot revert create for %s:%s — %d later revision(s) exist; revert from the most recent change first", ec.EntityType, ec.EntityID, len(laterRevIDs)),
				})
			}
		}

		// Detect field-level conflicts: fields changed after the target revision.
		conflicts, err := rv.detectConflicts(ctx, ec, target.OccurredAt)
		if err != nil {
			return nil, err
		}
		if len(conflicts) > 0 {
			action.HasConflict = true
			plan.Conflicts = append(plan.Conflicts, conflicts...)
		}

		_ = o // strategy used only during execute
		plan.Actions = append(plan.Actions, action)
	}
	return plan, nil
}

// RevertRevision executes the revert of an entire multi-table revision.
func (rv *Reverter) RevertRevision(ctx context.Context, revisionID string, opts ...RevertOption) (*store.Revision, error) {
	o := applyOpts(opts)

	plan, err := rv.PreviewRevert(ctx, revisionID, opts...)
	if err != nil {
		return nil, err
	}

	if len(plan.Conflicts) > 0 && o.strategy == StrategyBlockOnConflict {
		return nil, fmt.Errorf("trailog/revert: %d conflict(s) found — use WithStrategy(StrategyFieldLevel) or StrategyForceOverwrite to proceed: %v",
			len(plan.Conflicts), conflictSummary(plan.Conflicts))
	}

	// Create-order conflicts (Field == "*") are structural and can never be
	// bypassed — even StrategyForceOverwrite cannot delete an entity that has
	// later audit records, because that would silently orphan audit history.
	for _, c := range plan.Conflicts {
		if c.Field == "*" {
			return nil, fmt.Errorf("trailog/revert: %s", c.Reason)
		}
	}

	// Topologically sort entities so FK-safe write order is guaranteed.
	entities := extractEntities(plan.Actions)
	sorted, err := topoSort(entities, rv.rel)
	if err != nil {
		return nil, fmt.Errorf("trailog/revert: topo sort: %w", err)
	}

	// Apply each entity in sorted order.
	newRevID := uuid.New().String()
	newRev := store.Revision{
		ID:            newRevID,
		CorrelationID: newRevID,
		Action:        "revert",
		Reason:        o.reason,
		OccurredAt:    time.Now().UTC(),
	}
	if newRev.Reason == "" {
		newRev.Reason = fmt.Sprintf("Reverted revision %s", revisionID)
	}

	var newChanges []store.EntityChange

	for _, e := range sorted {
		action := actionForEntity(plan.Actions, e.Type, e.ID)
		if action == nil {
			continue
		}

		toState := action.ToState
		if o.strategy == StrategyFieldLevel && action.HasConflict {
			// Only restore fields not touched since target revision.
			toState = filterUntouched(action.ToState, action.FromState, plan.Conflicts, e.Type, e.ID)
		}

		if err := rv.applyRevert(ctx, e.Type, e.ID, action.Op, action.FromState, toState, o.applyMode, o.webhookTarget, newRevID); err != nil {
			return nil, err
		}

		ecID := uuid.New().String()
		newChanges = append(newChanges, store.EntityChange{
			ID:             ecID,
			RevisionID:     newRevID,
			EntityType:     e.Type,
			EntityID:       e.ID,
			Op:             "update",
			SnapshotBefore: action.FromState,
			SnapshotAfter:  toState,
		})
	}

	newRev.Changes = newChanges

	// Hash chaining (tamper-evidence).
	prevHash, _ := rv.store.LastRevisionHash(ctx)
	newRev.PrevHash = prevHash
	newRev.Hash = computeHash(newRev)

	if err := rv.store.SaveRevision(ctx, newRev); err != nil {
		return nil, fmt.Errorf("trailog/revert: save revert revision: %w", err)
	}

	hadConflicts := len(plan.Conflicts) > 0
	rl := store.RevertLog{
		ID:               uuid.New().String(),
		RevertRevisionID: newRevID,
		TargetRevisionID: revisionID,
		Strategy:         string(o.strategy),
		HadConflicts:     hadConflicts,
	}
	if hadConflicts {
		rl.ConflictDetail = map[string]any{"conflicts": plan.Conflicts}
	}
	if err := rv.store.SaveRevertLog(ctx, rl); err != nil {
		return nil, fmt.Errorf("trailog/revert: save revert log: %w", err)
	}

	return &newRev, nil
}

// RevertEntity reverts a single entity to its state as of a specific revision.
func (rv *Reverter) RevertEntity(ctx context.Context, entityType, entityID, toRevisionID string, opts ...RevertOption) (*store.Revision, error) {
	o := applyOpts(opts)

	targetRev, err := rv.store.GetRevision(ctx, toRevisionID)
	if err != nil {
		return nil, fmt.Errorf("trailog/revert: load target revision: %w", err)
	}

	var targetEC *store.EntityChange
	for _, ec := range targetRev.Changes {
		if ec.EntityType == entityType && ec.EntityID == entityID {
			ecCopy := ec
			targetEC = &ecCopy
			break
		}
	}
	if targetEC == nil {
		return nil, fmt.Errorf("trailog/revert: entity %s:%s not found in revision %s", entityType, entityID, toRevisionID)
	}

	current, err := rv.store.LatestSnapshot(ctx, entityType, entityID)
	if err != nil {
		return nil, err
	}

	// Guard: if the target change is a create, block revert when later changes exist.
	if targetEC.Op == "create" {
		laterChanges, err := rv.store.GetEntityChanges(ctx, entityType, entityID)
		if err != nil {
			return nil, fmt.Errorf("trailog/revert: check later changes for %s:%s: %w", entityType, entityID, err)
		}
		var laterRevIDs []string
		for _, lc := range laterChanges {
			if lc.RevisionID != toRevisionID {
				laterRevIDs = append(laterRevIDs, lc.RevisionID)
			}
		}
		if len(laterRevIDs) > 0 {
			return nil, fmt.Errorf(
				"trailog/revert: cannot revert create for %s:%s — %d later revision(s) exist; revert from the most recent change first",
				entityType, entityID, len(laterRevIDs),
			)
		}
	}

	toState := targetEC.SnapshotBefore
	if o.strategy == StrategyFieldLevel {
		conflicts, err := rv.detectConflicts(ctx, *targetEC, targetRev.OccurredAt)
		if err != nil {
			return nil, err
		}
		if len(conflicts) > 0 {
			toState = filterUntouched(toState, current, conflicts, entityType, entityID)
		}
	}

	newRevID := uuid.New().String()

	if err := rv.applyRevert(ctx, entityType, entityID, inverseOp(targetEC.Op), current, toState, o.applyMode, o.webhookTarget, newRevID); err != nil {
		return nil, err
	}
	reason := o.reason
	if reason == "" {
		reason = fmt.Sprintf("Reverted %s:%s to state at revision %s", entityType, entityID, toRevisionID)
	}
	newRev := store.Revision{
		ID:            newRevID,
		CorrelationID: newRevID,
		Action:        "revert",
		Reason:        reason,
		OccurredAt:    time.Now().UTC(),
		Changes: []store.EntityChange{
			{
				ID:             uuid.New().String(),
				RevisionID:     newRevID,
				EntityType:     entityType,
				EntityID:       entityID,
				Op:             "update",
				SnapshotBefore: current,
				SnapshotAfter:  toState,
			},
		},
	}
	prevHash, _ := rv.store.LastRevisionHash(ctx)
	newRev.PrevHash = prevHash
	newRev.Hash = computeHash(newRev)

	if err := rv.store.SaveRevision(ctx, newRev); err != nil {
		return nil, fmt.Errorf("trailog/revert: save revert revision: %w", err)
	}
	if err := rv.store.SaveRevertLog(ctx, store.RevertLog{
		ID:               uuid.New().String(),
		RevertRevisionID: newRevID,
		TargetRevisionID: toRevisionID,
		Strategy:         string(o.strategy),
	}); err != nil {
		return nil, err
	}
	return &newRev, nil
}

// ────────────────────────────────────────────────────────────────
// helpers
// ────────────────────────────────────────────────────────────────

// applyRevert dispatches a single entity revert write. Precedence:
//
//  1. ApplyModeSkip                                     → no-op (audit-only)
//  2. ApplyModeWebhook                                  → POST WebhookPayload to target URL
//  3. RevertApplier registered for the entity type      → called with full RevertContext
//  4. EntityRepository registered for the entity type   → Save / Delete called directly
//  5. Neither registered                                → error
func (rv *Reverter) applyRevert(ctx context.Context, entityType, entityID, op string, fromState, toState map[string]any, mode ApplyMode, webhook *WebhookTarget, revisionID string) error {
	// Audit-only mode: skip all application-table writes.
	if mode == ApplyModeSkip {
		return nil
	}

	// Webhook mode: call the main service's API with the snapshot payload.
	if mode == ApplyModeWebhook {
		if webhook == nil {
			return fmt.Errorf("trailog/revert: webhook mode selected but no WebhookTarget provided")
		}
		return rv.callWebhook(ctx, webhook, WebhookPayload{
			RevisionID:    revisionID,
			EntityType:    entityType,
			EntityID:      entityID,
			Op:            op,
			ToState:       toState,
			ChangedFields: changedFields(fromState, toState),
		})
	}

	applier := rv.repos.GetApplier(entityType)
	if applier != nil {
		rc := RevertContext{
			EntityType:    entityType,
			EntityID:      entityID,
			Op:            op,
			FromState:     fromState,
			ToState:       toState,
			ChangedFields: changedFields(fromState, toState),
		}
		if err := applier(ctx, rc); err != nil {
			return fmt.Errorf("trailog/revert: applier for %s:%s (%s): %w", entityType, entityID, op, err)
		}
		return nil
	}

	repo := rv.repos.Get(entityType)
	if repo == nil {
		return fmt.Errorf("trailog/revert: no repository or applier registered for entity type %q — register one with WithRepository or WithRevertApplier", entityType)
	}

	var err error
	switch op {
	case "restore_delete": // undo a create → delete the row
		err = repo.Delete(ctx, entityID)
	case "restore_create", "restore_update": // undo a delete/update → write old state
		err = repo.Save(ctx, entityID, toState)
	}
	if err != nil {
		return fmt.Errorf("trailog/revert: apply %s on %s:%s: %w", op, entityType, entityID, err)
	}
	return nil
}

// callWebhook sends a WebhookPayload to the target URL and returns an error if
// the response is not HTTP 2xx or the request fails.
func (rv *Reverter) callWebhook(ctx context.Context, target *WebhookTarget, payload WebhookPayload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("trailog/revert: marshal webhook payload: %w", err)
	}

	// Replace ":id" placeholder in URL with the actual entity ID.
	url := strings.ReplaceAll(target.URL, ":id", payload.EntityID)

	method := strings.ToUpper(target.Method)
	if method == "" {
		method = http.MethodPost
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("trailog/revert: build webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if target.Auth != "" {
		req.Header.Set("Authorization", target.Auth)
	}

	timeout := time.Duration(target.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	client := &http.Client{Timeout: timeout}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("trailog/revert: webhook call to %s failed: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("trailog/revert: webhook %s returned non-2xx status %d", url, resp.StatusCode)
	}
	return nil
}

// changedFields returns the field names that differ between fromState and toState.
// Used to populate RevertContext.ChangedFields so appliers can build targeted UPDATE queries.
func changedFields(from, to map[string]any) []string {
	if to == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var fields []string
	add := func(k string) {
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			fields = append(fields, k)
		}
	}
	for k, toVal := range to {
		fromVal, exists := from[k]
		if !exists || fmt.Sprintf("%v", fromVal) != fmt.Sprintf("%v", toVal) {
			add(k)
		}
	}
	return fields
}

func applyOpts(opts []RevertOption) *revertOptions {
	o := &revertOptions{strategy: StrategyBlockOnConflict}
	for _, fn := range opts {
		fn(o)
	}
	return o
}

func inverseOp(op string) string {
	switch op {
	case "create":
		return "restore_delete"
	case "delete":
		return "restore_create"
	default:
		return "restore_update"
	}
}

// detectConflicts checks whether any fields in ec were changed after occurredAt.
func (rv *Reverter) detectConflicts(ctx context.Context, ec store.EntityChange, occurredAt time.Time) ([]Conflict, error) {
	changes, err := rv.store.GetEntityChanges(ctx, ec.EntityType, ec.EntityID)
	if err != nil {
		return nil, fmt.Errorf("trailog/revert: detect conflicts: %w", err)
	}

	// Gather later revisions' field changes.
	laterFields := map[string][]string{} // fieldName → []revisionID
	for _, later := range changes {
		laterRev, err := rv.store.GetRevision(ctx, later.RevisionID)
		if err != nil {
			continue
		}
		if !laterRev.OccurredAt.After(occurredAt) {
			continue
		}
		for _, fd := range later.Fields {
			laterFields[fd.FieldName] = append(laterFields[fd.FieldName], later.RevisionID)
		}
	}

	// Cross-reference with the fields being reverted.
	var conflicts []Conflict
	for _, fd := range ec.Fields {
		if revIDs, touched := laterFields[fd.FieldName]; touched {
			current, _ := rv.store.LatestSnapshot(ctx, ec.EntityType, ec.EntityID)
			var currentVal any
			if current != nil {
				currentVal = current[fd.FieldName]
			}
			conflicts = append(conflicts, Conflict{
				EntityType:   ec.EntityType,
				EntityID:     ec.EntityID,
				Field:        fd.FieldName,
				TargetValue:  fd.OldValue,
				CurrentValue: currentVal,
				ChangedBy:    revIDs,
			})
		}
	}
	return conflicts, nil
}

func filterUntouched(toState, fromState map[string]any, conflicts []Conflict, entityType, entityID string) map[string]any {
	conflictFields := map[string]struct{}{}
	for _, c := range conflicts {
		if c.EntityType == entityType && c.EntityID == entityID {
			conflictFields[c.Field] = struct{}{}
		}
	}
	result := make(map[string]any, len(fromState))
	// Start with current state as baseline.
	for k, v := range fromState {
		result[k] = v
	}
	// Only restore fields that haven't been touched since target.
	for k, v := range toState {
		if _, conflict := conflictFields[k]; !conflict {
			result[k] = v
		}
	}
	return result
}

type entityRef struct {
	Type string
	ID   string
}

func extractEntities(actions []RevertAction) []entityRef {
	var out []entityRef
	for _, a := range actions {
		out = append(out, entityRef{Type: a.Entity.EntityType, ID: a.Entity.EntityID})
	}
	return out
}

func actionForEntity(actions []RevertAction, entityType, entityID string) *RevertAction {
	for i := range actions {
		if actions[i].Entity.EntityType == entityType && actions[i].Entity.EntityID == entityID {
			return &actions[i]
		}
	}
	return nil
}

func conflictSummary(conflicts []Conflict) string {
	s := ""
	for _, c := range conflicts {
		s += fmt.Sprintf("[%s:%s.%s] ", c.EntityType, c.EntityID, c.Field)
	}
	return s
}

func computeHash(rev store.Revision) string {
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

// topoSort wraps relation/graph.TopoSort for the entityRef type used here.
func topoSort(entities []entityRef, reg *relation.Registry) ([]entityRef, error) {
	// Build a simple adjacency map: parentIdx → []childIdx
	n := len(entities)
	inDegree := make([]int, n)
	adj := make([][]int, n)

	for i, e := range entities {
		for j, other := range entities {
			if i == j {
				continue
			}
			dep := reg.DependencyBetween(e.Type, other.Type)
			if dep == relation.ChildDependsOnParent {
				adj[i] = append(adj[i], j)
				inDegree[j]++
			}
		}
	}

	var queue []int
	for i, d := range inDegree {
		if d == 0 {
			queue = append(queue, i)
		}
	}

	var sorted []entityRef
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		sorted = append(sorted, entities[cur])
		for _, next := range adj[cur] {
			inDegree[next]--
			if inDegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}

	if len(sorted) != n {
		return nil, fmt.Errorf("cycle in entity dependency graph")
	}
	return sorted, nil
}
