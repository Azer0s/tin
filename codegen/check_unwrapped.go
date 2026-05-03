package codegen

import (
	"strings"

	"github.com/Azer0s/tin/ast"
)

// -Wunwrapped-c-resource
//
// A struct field that holds a value with the shape of a C-managed
// resource (raw `*void`, pointer to an opaque extern struct, or `i64`
// named like an fd / returned by a known fd-returning POSIX function)
// AND whose value transitively crosses an extern boundary AND is not
// already wrapped in a `#no_copy` struct (e.g. *rc::Cell[T]) is a
// double-free / double-close waiting to happen — copying the containing
// Tin struct silently shares ownership of the resource, and both
// scope-exit drops will run the cleanup. The wrapper makes the sharing
// reference-counted; the warning nudges new code toward it.
//
// To intentionally opt out (e.g. a struct that maintains its own
// refcount inside C), put `//!-Wno-unwrapped-c-resource` on the line
// directly above the field. Document why right next to it.

// posixFdReturning lists C functions known to return a POSIX file
// descriptor (or the equivalent on other Unix variants). When a
// struct field's i64 value comes from one of these, the field is
// flagged regardless of its name. Add entries as new platforms /
// libraries land.
var posixFdReturning = map[string]bool{
	// Files
	"open": true, "openat": true, "creat": true,
	"mkstemp": true, "mkstemps": true, "mkostemp": true, "mkostemps": true,
	"memfd_create": true, "shm_open": true,

	// Sockets
	"socket": true, "accept": true, "accept4": true,

	// Duplication
	"dup": true, "dup2": true, "dup3": true,

	// Pipes (these write fds via int[2] out-params; included for completeness)
	"pipe": true, "pipe2": true,

	// Polling / events
	"epoll_create": true, "epoll_create1": true,
	"eventfd": true, "signalfd": true, "timerfd_create": true,
	"inotify_init": true, "inotify_init1": true,
	"kqueue": true,

	// FILE * -> fd
	"fileno": true,
}

// checkAllUnwrappedCResources runs the warning over every struct seen
// during compilation, including stdlib. Walking stdlib structs is what
// lets the warning catch the canonical pattern (Channel/Mutex/...) and
// what lets stdlib authors silence it locally with `//!-Wno-...`.
// Cheap: one pass per struct, one pass per method body.
func (cg *CodeGen) checkAllUnwrappedCResources(prog *ast.Program) {
	if cg.diagSuppressed(DiagUnwrappedCResource) {
		return
	}

	for name, sd := range cg.structDeclsByName {
		if sd == nil {
			continue
		}

		cg.checkStructUnwrappedCResources(name, sd)
	}
}

// checkStructUnwrappedCResources flags fields of sd that look like raw
// C resources flowing through extern boundaries without an Rc wrapper.
// structKey is the canonical name (incl. package prefix) used to find
// the originating source file for `//!-Wno-` lookups.
func (cg *CodeGen) checkStructUnwrappedCResources(structKey string, sd *ast.StructDecl) {
	for _, f := range sd.Fields {
		shape := cg.cResourceFieldShape(f.Type, f.Name)
		if shape == "" {
			continue
		}

		if cg.fieldTypeIsWrapped(f.Type) {
			continue
		}

		if !cg.fieldFlowsThroughExtern(sd, f.Name) {
			continue
		}

		hint := cg.cResourceWrapHint(shape, f.Type, f.Name)

		pos := f.Pos
		if pos.Line == 0 {
			pos = sd.Pos()
		}

		cg.warnInFile(cg.structDeclFiles[structKey], DiagUnwrappedCResource, pos,
			"struct %s field %q (%s) crosses an extern boundary unwrapped — "+
				"copies of %s will alias the resource and double-free on scope exit. %s",
			prettyStructName(sd.Name), f.Name, shape,
			prettyStructName(sd.Name), hint)
	}
}

