package codegen

import (
	"fmt"
	"strings"

	"github.com/Azer0s/tin/ast"
)

// hasTag returns true if the tag slice contains the given tag name (without #).
func hasTag(tags []string, name string) bool {
	for _, t := range tags {
		if t == name {
			return true
		}
	}

	return false
}

// pureBuiltins is the set of built-in functions known to be pure (no I/O,
// no mutation of external state). These are allowed inside #pure functions
// even though they are not declared in funcDecls.
var pureBuiltins = map[string]bool{
	"len":        true,
	"sizeof":     true,
	"default":    true,
	"typeof":     true,
	"traitof":    true,
	"fieldnames": true,
	"fieldtypes": true,
	"fieldtag":   true,
	"getfield":   true,
}

// isPureBuiltin returns true if name is a known-pure built-in function.
func isPureBuiltin(name string) bool {
	return pureBuiltins[name]
}

// ---------------------------------------------------------------------------
// #pure enforcement
// ---------------------------------------------------------------------------

// checkAllPureFuncs validates every #pure-tagged function in funcDecls.
// Called after the predeclaration pass so all function signatures are known.
func (cg *CodeGen) checkAllPureFuncs() error {
	for _, fd := range cg.funcDecls {
		if hasTag(fd.Tags, "pure") {
			if err := cg.checkPureBody(fd); err != nil {
				return err
			}
		}
	}

	return nil
}

// checkPureBody verifies that fn is actually pure by walking its body.
// A function is pure if it contains no echo statements, no calls to
// #sideffect or extern functions, no indirect (function-pointer) calls,
// and no calls to unverifiable external package functions -
// except inside #allow_sideffect blocks.
func (cg *CodeGen) checkPureBody(fn *ast.FuncDecl) error {
	visited := make(map[string]bool)

	return cg.walkPureNode(fn.Name, fn.Body, false, visited)
}

// walkPureNode walks an AST node looking for side-effect violations.
// fnCtx is the name of the #pure function being checked (for error messages).
// allowSideEffect is true when we're inside an #allow_sideffect block.
// visited prevents infinite recursion when following the call graph.
func (cg *CodeGen) walkPureNode(fnCtx string, node ast.Node, allowSideEffect bool, visited map[string]bool) error {
	if node == nil {
		return nil
	}

	switch v := node.(type) {
	case *ast.EchoStmt:
		if !allowSideEffect {
			return fmt.Errorf("fn %s: #pure violation - echo is a side effect", fnCtx)
		}

	case *ast.TaggedBlock:
		// #allow_sideffect block: permit side effects inside
		inner := allowSideEffect || hasTag(v.Tags, "allow_sideffect")

		return cg.walkPureNode(fnCtx, v.Body, inner, visited)

	case *ast.CallExpr:
		if !allowSideEffect {
			if err := cg.checkCallPure(fnCtx, v, visited); err != nil {
				return err
			}
		}
		// Also walk argument expressions
		for _, arg := range v.Args {
			if err := cg.walkPureNode(fnCtx, arg, allowSideEffect, visited); err != nil {
				return err
			}
		}

	case *ast.Block:
		for _, s := range v.Stmts {
			if err := cg.walkPureNode(fnCtx, s, allowSideEffect, visited); err != nil {
				return err
			}
		}

	case *ast.IfStmt:
		if err := cg.walkPureNode(fnCtx, v.Cond, allowSideEffect, visited); err != nil {
			return err
		}

		if v.Then != nil {
			if err := cg.walkPureNode(fnCtx, v.Then, allowSideEffect, visited); err != nil {
				return err
			}
		}

		for _, ei := range v.ElseIfs {
			if err := cg.walkPureNode(fnCtx, ei.Cond, allowSideEffect, visited); err != nil {
				return err
			}

			if ei.Body != nil {
				if err := cg.walkPureNode(fnCtx, ei.Body, allowSideEffect, visited); err != nil {
					return err
				}
			}
		}

		if v.Else != nil {
			if err := cg.walkPureNode(fnCtx, v.Else, allowSideEffect, visited); err != nil {
				return err
			}
		}

	case *ast.ForStmt:
		if err := cg.walkPureNode(fnCtx, v.Cond, allowSideEffect, visited); err != nil {
			return err
		}

		if err := cg.walkPureNode(fnCtx, v.Init, allowSideEffect, visited); err != nil {
			return err
		}

		if err := cg.walkPureNode(fnCtx, v.Post, allowSideEffect, visited); err != nil {
			return err
		}

		if err := cg.walkPureNode(fnCtx, v.Iter, allowSideEffect, visited); err != nil {
			return err
		}

		if v.Body != nil {
			if err := cg.walkPureNode(fnCtx, v.Body, allowSideEffect, visited); err != nil {
				return err
			}
		}

	case *ast.ReturnStmt:
		return cg.walkPureNode(fnCtx, v.Value, allowSideEffect, visited)

	case *ast.AssignStmt:
		if err := cg.walkPureNode(fnCtx, v.Target, allowSideEffect, visited); err != nil {
			return err
		}

		return cg.walkPureNode(fnCtx, v.Value, allowSideEffect, visited)

	case *ast.AugAssignStmt:
		return cg.walkPureNode(fnCtx, v.Value, allowSideEffect, visited)

	case *ast.VarDecl:
		return cg.walkPureNode(fnCtx, v.Value, allowSideEffect, visited)

	case *ast.ExprStmt:
		return cg.walkPureNode(fnCtx, v.Expr, allowSideEffect, visited)

	case *ast.BinExpr:
		if err := cg.walkPureNode(fnCtx, v.Left, allowSideEffect, visited); err != nil {
			return err
		}

		return cg.walkPureNode(fnCtx, v.Right, allowSideEffect, visited)

	case *ast.UnaryExpr:
		return cg.walkPureNode(fnCtx, v.Expr, allowSideEffect, visited)

	case *ast.TernaryExpr:
		if err := cg.walkPureNode(fnCtx, v.Cond, allowSideEffect, visited); err != nil {
			return err
		}

		if err := cg.walkPureNode(fnCtx, v.Then, allowSideEffect, visited); err != nil {
			return err
		}

		return cg.walkPureNode(fnCtx, v.Else, allowSideEffect, visited)

	case *ast.FieldAccess:
		return cg.walkPureNode(fnCtx, v.Expr, allowSideEffect, visited)

	case *ast.IndexExpr:
		if err := cg.walkPureNode(fnCtx, v.Expr, allowSideEffect, visited); err != nil {
			return err
		}

		return cg.walkPureNode(fnCtx, v.Index, allowSideEffect, visited)

	case *ast.DeferStmt:
		return cg.walkPureNode(fnCtx, v.Call, allowSideEffect, visited)

	case *ast.WhereList:
		for _, c := range v.Clauses {
			if err := cg.walkPureNode(fnCtx, c.Body, allowSideEffect, visited); err != nil {
				return err
			}
		}
	}

	return nil
}

