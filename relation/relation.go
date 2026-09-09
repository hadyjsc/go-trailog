// Package relation manages declared and resolver-based links between entity types,
// enabling cross-entity timeline queries and safe topological ordering for reverts.
package relation

import (
	"context"
	"fmt"
	"sync"

	"github.com/hadyjsc/go-trailog/internal/types"
)

// Dependency describes the write-order constraint between parent and child entity types,
// mirroring the real FK direction so the revert engine can topologically sort writes.
type Dependency string

const (
	// ChildDependsOnParent means the child row has a FK pointing to the parent
	// (e.g. order_item.order_id -> order.id). On revert the parent must be
	// written/restored before the child.
	ChildDependsOnParent Dependency = "child_depends_on_parent"

	// Independent means no FK constraint between the two types — write order
	// is arbitrary for revert purposes.
	Independent Dependency = "independent"
)

// Declaration is a static, type-level link between two entity types.
type Declaration struct {
	ParentType   string
	ChildType    string
	RelationName string
	Dependency   Dependency
}

// Resolver is a function that, given a concrete entity, returns all related entities
// at query time (used for dynamic / runtime-computed relations).
type Resolver func(ctx context.Context, e types.Entity) ([]types.Entity, error)

// Registry holds all static declarations and dynamic resolvers.
type Registry struct {
	mu           sync.RWMutex
	declarations []Declaration
	resolvers    map[string][]Resolver // keyed by entity type
}

// New creates a new, empty Registry.
func New() *Registry {
	return &Registry{
		resolvers: make(map[string][]Resolver),
	}
}

// Declare registers a static parent→child relation between two entity types.
// dependency tells the revert engine the FK direction.
func (r *Registry) Declare(parentType, childType, relationName string, dep Dependency) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.declarations = append(r.declarations, Declaration{
		ParentType:   parentType,
		ChildType:    childType,
		RelationName: relationName,
		Dependency:   dep,
	})
}

// RegisterResolver adds a dynamic resolver for an entity type. Multiple resolvers
// can be registered for the same type; they are all called and results merged.
func (r *Registry) RegisterResolver(entityType string, fn Resolver) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolvers[entityType] = append(r.resolvers[entityType], fn)
}

// Declarations returns a snapshot of all static declarations.
func (r *Registry) Declarations() []Declaration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Declaration, len(r.declarations))
	copy(out, r.declarations)
	return out
}

// Resolve calls all dynamic resolvers registered for the given entity's type and
// returns the union of related entities. It does NOT include statically declared
// child entities by type — those are resolved structurally (see graph.go).
func (r *Registry) Resolve(ctx context.Context, e types.Entity) ([]types.Entity, error) {
	r.mu.RLock()
	fns := r.resolvers[e.Type]
	r.mu.RUnlock()

	var related []types.Entity
	for _, fn := range fns {
		entities, err := fn(ctx, e)
		if err != nil {
			return nil, fmt.Errorf("trailog/relation: resolver for %q: %w", e.Type, err)
		}
		related = append(related, entities...)
	}
	return related, nil
}

// DependencyBetween returns the Dependency between parentType and childType,
// or Independent if no declaration exists.
func (r *Registry) DependencyBetween(parentType, childType string) Dependency {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, d := range r.declarations {
		if d.ParentType == parentType && d.ChildType == childType {
			return d.Dependency
		}
	}
	return Independent
}

// ChildrenOf returns all declared child types for a given parent type.
func (r *Registry) ChildrenOf(parentType string) []Declaration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Declaration
	for _, d := range r.declarations {
		if d.ParentType == parentType {
			out = append(out, d)
		}
	}
	return out
}

// ResolvedEntity is a (Type, ID) pair returned by ResolveByTypeID so callers
// outside the internal/types package can use it without an import cycle.
type ResolvedEntity struct {
	Type string
	ID   string
}

// ResolveByTypeID is identical to Resolve but accepts (entityType, entityID)
// strings instead of a types.Entity, making it usable from packages that do
// not import internal/types directly (e.g. the timeline package).
func (r *Registry) ResolveByTypeID(ctx context.Context, entityType, entityID string) ([]ResolvedEntity, error) {
	raw, err := r.Resolve(ctx, types.Entity{Type: entityType, ID: entityID})
	if err != nil {
		return nil, err
	}
	out := make([]ResolvedEntity, len(raw))
	for i, e := range raw {
		out[i] = ResolvedEntity{Type: e.Type, ID: e.ID}
	}
	return out, nil
}
