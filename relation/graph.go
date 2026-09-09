package relation

import (
	"context"
	"fmt"

	"github.com/hadyjsc/go-trailog/internal/types"
)

// Expand performs a BFS over the relation graph starting from root, following
// declared and dynamic relations up to maxDepth hops. It returns all reachable
// related entities (excluding root itself).
//
// Used by the timeline query to widen results to related entities.
func Expand(ctx context.Context, reg *Registry, root types.Entity, maxDepth int) ([]types.Entity, error) {
	if maxDepth <= 0 {
		return nil, nil
	}

	visited := map[string]struct{}{root.String(): {}}
	queue := []types.Entity{root}
	var related []types.Entity

	for depth := 0; depth < maxDepth && len(queue) > 0; depth++ {
		var nextQueue []types.Entity

		for _, e := range queue {
			// 1. Static declarations: add all child entity *types* we know about.
			//    (Actual child entity IDs come from dynamic resolvers.)
			// 2. Dynamic resolvers: resolve concrete entity IDs.
			resolved, err := reg.Resolve(ctx, e)
			if err != nil {
				return nil, fmt.Errorf("trailog/relation/graph: expand %s: %w", e, err)
			}

			for _, r := range resolved {
				key := r.String()
				if _, seen := visited[key]; seen {
					continue
				}
				visited[key] = struct{}{}
				related = append(related, r)
				nextQueue = append(nextQueue, r)
			}
		}

		queue = nextQueue
	}

	return related, nil
}

// TopoSort sorts entities into a write-safe order for revert operations, given
// the dependency declarations in the registry.
//
// Algorithm: Kahn's topological sort where an edge A→B means "A must be
// written before B" (i.e. B child-depends-on-parent A).
// Entities with no declared dependency get written last (they're independent).
func TopoSort(entities []types.Entity, reg *Registry) ([]types.Entity, error) {
	// Build adjacency: for each entity, collect entities that depend on it.
	// Edge: parent → child  (parent written first).
	index := make(map[string]int, len(entities))
	for i, e := range entities {
		index[e.String()] = i
	}

	inDegree := make([]int, len(entities))
	adj := make([][]int, len(entities)) // adj[i] = list of indices that depend on i

	for i, e := range entities {
		for j, other := range entities {
			if i == j {
				continue
			}
			dep := reg.DependencyBetween(e.Type, other.Type)
			if dep == ChildDependsOnParent {
				// e is parent, other is child — child depends on parent → e before other
				adj[i] = append(adj[i], j)
				inDegree[j]++
			}
		}
	}

	// Kahn's BFS.
	var queue []int
	for i, d := range inDegree {
		if d == 0 {
			queue = append(queue, i)
		}
	}

	var sorted []types.Entity
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

	if len(sorted) != len(entities) {
		return nil, fmt.Errorf("trailog/relation/graph: cycle detected in entity dependency graph")
	}

	return sorted, nil
}

// _ keeps the index variable used above from triggering a lint warning.
var _ = fmt.Errorf
