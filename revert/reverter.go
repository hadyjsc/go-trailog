// Package revert implements the point-in-time restore engine (§7.7, §11 of TRD).
// A revert is git-revert semantics: it creates a NEW revision that undoes the
// target, leaving full history intact.
package revert

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/trailog/trailog/relation"
	"github.com/trailog/trailog/store"
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

// RevertOption configures a revert call.
type RevertOption func(*revertOptions)

type revertOptions struct {
	strategy Strategy
	cascade  int    // relation hops to cascade revert to related entities
	reason   string
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

// RepositoryRegistry maps entity types to their EntityRepository adapters.
type RepositoryRegistry struct {
	repos map[string]EntityRepository
}

// NewRepositoryRegistry creates an empty registry.
func NewRepositoryRegistry() *RepositoryRegistry {
	return &RepositoryRegistry{repos: make(map[string]EntityRepository)}
}

// Register associates an entity type with its repository adapter.
func (r *RepositoryRegistry) Register(entityType string, repo EntityRepository) {
	r.repos[entityType] = repo
}

// Get returns the repository for an entity type, or nil if not registered.
func (r *RepositoryRegistry) Get(entityType string) EntityRepository {
	return r.repos[entityType]
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

		// Detect conflicts: fields changed after the target revision.
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

		repo := rv.repos.Get(e.Type)
		if repo == nil {
			return nil, fmt.Errorf("trailog/revert: no repository registered for entity type %q — register one with WithRepository", e.Type)
		}

		toState := action.ToState
		if o.strategy == StrategyFieldLevel && action.HasConflict {
			// Only restore fields not touched since target revision.
			toState = filterUntouched(action.ToState, action.FromState, plan.Conflicts, e.Type, e.ID)
		}

		var applyErr error
		switch action.Op {
		case "restore_delete": // undo a create → delete the row
			applyErr = repo.Delete(ctx, e.ID)
		case "restore_create", "restore_update": // undo a delete/update → save old state
			applyErr = repo.Save(ctx, e.ID, toState)
		}
		if applyErr != nil {
			return nil, fmt.Errorf("trailog/revert: apply %s on %s:%s: %w", action.Op, e.Type, e.ID, applyErr)
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

	repo := rv.repos.Get(entityType)
	if repo == nil {
		return nil, fmt.Errorf("trailog/revert: no repository registered for entity type %q", entityType)
	}

	current, err := rv.store.LatestSnapshot(ctx, entityType, entityID)
	if err != nil {
		return nil, err
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

	if err := repo.Save(ctx, entityID, toState); err != nil {
		return nil, fmt.Errorf("trailog/revert: save entity: %w", err)
	}

	newRevID := uuid.New().String()
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