// cResourceFieldShape returns a short label for the field's C-resource
// shape, or "" if the field type isn't a candidate.
func (cg *CodeGen) cResourceFieldShape(te ast.TypeExpr, fieldName string) string {
	if te == nil {
		return ""
	}

	switch t := te.(type) {
	case *ast.PointerType:
		// *void — by far the most common case.
		if st, ok := t.Elem.(*ast.SimpleType); ok && st.Name == "void" {
			return "*void"
		}
		// Pointer to an opaque extern-declared struct: the user has no
		// Tin-side fields, so the only thing the pointer can be doing
		// is naming a C-managed handle.
		if st, ok := t.Elem.(*ast.SimpleType); ok && cg.isOpaqueExternStruct(st.Name) {
			return "*" + st.Name
		}
	case *ast.SimpleType:
		// i64 named with `fd` in it (any case, any separator).
		if t.Name == "i64" || t.Name == "u64" {
			low := strings.ToLower(fieldName)
			if low == "fd" || strings.HasSuffix(low, "_fd") ||
				strings.HasPrefix(low, "fd_") || strings.Contains(low, "fd") {
				return "i64 fd"
			}
		}
	}

	return ""
}

// fieldTypeIsWrapped reports whether te is a pointer to a #no_copy
// struct (a recognized RC wrapper like *rc::Cell[*void]).
func (cg *CodeGen) fieldTypeIsWrapped(te ast.TypeExpr) bool {
	pt, ok := te.(*ast.PointerType)
	if !ok {
		return false
	}

	return cg.noCopyValueTypeName(pt.Elem) != ""
}

// isOpaqueExternStruct reports whether name refers to a struct that
// has been declared extern (i.e. the body lives in C and Tin holds
// a pointer to it). For now only `void` qualifies — extending to
// arbitrary opaque C structs needs an extern-struct registry that
// Tin doesn't currently maintain. Most C-handle fields use `*void`
// already.
func (cg *CodeGen) isOpaqueExternStruct(name string) bool {
	return false
}

// fieldFlowsThroughExtern walks every method on sd looking for a place
// where the field's value is either:
//   - assigned/initialized from an extern call's return, or
//   - passed into an extern call as an argument.
//
// One level of Tin call indirection through the same struct's other
// methods is followed; deeper transitive flows aren't pursued (they're
// rare for this pattern and the false-negative is acceptable).
func (cg *CodeGen) fieldFlowsThroughExtern(sd *ast.StructDecl, fieldName string) bool {
	for _, m := range sd.Methods {
		if cg.methodTouchesFieldViaExtern(sd.Name, m, fieldName, 0) {
			return true
		}
	}

	return false
}

// matchesStructName reports whether litName (as written in a StructLit)
// could refer to sd's bare name. Tin parses struct literals with the
// surface name (`Mutex`), but our registry keys are package-qualified
// (`sync__Mutex`). Compare the trailing segment in both forms.
func matchesStructName(litName, sdName string) bool {
	if litName == sdName {
		return true
	}

	bare := litName
	if idx := strings.LastIndex(bare, "::"); idx >= 0 {
		bare = bare[idx+2:]
	}

	sdBare := sdName
	if idx := strings.LastIndex(sdBare, "__"); idx >= 0 {
		sdBare = sdBare[idx+2:]
	}

	return bare == sdBare
}

