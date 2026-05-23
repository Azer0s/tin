package codegen

// exprs_ref.go - codegen for the `ref a` call-site assertion.
//
// `ref a` evaluates exactly like a plain identifier read (no rc work
// at the keyword itself).  Its effect is a compile-time check at the
// call site: the matched parameter's convention must be `transparent`
// (analyzer-classified as a read-only, non-escaping borrow), and any
// other classification (`consumes` or `retains`) is rejected with a
// fix-it pointing at the two paths out -- drop `ref` or refactor the
// callee to not retain or escape the param.
//
// Outside a call-arg position the keyword degrades to a plain
// identifier read.  There is no callee context to look up; the
// assertion silently passes.

import (
	"fmt"

	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// callArgContext is one frame on cg.callArgContextStack, pushed by
// genCallExpr while a call's args are evaluated.  The frame records
// the callee's mangled name (empty for dynamic callees like lambda,
// trait dispatch or extern), the ordered param-name list for
// convention lookups, and the live AST args slice.  A nested
// `ref a` evaluation finds its own position by scanning args for an
// identity match and reads the convention for that param.
type callArgContext struct {
	calleeName string
	paramNames []string
	args       []ast.Node
}

// pushCallArgContext starts tracking arg evaluation for a call.  Pass
// the empty string for calleeName when the callee is not analyzable
// (extern, lambda, trait dispatch on an unknown impl); ref-arg checks
// stay lenient in that case rather than rejecting valid code.
func (cg *CodeGen) pushCallArgContext(calleeName string, paramNames []string, args []ast.Node) {
	cg.callArgContextStack = append(cg.callArgContextStack, callArgContext{
		calleeName: calleeName,
		paramNames: paramNames,
		args:       args,
	})
}

// popCallArgContext removes the top frame.  Must be paired with each
// pushCallArgContext.
func (cg *CodeGen) popCallArgContext() {
	if n := len(cg.callArgContextStack); n > 0 {
		cg.callArgContextStack = cg.callArgContextStack[:n-1]
	}
}

// callArgPosition returns the matched param name when `arg` is one of
// the top-frame's args (identity comparison), or the empty string
// otherwise.  Used by genRefExpr to find the convention slot.
func (cg *CodeGen) callArgPosition(arg ast.Node) string {
	n := len(cg.callArgContextStack)
	if n == 0 {
		return ""
	}

	ctx := cg.callArgContextStack[n-1]

	for i, a := range ctx.args {
		if a == arg && i < len(ctx.paramNames) {
			return ctx.paramNames[i]
		}
	}

	return ""
}

// callContextInfoFor extracts the callee's mangled name and ordered
// param-name list for the call-site ref machinery.  Returns ("", nil)
// when the callee is dynamic (lambda, trait dispatch on an opaque
// impl, etc.); call-site convention checks stay silent in that case.
//
// For instance method calls `value.method(...)`, attempts to resolve
// the concrete method by looking up the receiver's static type and
// constructing the `Type_method` key.  Resolution only succeeds when
// the receiver is an Identifier whose binding's tinType is a simple
// named struct (the common case); other receiver shapes (field
// access, call result, generic dispatch) stay dynamic and return ""
// so the call-site autocopy / ref checks degrade silently.
func (cg *CodeGen) callContextInfoFor(fnNode ast.Node) (string, []string) {
	if fnNode == nil {
		return "", nil
	}

	var name string

	switch fn := fnNode.(type) {
	case *ast.Identifier:
		if fn != nil {
			name = fn.Name
		}
	case *ast.ScopeAccess:
		if fn != nil {
			joined := ""
			for i, p := range fn.Path {
				if i > 0 {
					joined += "__"
				}

				joined += p
			}

			name = joined
		}
	case *ast.FieldAccess:
		if fn != nil {
			name = cg.resolveMethodKeyForCallee(fn)
		}
	}

	if name == "" {
		return "", nil
	}

	decl := cg.funcDecls[name]
	if decl == nil {
		return name, nil
	}

	paramNames := make([]string, 0, len(decl.Params))

	for _, p := range decl.Params {
		paramNames = append(paramNames, p.Name)
	}

	// Method-call paramNames begin with the receiver (`this`), but the
	// caller's e.Args list does NOT include the receiver - it's the
	// FieldAccess.Expr.  Skip the receiver from the paramNames so
	// callArgPosition's per-position lookup matches arg[i] to the
	// post-receiver parameter slot.  Detected as: callee resolved
	// from a FieldAccess (method-key shape `Struct_method`).
	if _, ok := fnNode.(*ast.FieldAccess); ok && len(paramNames) > 0 {
		paramNames = paramNames[1:]
	}

	return name, paramNames
}

// resolveMethodKeyForCallee tries to map `receiver.method` to the
// `Type_method` key registered in funcDecls when the receiver is a
// scope-bound Identifier with a known static struct type.  Returns
// the empty string for any shape the resolver can't handle (the
// caller treats this as "dynamic dispatch, skip ref/autocopy
// machinery").
func (cg *CodeGen) resolveMethodKeyForCallee(fa *ast.FieldAccess) string {
	if fa == nil || fa.Field == "" {
		return ""
	}

	recvID, ok := fa.Expr.(*ast.Identifier)
	if !ok || recvID == nil || cg.curScope == nil {
		return ""
	}

	entry, found := cg.curScope.lookup(recvID.Name)
	if !found || entry == nil {
		return ""
	}

	structName := ""

	if entry.tinType != nil {
		if simple, ok2 := entry.tinType.(*ast.SimpleType); ok2 {
			structName = simple.Name
		}
	}

	if structName == "" && entry.val != nil {
		structName = cg.typeNameOf(entry.val.Type())
		// Pointer-receiver bindings store an alloca whose elem is
		// the struct; peel one level to find the struct's name.
		if structName == "" {
			if pt, isPtr := entry.val.Type().(*irtypes.PointerType); isPtr {
				structName = cg.typeNameOf(pt.ElemType)
			}
		}
	}

	if structName == "" {
		return ""
	}

	key := structName + "_" + fa.Field
	if _, hasDecl := cg.funcDecls[key]; !hasDecl {
		return ""
	}

	return key
}

// genRefExpr emits the value of the borrowed identifier (identical to
// a plain identifier read at the IR level) and validates the
// assertion: when `ref a` appears as a call argument the callee's
// per-param convention for the matched position must be `transparent`.
// Any other convention (consumes / retains) is a compile error.
//
// Outside a call-arg context there is no callee to consult; the
// keyword degrades to an identifier read.
func (cg *CodeGen) genRefExpr(block *ir.Block, e *ast.RefExpr) (value.Value, error) {
	if e == nil || e.Name == "" {
		return nil, fmt.Errorf("%s: malformed ref expression", cg.posStr(e))
	}

	_, ok := cg.curScope.lookup(e.Name)
	if !ok {
		return nil, fmt.Errorf("%s: cannot borrow `%s`: no such binding in scope",
			cg.posStr(e), e.Name)
	}

	if err := cg.checkRefCallSiteConvention(e); err != nil {
		return nil, err
	}

	idRef := &ast.Identifier{Name: e.Name}
	idRef.SetPos(e.Pos())

	return cg.genIdentifier(block, idRef)
}

// checkRefCallSiteConvention verifies that a `ref a` argument matches
// the callee's `transparent` convention for the corresponding param.
// Returns nil when:
//   - there's no enclosing call (ref used outside a call-arg position),
//   - the callee isn't analyzable (extern, lambda, trait-dispatched impl),
//   - or the convention is `transparent`.
//
// Otherwise emits a compile error pointing at the ref keyword.
func (cg *CodeGen) checkRefCallSiteConvention(e *ast.RefExpr) error {
	n := len(cg.callArgContextStack)
	if n == 0 {
		return nil
	}

	ctx := cg.callArgContextStack[n-1]
	if ctx.calleeName == "" {
		return nil
	}

	conv := cg.paramConventionsFor(ctx.calleeName)
	if conv == nil {
		return nil
	}

	paramName := cg.callArgPosition(e)
	if paramName == "" {
		return nil
	}

	got, ok := conv[paramName]
	if !ok || got == paramTransparent {
		return nil
	}

	return fmt.Errorf("%s: cannot pass `ref %s` to `%s`: parameter `%s` is %s (would emit rc traffic)\n"+
		"  help: drop `ref` to allow the retain/release:  %s\n"+
		"  help: or refactor `%s` so it does not %s the parameter",
		cg.posStr(e), e.Name, ctx.calleeName, paramName, got.String(),
		e.Name, ctx.calleeName, refRemediationVerb(got))
}

// refRemediationVerb returns the human verb that fits the convention
// for use in the `ref` rejection error's fix-it line.
func refRemediationVerb(c ParamConvention) string {
	switch c {
	case paramConsumes:
		return "consume"
	case paramRetains:
		return "retain"
	}

	return "use"
}
