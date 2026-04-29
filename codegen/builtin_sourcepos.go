package codegen

// builtin_sourcepos.go - implements `sourcepos(expr)`, a compile-time
// builtin that returns an atom of the form `file:line:col`.
//
// Resolution rules (in order):
//
//   sourcepos(my_fn)   -> position of my_fn's *ast.FuncDecl, if known
//   sourcepos(my_var)  -> declaration position of the binding, looked up
//                         in scopeEntry.declPos (locals/params) or
//                         cg.topLevelVarPos (top-level let/var/const)
//   sourcepos(<expr>)  -> the expression's own source position (the
//                         place where it appears in the program)
//
// The result is interned exactly the same way an `:atom` literal would
// be — registerAtom + atomConstant — so a `sourcepos(...)` value is a
// regular atom you can compare, store, format, or pass around.
//
// File component: the compiler does not currently track per-node source
// files (only per-CodeGen entry filename), so the file part is always
// `cg.filenameForDiag()`. Decls reachable from imports therefore report
// the entry file's name with the imported decl's line/col. This matches
// the diagnostic system's existing approximation; tightening it is a
// follow-up that would need per-node file tracking.

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// genBuiltinSourcepos emits the atom-typed compile-time constant for
// `sourcepos(arg)`. callPos is the position of the sourcepos(...) call
// itself, used as the last-resort fallback.
//
// The atom name is wrapped in surrounding double quotes so it matches
// the in-source literal form `'"<file>:<line>:<col>"`. The lexer
// preserves the wrapping double quotes when parsing the complex-atom
// literal (atoms with non-identifier chars MUST use the quoted form)
// AND does not interpret escape sequences inside, so we wrap by raw
// concatenation rather than fmt's %q (which would escape backslashes
// and quotes — making round-trip equality with hand-written atom
// literals fail on paths containing those chars, e.g. on Windows).
func (cg *CodeGen) genBuiltinSourcepos(_ *ir.Block, arg ast.Node, callPos ast.Pos) (value.Value, error) {
	pos := cg.resolveSourcePos(arg, callPos)
	name := `"` + fmt.Sprintf("%s:%d:%d", cg.filenameForDiag(), pos.Line, pos.Col) + `"`

	return cg.atomConstant(cg.registerAtom(name)), nil
}

// resolveSourcePos picks the right Pos for arg, walking the resolution
// hierarchy described in this file's header. Returns callPos when
// nothing better can be found.
func (cg *CodeGen) resolveSourcePos(arg ast.Node, callPos ast.Pos) ast.Pos {
	if arg == nil {
		return callPos
	}

	if id, ok := arg.(*ast.Identifier); ok {
		if pos, found := cg.lookupSymbolPos(id.Name); found {
			return pos
		}

		// Identifier we couldn't resolve to a known decl — fall through
		// to the identifier's own pos rather than callPos so the user
		// at least sees the use site.
		return id.Pos()
	}

	// Any non-identifier expression: report its own source position.
	if p := arg.Pos(); p.Line != 0 || p.Col != 0 {
		return p
	}

	return callPos
}

// lookupSymbolPos resolves a bare name to the source position of its
// declaration. Returns (zero, false) if the name doesn't match any
// known fn / scope binding / top-level var.
//
// Lookup order matches lexical visibility: a local binding (let/var/
// param/for-iter) takes precedence over a top-level fn of the same
// name, and a top-level fn takes precedence over a top-level var.
// Without that ordering, sourcepos on a name shadowed by an inner let
// would resolve to the outer fn's decl line — wrong by language
// semantics, since the user's `name` at the call site refers to the
// inner binding.
func (cg *CodeGen) lookupSymbolPos(name string) (ast.Pos, bool) {
	// 1. lexical scope chain first — locals and parameters that recorded
	//    their binding position win over module-level decls of the same
	//    name, mirroring the resolution path for ordinary identifiers.
	if cg.curScope != nil {
		if e, ok := cg.curScope.lookup(name); ok && e != nil {
			if e.declPos.Line != 0 || e.declPos.Col != 0 {
				return e.declPos, true
			}
		}
	}

	// 2. function declarations (cg.funcDecls is populated module-wide).
	if fd, ok := cg.funcDecls[name]; ok && fd != nil {
		return fd.Pos(), true
	}

	// 3. generic / constrained function templates. These live in their
	//    own registries (genericFuncs / constrainedFuncs) rather than
	//    funcDecls, since the latter holds only fully-monomorphized
	//    entries. Without this branch sourcepos(my_generic) would fall
	//    through to the use-site fallback.
	if fd, ok := cg.genericFuncs[name]; ok && fd != nil {
		return fd.Pos(), true
	}

	if fd, ok := cg.constrainedFuncs[name]; ok && fd != nil {
		return fd.Pos(), true
	}

	// 4. top-level vars / consts recorded during decl processing.
	if pos, ok := cg.topLevelVarPos[name]; ok {
		return pos, true
	}

	return ast.Pos{}, false
}