// checkCallPure checks whether a CallExpr is pure (no side effects).
// It resolves the callee name and follows transitive calls.
// Unresolvable calls (function pointers / indirect calls) are rejected -
// their purity cannot be verified statically.
func (cg *CodeGen) checkCallPure(fnCtx string, call *ast.CallExpr, visited map[string]bool) error {
	calleeName := resolveCalleeName(call)
	if calleeName == "" {
		// Cannot resolve statically (e.g. function pointer / indirect call).
		// Purity cannot be verified - reject.
		return fmt.Errorf("fn %s: #pure violation - indirect call through function pointer is not verifiable", fnCtx)
	}

	return cg.isPureCallable(fnCtx, calleeName, visited)
}

// isPureCallable returns nil if calleeName is safe to call from a #pure function,
// or an error describing the violation. visited tracks already-checked functions
// to handle mutual recursion without infinite loops.
//
// calleeName may be:
//   - "funcName"        - top-level function
//   - "pkg::funcName"   - package-qualified function (rejected: unverifiable)
//   - ".methodName"     - method call (receiver type unknown); checked conservatively
//     by scanning all registered "Struct_methodName" entries for any #sideffect match
func (cg *CodeGen) isPureCallable(fnCtx, calleeName string, visited map[string]bool) error {
	// Method call via FieldAccess: we don't have the receiver type, so do a
	// conservative suffix search across all registered struct methods.
	if strings.HasPrefix(calleeName, ".") {
		methodName := calleeName[1:] // strip leading "."

		suffix := "_" + methodName
		for key, fd := range cg.funcDecls {
			if !strings.HasSuffix(key, suffix) {
				continue
			}
			// Found at least one struct method with this name. Check it.
			if hasTag(fd.Tags, "sideffect") {
				return fmt.Errorf("fn %s: #pure violation - calls #sideffect method %q", fnCtx, methodName)
			}

			if fd.IsExtern != "" {
				return fmt.Errorf("fn %s: #pure violation - calls extern method %q", fnCtx, methodName)
			}

			if visited[key] {
				continue
			}

			visited[key] = true
			if err := cg.walkPureNode(fnCtx, fd.Body, false, visited); err != nil {
				return err
			}
		}

		return nil
	}

	// Package-qualified call (e.g. "assert::equals"): purity of external
	// package functions cannot be verified - reject.
	if strings.Contains(calleeName, "::") {
		return fmt.Errorf("fn %s: #pure violation - cannot verify purity of package call %q", fnCtx, calleeName)
	}

	// Already checked in this traversal - avoid infinite recursion
	if visited[calleeName] {
		return nil
	}

	visited[calleeName] = true

	fd, known := cg.funcDecls[calleeName]
	if !known {
		// Check if it's a known-pure built-in.
		if isPureBuiltin(calleeName) {
			return nil
		}
		// Unknown function - purity cannot be verified.

		return fmt.Errorf("fn %s: #pure violation - cannot verify purity of %q", fnCtx, calleeName)
	}

	// Explicitly tagged #sideffect
	if hasTag(fd.Tags, "sideffect") {
		return fmt.Errorf("fn %s: #pure violation - calls #sideffect function %q", fnCtx, calleeName)
	}

	// Extern functions (all extern = side-effectful)
	if fd.IsExtern != "" {
		return fmt.Errorf("fn %s: #pure violation - calls extern function %q", fnCtx, calleeName)
	}

	// Transitively check the callee's body

	return cg.walkPureNode(fnCtx, fd.Body, false, visited)
}