// methodTouchesFieldViaExtern walks m's body looking for the field's
// value crossing an extern boundary. depth caps the inter-method
// recursion so a misbehaving call cycle can't loop forever.
func (cg *CodeGen) methodTouchesFieldViaExtern(structName string, m *ast.FuncDecl, fieldName string, depth int) bool {
	if m == nil || m.Body == nil || depth > 4 {
		return false
	}

	receiver := ""
	if len(m.Params) > 0 {
		receiver = m.Params[0].Name
	}

	hit := false

	walkAST(m.Body, func(n ast.Node) {
		if hit {
			return
		}

		switch e := n.(type) {
		case *ast.CallExpr:
			if cg.callIsExtern(e) {
				for _, a := range e.Args {
					if cg.exprReadsField(a, receiver, fieldName) {
						hit = true

						return
					}
				}

				return
			}
			// Tin-level call — descend one level.
			if depth+1 <= 4 {
				if calleeM := cg.lookupStructMethod(structName, e); calleeM != nil {
					if cg.methodTouchesFieldViaExtern(structName, calleeM, fieldName, depth+1) {
						hit = true
					}
				}
			}
		case *ast.AssignStmt:
			if cg.exprReadsField(e.Target, receiver, fieldName) && cg.exprIsExternProduced(e.Value) {
				hit = true
			}
		case *ast.StructLit:
			if !matchesStructName(e.TypeName, structName) {
				return
			}

			for _, fl := range e.Fields {
				if fl.Name != fieldName {
					continue
				}

				if cg.exprIsExternProduced(fl.Value) {
					hit = true
				}
			}
		}
	})

	return hit
}

// exprReadsField reports whether expr is `receiver.field` (FieldAccess
// reading the named field on the receiver parameter).
func (cg *CodeGen) exprReadsField(expr ast.Node, receiver, field string) bool {
	if receiver == "" {
		return false
	}

	switch e := expr.(type) {
	case *ast.FieldAccess:
		if e.Field != field {
			return false
		}

		id, ok := e.Expr.(*ast.Identifier)

		return ok && id.Name == receiver
	}

	return false
}

// exprIsExternProduced reports whether expr is (or contains) a direct
// call to an extern function. Conservative — only the outer call is
// examined.
func (cg *CodeGen) exprIsExternProduced(expr ast.Node) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}

	return cg.callIsExtern(call)
}

// callIsExtern reports whether the given call resolves (best-effort)
// to a function declared with `= extern("...")`. For the i64-fd shape
// it also catches calls whose name appears in posixFdReturning even
// when the Tin program never declared the extern explicitly (e.g.
// resolved via a wrapper module).
//
// Callees from stdlib live in cg.moduleScope rather than the per-program
// funcDecls registry, so we resolve through the scope when bare lookup
// fails.
func (cg *CodeGen) callIsExtern(call *ast.CallExpr) bool {
	name := resolveCalleeName(call)
	if name == "" {
		return false
	}

	bare := name
	if idx := strings.LastIndex(bare, "::"); idx >= 0 {
		bare = bare[idx+2:]
	}

	bare = strings.TrimPrefix(bare, ".")

	if posixFdReturning[bare] {
		return true
	}

	if fd, ok := cg.funcDecls[bare]; ok && fd.IsExtern != "" {
		return true
	}
	// Stdlib externs land in moduleScope as ir.Func wrappers; if the IR
	// name shows up in externIRNames, it's an extern shim.
	if cg.moduleScope != nil {
		if entry, ok := cg.moduleScope.lookup(bare); ok && entry != nil {
			if f, ok2 := entry.val.(interface{ Name() string }); ok2 {
				if cg.externIRNames[f.Name()] {
					return true
				}
			}
		}
	}

	return false
}

// lookupStructMethod resolves a method call like `this.helper()` to
// the struct method's FuncDecl, if any.
func (cg *CodeGen) lookupStructMethod(structName string, call *ast.CallExpr) *ast.FuncDecl {
	fa, ok := call.Func.(*ast.FieldAccess)
	if !ok {
		return nil
	}

	key := structName + "_" + fa.Field

	return cg.funcDecls[key]
}

// cResourceWrapHint produces a one-line "use *rc::Cell[T]" hint
// tailored to the field's shape. Kept short so it fits next to the
// rest of the diagnostic.
func (cg *CodeGen) cResourceWrapHint(shape string, te ast.TypeExpr, fieldName string) string {
	switch shape {
	case "*void":
		return "Wrap with `*rc::Cell[*void]` (or another #no_copy wrapper) so copies bump a refcount."
	case "i64 fd":
		return "Wrap with `*rc::Cell[i64]` and a closer destructor so copies share the descriptor lifetime."
	}

	return "Wrap with a #no_copy refcount cell so copies share ownership safely."
}
