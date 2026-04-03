package codegen

// cycles.go - compile-time cycle detection for struct reference graphs.
//
// Rule: every cycle in the struct type graph must have at least one strong
// edge and at least one weak edge.
//   - No weak edge    -> error: cycle must have at least one `weak` field
//   - All edges weak  -> error: cycle must have at least one strong field
//
// The graph is built from each concrete (non-generic) struct's fields.
// Edges are added for all direct, array, and pointer references to other
// named struct types, resolving type aliases as needed.

import (
	"fmt"
	"strings"

	"github.com/Azer0s/tin/ast"
)

// cycleEdge describes a single struct-field reference in the type graph.
type cycleEdge struct {
	to     string // target struct name
	isWeak bool
}

// checkStructCycles validates weak/strong invariants across all concrete struct
// declarations.  It is called once after the preregister pass so all types are
// known.  Returns a descriptive error on the first violation found.
func (cg *CodeGen) checkStructCycles(decls []*ast.StructDecl) error {
	// Build adjacency list: structName -> []cycleEdge
	adj := make(map[string][]cycleEdge, len(decls))

	for _, d := range decls {
		edges := adj[d.Name] // may already exist (deduplication not needed)
		for _, f := range d.Fields {
			target := cg.structNameFromTinType(f.Type)
			if target == "" || target == d.Name {
				// Self-references and non-struct types are handled separately;
				// direct self-reference (Node -> Node) is still added so the
				// SCC algorithm can find it.
				if target == d.Name {
					edges = append(edges, cycleEdge{to: target, isWeak: f.IsWeak})
				}

				continue
			}

			edges = append(edges, cycleEdge{to: target, isWeak: f.IsWeak})
		}

		adj[d.Name] = edges
	}

	// Tarjan's SCC algorithm.
	index := make(map[string]int)
	lowlink := make(map[string]int)
	onStack := make(map[string]bool)

	var stack []string

	idx := 0

	var sccs [][]string

	var strongConnect func(v string)

	strongConnect = func(v string) {
		index[v] = idx
		lowlink[v] = idx
		idx++

		stack = append(stack, v)
		onStack[v] = true

		for _, e := range adj[v] {
			w := e.to
			if _, seen := index[w]; !seen {
				strongConnect(w)

				if lowlink[w] < lowlink[v] {
					lowlink[v] = lowlink[w]
				}
			} else if onStack[w] {
				if index[w] < lowlink[v] {
					lowlink[v] = index[w]
				}
			}
		}

		if lowlink[v] == index[v] {
			var scc []string

			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				scc = append(scc, w)

				if w == v {
					break
				}
			}

			sccs = append(sccs, scc)
		}
	}

	for name := range adj {
		if _, seen := index[name]; !seen {
			strongConnect(name)
		}
	}

	// Validate each SCC with more than one member, or single-member with
	// a self-loop (direct self-reference).
	for _, scc := range sccs {
		isCycle := len(scc) > 1

		if !isCycle && len(scc) == 1 {
			// Check for self-loop.
			for _, e := range adj[scc[0]] {
				if e.to == scc[0] {
					isCycle = true

					break
				}
			}
		}

		if !isCycle {
			continue
		}

		// Count strong vs weak edges within the SCC.
		sccSet := make(map[string]bool, len(scc))
		for _, n := range scc {
			sccSet[n] = true
		}

		strongCount := 0
		weakCount := 0

		for _, n := range scc {
			for _, e := range adj[n] {
				if !sccSet[e.to] {
					continue
				}

				if e.isWeak {
					weakCount++
				} else {
					strongCount++
				}
			}
		}

		cycle := strings.Join(scc, " -> ")

		if weakCount == 0 {
			return fmt.Errorf(
				"reference cycle detected: %s\n"+
					"\tat least one field in the cycle must be marked `weak`",
				cycle)
		}

		if strongCount == 0 {
			return fmt.Errorf(
				"all fields in reference cycle are weak: %s\n"+
					"\tat least one field in the cycle must be strong",
				cycle)
		}
	}

	return nil
}

// structNameFromTinType returns the concrete struct name referenced by a Tin
// TypeExpr, or "" if the type does not refer to a known struct.  It resolves
// type aliases and unwraps array/pointer wrappers one level deep.
func (cg *CodeGen) structNameFromTinType(t ast.TypeExpr) string {
	if t == nil {
		return ""
	}

	switch n := t.(type) {
	case *ast.SimpleType:
		name := n.Name
		// Resolve type aliases.
		if alias, ok := cg.typeAliases[name]; ok {
			return cg.structNameFromTinType(alias)
		}

		if _, ok := cg.structTypes[name]; ok {
			return name
		}

		return ""

	case *ast.ArrayType:
		return cg.structNameFromTinType(n.Elem)

	case *ast.PointerType:
		return cg.structNameFromTinType(n.Elem)
	}

	return ""
}