// ---------------------------------------------------------------------------
// #no_recurse enforcement (transitive)
// ---------------------------------------------------------------------------

// checkAllNoRecurseFuncs validates every #no_recurse-tagged function in funcDecls.
// Detects recursion transitively through any depth of call chain.
func (cg *CodeGen) checkAllNoRecurseFuncs() error {
	for key, fd := range cg.funcDecls {
		if hasTag(fd.Tags, "no_recurse") {
			// #pure #no_recurse functions are CTFE macros - their body may contain
			// self-calls that are resolved at compile time. Skip the recursion check
			// since CTFE handles them rather than runtime calls.
			if hasTag(fd.Tags, "pure") {
				continue
			}
			// seed visited with the function itself so any path back to it is caught
			visited := map[string]bool{key: true}
			if err := cg.walkNoRecurseNode(key, fd.Body, visited); err != nil {
				return err
			}
		}
	}

	return nil
}

// walkNoRecurseNode walks an AST node searching for any call path that
// reaches targetFn. visited tracks explored functions to prevent infinite
// loops through non-recursive cycles.
func (cg *CodeGen) walkNoRecurseNode(targetFn string, node ast.Node, visited map[string]bool) error {
	if node == nil {
		return nil
	}

	switch v := node.(type) {
	case *ast.CallExpr:
		if err := cg.checkCallNoRecurse(targetFn, v, visited); err != nil {
			return err
		}

		for _, arg := range v.Args {
			if err := cg.walkNoRecurseNode(targetFn, arg, visited); err != nil {
				return err
			}
		}

	case *ast.Block:
		for _, s := range v.Stmts {
			if err := cg.walkNoRecurseNode(targetFn, s, visited); err != nil {
				return err
			}
		}

	case *ast.TaggedBlock:
		return cg.walkNoRecurseNode(targetFn, v.Body, visited)

	case *ast.IfStmt:
		if err := cg.walkNoRecurseNode(targetFn, v.Cond, visited); err != nil {
			return err
		}

		if v.Then != nil {
			if err := cg.walkNoRecurseNode(targetFn, v.Then, visited); err != nil {
				return err
			}
		}

		for _, ei := range v.ElseIfs {
			if err := cg.walkNoRecurseNode(targetFn, ei.Cond, visited); err != nil {
				return err
			}

			if ei.Body != nil {
				if err := cg.walkNoRecurseNode(targetFn, ei.Body, visited); err != nil {
					return err
				}
			}
		}

		if v.Else != nil {
			if err := cg.walkNoRecurseNode(targetFn, v.Else, visited); err != nil {
				return err
			}
		}

	case *ast.ForStmt:
		if err := cg.walkNoRecurseNode(targetFn, v.Cond, visited); err != nil {
			return err
		}

		if err := cg.walkNoRecurseNode(targetFn, v.Init, visited); err != nil {
			return err
		}

		if err := cg.walkNoRecurseNode(targetFn, v.Post, visited); err != nil {
			return err
		}

		if err := cg.walkNoRecurseNode(targetFn, v.Iter, visited); err != nil {
			return err
		}

		if v.Body != nil {
			if err := cg.walkNoRecurseNode(targetFn, v.Body, visited); err != nil {
				return err
			}
		}

	case *ast.ReturnStmt:
		return cg.walkNoRecurseNode(targetFn, v.Value, visited)

	case *ast.AssignStmt:
		if err := cg.walkNoRecurseNode(targetFn, v.Target, visited); err != nil {
			return err
		}

		return cg.walkNoRecurseNode(targetFn, v.Value, visited)

	case *ast.AugAssignStmt:
		return cg.walkNoRecurseNode(targetFn, v.Value, visited)

	case *ast.VarDecl:
		return cg.walkNoRecurseNode(targetFn, v.Value, visited)

	case *ast.ExprStmt:
		return cg.walkNoRecurseNode(targetFn, v.Expr, visited)

	case *ast.BinExpr:
		if err := cg.walkNoRecurseNode(targetFn, v.Left, visited); err != nil {
			return err
		}

		return cg.walkNoRecurseNode(targetFn, v.Right, visited)

	case *ast.UnaryExpr:
		return cg.walkNoRecurseNode(targetFn, v.Expr, visited)

	case *ast.TernaryExpr:
		if err := cg.walkNoRecurseNode(targetFn, v.Cond, visited); err != nil {
			return err
		}

		if err := cg.walkNoRecurseNode(targetFn, v.Then, visited); err != nil {
			return err
		}

		return cg.walkNoRecurseNode(targetFn, v.Else, visited)

	case *ast.FieldAccess:
		return cg.walkNoRecurseNode(targetFn, v.Expr, visited)

	case *ast.IndexExpr:
		if err := cg.walkNoRecurseNode(targetFn, v.Expr, visited); err != nil {
			return err
		}

		return cg.walkNoRecurseNode(targetFn, v.Index, visited)

	case *ast.DeferStmt:
		return cg.walkNoRecurseNode(targetFn, v.Call, visited)

	case *ast.WhereList:
		for _, c := range v.Clauses {
			if err := cg.walkNoRecurseNode(targetFn, c.Body, visited); err != nil {
				return err
			}
		}
	}

	return nil
}

