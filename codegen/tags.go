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
// no calls to unverifiable external package functions, and no reads or
// writes of mutable top-level `var` globals - except inside
// #allow_sideffect blocks.
func (cg *CodeGen) checkPureBody(fn *ast.FuncDecl) error {
	visited := make(map[string]bool)

	locals := make(map[string]bool, len(fn.Params))
	for _, p := range fn.Params {
		locals[p.Name] = true
	}

	return cg.walkPureNode(fn.Name, fn.Body, false, visited, locals)
}

// cloneLocals returns a shallow copy of locals so a child scope can extend
// its bindings without leaking them back to siblings of its enclosing scope.
func cloneLocals(locals map[string]bool) map[string]bool {
	out := make(map[string]bool, len(locals))
	for k, v := range locals {
		out[k] = v
	}

	return out
}

// walkPureNode walks an AST node looking for side-effect violations.
// fnCtx is the name of the #pure function being checked (for error messages).
// allowSideEffect is true when we're inside an #allow_sideffect block.
// visited prevents infinite recursion when following the call graph.
// locals tracks identifiers introduced by params, let-bindings, and for-iter
// vars so that an Identifier read can be matched against the top-level var
// set without false positives when a local shadows a global of the same name.
func (cg *CodeGen) walkPureNode(fnCtx string, node ast.Node, allowSideEffect bool, visited map[string]bool, locals map[string]bool) error {
	if node == nil {
		return nil
	}

	switch v := node.(type) {
	case *ast.EchoStmt:
		if !allowSideEffect {
			return cg.nodeErr(v, "fn %s: #pure violation - echo is a side effect", fnCtx)
		}

	case *ast.Identifier:
		if !allowSideEffect && cg.topLevelVarBareNames[v.Name] && !locals[v.Name] {
			return cg.nodeErr(v, "fn %s: #pure violation - reads mutable top-level var %q", fnCtx, v.Name)
		}

	case *ast.ScopeAccess:
		// pkg::name read: reject if it resolves to a mutable top-level var in
		// any package. The bare name is the last path segment.
		if !allowSideEffect && len(v.Path) > 0 {
			last := v.Path[len(v.Path)-1]
			if cg.topLevelVarBareNames[last] {
				return cg.nodeErr(v, "fn %s: #pure violation - reads mutable top-level var %q", fnCtx, last)
			}
		}

	case *ast.TaggedBlock:
		// #allow_sideffect block: permit side effects inside
		inner := allowSideEffect || hasTag(v.Tags, "allow_sideffect")

		return cg.walkPureNode(fnCtx, v.Body, inner, visited, locals)

	case *ast.CallExpr:
		if !allowSideEffect {
			if err := cg.checkCallPure(fnCtx, v, visited); err != nil {
				return err
			}
		}
		// Also walk argument expressions
		for _, arg := range v.Args {
			if err := cg.walkPureNode(fnCtx, arg, allowSideEffect, visited, locals); err != nil {
				return err
			}
		}

	case *ast.Block:
		// Each block establishes its own scope: clone locals so let-bindings
		// introduced inside this block do not escape to enclosing siblings.
		blockLocals := cloneLocals(locals)
		for _, s := range v.Stmts {
			if err := cg.walkPureNode(fnCtx, s, allowSideEffect, visited, blockLocals); err != nil {
				return err
			}
		}

	case *ast.IfStmt:
		if err := cg.walkPureNode(fnCtx, v.Cond, allowSideEffect, visited, locals); err != nil {
			return err
		}

		if v.Then != nil {
			if err := cg.walkPureNode(fnCtx, v.Then, allowSideEffect, visited, locals); err != nil {
				return err
			}
		}

		for _, ei := range v.ElseIfs {
			if err := cg.walkPureNode(fnCtx, ei.Cond, allowSideEffect, visited, locals); err != nil {
				return err
			}

			if ei.Body != nil {
				if err := cg.walkPureNode(fnCtx, ei.Body, allowSideEffect, visited, locals); err != nil {
					return err
				}
			}
		}

		if v.Else != nil {
			if err := cg.walkPureNode(fnCtx, v.Else, allowSideEffect, visited, locals); err != nil {
				return err
			}
		}

	case *ast.ForStmt:
		// For-loops introduce their iter var (and any C-style init) in a fresh
		// scope that does not leak past the loop.
		forLocals := cloneLocals(locals)
		if v.VarName != "" {
			forLocals[v.VarName] = true
		}

		if err := cg.walkPureNode(fnCtx, v.Cond, allowSideEffect, visited, forLocals); err != nil {
			return err
		}

		if err := cg.walkPureNode(fnCtx, v.Init, allowSideEffect, visited, forLocals); err != nil {
			return err
		}

		if err := cg.walkPureNode(fnCtx, v.Post, allowSideEffect, visited, forLocals); err != nil {
			return err
		}

		if err := cg.walkPureNode(fnCtx, v.Iter, allowSideEffect, visited, forLocals); err != nil {
			return err
		}

		if v.Body != nil {
			if err := cg.walkPureNode(fnCtx, v.Body, allowSideEffect, visited, forLocals); err != nil {
				return err
			}
		}

	case *ast.ReturnStmt:
		return cg.walkPureNode(fnCtx, v.Value, allowSideEffect, visited, locals)

	case *ast.AssignStmt:
		if err := cg.walkPureNode(fnCtx, v.Target, allowSideEffect, visited, locals); err != nil {
			return err
		}

		return cg.walkPureNode(fnCtx, v.Value, allowSideEffect, visited, locals)

	case *ast.AugAssignStmt:
		if err := cg.walkPureNode(fnCtx, v.Target, allowSideEffect, visited, locals); err != nil {
			return err
		}

		return cg.walkPureNode(fnCtx, v.Value, allowSideEffect, visited, locals)

	case *ast.VarDecl:
		// Walk the initializer in the OLD scope, then introduce the binding so
		// `let x = x` reads the outer x but later refs see the new local.
		if err := cg.walkPureNode(fnCtx, v.Value, allowSideEffect, visited, locals); err != nil {
			return err
		}

		locals[v.Name] = true

	case *ast.ExprStmt:
		return cg.walkPureNode(fnCtx, v.Expr, allowSideEffect, visited, locals)

	case *ast.BinExpr:
		if err := cg.walkPureNode(fnCtx, v.Left, allowSideEffect, visited, locals); err != nil {
			return err
		}

		return cg.walkPureNode(fnCtx, v.Right, allowSideEffect, visited, locals)

	case *ast.UnaryExpr:
		return cg.walkPureNode(fnCtx, v.Expr, allowSideEffect, visited, locals)

	case *ast.TernaryExpr:
		if err := cg.walkPureNode(fnCtx, v.Cond, allowSideEffect, visited, locals); err != nil {
			return err
		}

		if err := cg.walkPureNode(fnCtx, v.Then, allowSideEffect, visited, locals); err != nil {
			return err
		}

		return cg.walkPureNode(fnCtx, v.Else, allowSideEffect, visited, locals)

	case *ast.FieldAccess:
		return cg.walkPureNode(fnCtx, v.Expr, allowSideEffect, visited, locals)

	case *ast.IndexExpr:
		if err := cg.walkPureNode(fnCtx, v.Expr, allowSideEffect, visited, locals); err != nil {
			return err
		}

		return cg.walkPureNode(fnCtx, v.Index, allowSideEffect, visited, locals)

	case *ast.DeferStmt:
		return cg.walkPureNode(fnCtx, v.Call, allowSideEffect, visited, locals)

	case *ast.WhereList:
		for _, c := range v.Clauses {
			if err := cg.walkPureNode(fnCtx, c.Body, allowSideEffect, visited, locals); err != nil {
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
		return cg.nodeErr(call, "fn %s: #pure violation - indirect call through function pointer is not verifiable", fnCtx)
	}

	if err := cg.isPureCallable(fnCtx, calleeName, visited); err != nil {
		return cg.nodeErr(call, "%s", err.Error())
	}

	return nil
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

			calleeLocals := make(map[string]bool, len(fd.Params))
			for _, p := range fd.Params {
				calleeLocals[p.Name] = true
			}

			if err := cg.walkPureNode(fnCtx, fd.Body, false, visited, calleeLocals); err != nil {
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

	// Transitively check the callee's body. Seed locals from the callee's own
	// params so reads of its parameters are not mistaken for global reads.
	calleeLocals := make(map[string]bool, len(fd.Params))
	for _, p := range fd.Params {
		calleeLocals[p.Name] = true
	}

	return cg.walkPureNode(fnCtx, fd.Body, false, visited, calleeLocals)
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
				return cg.nodeErr(call, "fn %s: #no_recurse violation - function calls itself", targetFn)
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
		return cg.nodeErr(call, "fn %s: #no_recurse violation - function calls itself", targetFn)
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
