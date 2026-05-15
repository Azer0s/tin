package codegen

// cycles.go - compile-time cycle detection for struct reference graphs.
//
// # Field ownership modifiers
//
// Tin fields carry one of three ownership flavors that the cycle checker
// uses to validate safe memory management under ARC:
//
//   - plain strong (no keyword)
//     Owning reference.  RC is incremented on assign and decremented on
//     release.  May not participate in a detected type-graph cycle unless
//     paired with at least one `weak` or `own` edge in the same SCC.
//
//   - weak
//     Non-owning reference.  RC is NOT incremented; the field does not
//     keep the target alive.  Used for back-references (parent pointers,
//     delegates) that would otherwise create a retain cycle.
//
//   - own
//     Owning tree-edge.  RC behavior is identical to a plain strong field
//     (retain on assign, release on free).  The programmer declares that
//     the runtime data reachable through this field forms a DAG / tree and
//     will never contain a cycle back to the owning struct.  The compiler
//     trusts this declaration: a detected cycle is allowed when at least
//     one edge in the SCC is `own`.
//
//     NOTE: the compiler does NOT verify the acyclicity promise at compile
//     time (that would require a full ownership / borrow-check system) or
//     at runtime (that would require an O(depth) walk on every assignment).
//     A future debug-mode build option may add the runtime check.  For now
//     `own` is an explicit programmer contract: violating it (e.g. writing
//     `node.children own= [node]`) produces a memory leak, just as manually
//     creating a strong-reference cycle would in any ARC language.
//
// # Cycle validation rules
//
// Tarjan's SCC algorithm finds every set of mutually-referencing structs.
// For each SCC that contains a cycle the following must hold:
//
//   - At least one weak edge  OR  at least one own edge:
//     Without either, every reference is a plain strong owner and the
//     runtime is guaranteed to leak (neither node can ever reach RC == 0).
//
//   - At least one strong edge (plain strong OR own):
//     A cycle made entirely of weak edges has no owner; the referenced
//     objects could be freed while the weak pointers still exist.
//
// Equivalently, a cycle is rejected only when:
//   - all edges are plain strong (no weak, no own)  -> error: need weak or own
//   - all edges are weak                            -> error: need a strong owner
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
	to        string // target struct name
	fieldName string // source field that established this edge
	isWeak    bool
	isOwn     bool
}

// checkStructCycles validates ownership invariants across all concrete struct
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
					edges = append(edges, cycleEdge{to: target, fieldName: f.Name, isWeak: f.IsWeak, isOwn: f.IsOwn})
				}

				continue
			}

			edges = append(edges, cycleEdge{to: target, fieldName: f.Name, isWeak: f.IsWeak, isOwn: f.IsOwn})
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

		// Count edge kinds within the SCC.
		sccSet := make(map[string]bool, len(scc))
		for _, n := range scc {
			sccSet[n] = true
		}

		strongCount := 0 // plain strong (neither weak nor own)
		weakCount := 0
		ownCount := 0

		for _, n := range scc {
			for _, e := range adj[n] {
				if !sccSet[e.to] {
					continue
				}

				switch {
				case e.isWeak:
					weakCount++
				case e.isOwn:
					ownCount++
				default:
					strongCount++
				}
			}
		}

		cycle := strings.Join(scc, " -> ")

		// A cycle is only safe when there is at least one cycle-breaking
		// edge (weak) or an explicit ownership declaration (own).
		if weakCount == 0 && ownCount == 0 {
			// Suggest a concrete candidate: name the first plain-strong
			// edge in the SCC.  The user can then `weak` or `own` it.
			suggestion := ""

			for _, n := range scc {
				for _, e := range adj[n] {
					if sccSet[e.to] && !e.isWeak && !e.isOwn && e.fieldName != "" {
						suggestion = fmt.Sprintf(
							"\n\thint: marking %s.%s as `weak` (cycle-breaking, non-owning) "+
								"or `own` (owning, kept alive while %s is alive) breaks the cycle",
							n, e.fieldName, n)

						break
					}
				}

				if suggestion != "" {
					break
				}
			}

			return fmt.Errorf(
				"reference cycle detected: %s\n"+
					"\tat least one field in the cycle must be marked `weak` or `own`%s",
				cycle, suggestion)
		}

		// A cycle must have at least one strong owner so objects are not
		// freed while references still exist.
		if strongCount == 0 && ownCount == 0 {
			return fmt.Errorf(
				"all fields in reference cycle are weak: %s\n"+
					"\tat least one field in the cycle must be strong (`own` or plain strong)",
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