// checkCallNoRecurse checks whether a single CallExpr creates a call path back
// to targetFn. Follows transitive calls through funcDecls.
func (cg *CodeGen) checkCallNoRecurse(targetFn string, call *ast.CallExpr, visited map[string]bool) error {
	calleeName := resolveCalleeName(call)
	if calleeName == "" {
		// Indirect call - cannot trace; conservatively allow.
		return nil
	}

	// Method call via FieldAccess: suffix search across all struct methods.
	if strings.HasPrefix(calleeName, ".") {
		methodName := calleeName[1:]

		suffix := "_" + methodName
		for key, fd := range cg.funcDecls {
			if !strings.HasSuffix(key, suffix) {
				continue
			}

			if key == targetFn {
				return fmt.Errorf("fn %s: #no_recurse violation - function calls itself", targetFn)
			}

			if visited[key] {
				continue
			}

			visited[key] = true
			if err := cg.walkNoRecurseNode(targetFn, fd.Body, visited); err != nil {
				return err
			}
		}

		return nil
	}

	// Strip module qualifier for lookup (e.g. "pkg::fn" -> "fn")
	lookupName := calleeName
	if idx := strings.LastIndex(calleeName, "::"); idx >= 0 {
		lookupName = calleeName[idx+2:]
	}

	// Direct match: the callee IS the no_recurse function.
	if lookupName == targetFn {
		return fmt.Errorf("fn %s: #no_recurse violation - function calls itself", targetFn)
	}

	// Already explored this callee in this traversal - skip.
	if visited[calleeName] {
		return nil
	}

	visited[calleeName] = true

	fd, known := cg.funcDecls[lookupName]
	if !known {
		// External / built-in - cannot trace; conservatively allow.
		return nil
	}

	// Recursively check the callee's body for a path back to targetFn.

	return cg.walkNoRecurseNode(targetFn, fd.Body, visited)
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// resolveCalleeName extracts the function name string from a CallExpr's Func field,
// returning "" if it cannot be determined statically (e.g. function pointer).
// For method calls (FieldAccess), returns ".methodName" so the callers can do
// a suffix-based lookup across all registered struct methods.
func resolveCalleeName(call *ast.CallExpr) string {
	switch f := call.Func.(type) {
	case *ast.Identifier:
		return f.Name
	case *ast.ScopeAccess:
		return strings.Join(f.Path, "::")
	case *ast.FieldAccess:
		// obj.method() - return ".method" as a hint; callers will do a suffix search
		return "." + f.Field
	}

	return ""
}
