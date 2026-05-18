package codegen

import (
	"fmt"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) synthesizeImplicitMainForTry(stmts []ast.Node) []ast.Node {
	hasExplicitMain := false

	for _, s := range stmts {
		if fd, ok := s.(*ast.FuncDecl); ok && fd.Name == "main" && !fd.IsStatic {
			hasExplicitMain = true

			break
		}
	}

	if hasExplicitMain {
		return stmts
	}

	bodyStmts := make([]ast.Node, 0, len(stmts))

	otherStmts := make([]ast.Node, 0, len(stmts))

	hasTry := false

	for _, s := range stmts {
		if isTopLevelDecl(s) {
			otherStmts = append(otherStmts, s)

			continue
		}

		bodyStmts = append(bodyStmts, s)

		if nodeContainsTry(s) {
			hasTry = true
		}
	}

	if !hasTry {
		return stmts
	}

	// `return Ok(0)` as the final body statement.  Using i64 instead of
	// sync::Unit avoids forcing the user's script to `use sync` just to
	// satisfy the synthetic Result wrapper.  The C-main wrapper accepts
	// Result[i64, errors::Err] equally well and exits with the inner i64
	// truncated to i32.
	okCall := &ast.CallExpr{
		Func: &ast.Identifier{Name: "Ok"},
		Args: []ast.Node{
			&ast.IntLit{Value: 0},
		},
	}

	bodyStmts = append(bodyStmts, &ast.ReturnStmt{Value: okCall})

	resultType := &ast.GenericType{
		Name: "Result",
		TypeParams: []ast.TypeExpr{
			&ast.SimpleType{Name: "i64"},
			&ast.SimpleType{Name: "errors::Err"},
		},
	}

	mainDecl := &ast.FuncDecl{
		Name:    "main",
		RetType: resultType,
		Body:    &ast.Block{Stmts: bodyStmts},
	}

	return append(otherStmts, mainDecl)
}

// isTopLevelDecl returns true for AST nodes that belong at the top
// level and MUST stay outside the synthesized main body (function /
// type / use / etc. declarations).  Everything else is imperative
// script-level code and gets absorbed into the synthesized main.
func isTopLevelDecl(n ast.Node) bool {
	switch n.(type) {
	case *ast.FuncDecl, *ast.StructDecl, *ast.EnumDecl, *ast.TypeDecl,
		*ast.UseDecl, *ast.ExportDecl, *ast.TraitDecl, *ast.MacroDecl,
		*ast.UnionDecl, *ast.DataDecl, *ast.TestDecl, *ast.TopLevelVar:
		return true
	}

	return false
}

// nodeContainsTry returns true when n or any of its descendants is a
// TryExpr.  Used by the implicit-main synthesizer to decide whether
// the script needs a Result-typed wrapper.
func nodeContainsTry(n ast.Node) bool {
	if n == nil {
		return false
	}

	if _, ok := n.(*ast.TryExpr); ok {
		return true
	}

	found := false

	walkAST(n, func(v ast.Node) {
		if _, ok := v.(*ast.TryExpr); ok {
			found = true
		}
	})

	return found
}

// tryEmitResultMainReturn unpacks ret as a Result[Unit | i64, errors::Err]
// when the user's `fn main()` is typed that way.  Ok exits 0 (or the
// inner i64 truncated to i32); Err calls the iface's `errors::Err::message`
// method, forwards the string to the `_tin_main_err_exit` C helper which
// prints "error: <msg>\n" to stderr and exits 1.  Returns true when the
// helper emitted the full unpack-and-return chain (caller must skip its
// own terminator).
func (cg *CodeGen) tryEmitResultMainReturn(block *ir.Block, ret value.Value) bool {
	retSt, ok := ret.Type().(*irtypes.StructType)
	if !ok {
		return false
	}

	name := retSt.Name()
	if !strings.HasPrefix(name, "Result__") {
		return false
	}

	variants, ok := cg.dataVariants[name]
	if !ok {
		return false
	}

	okVI, ok := variants["Ok"]
	if !ok {
		return false
	}

	errVI, ok := variants["Err"]
	if !ok {
		return false
	}
	// Ok payload must be Unit or i64 - any other shape means the caller
	// can't sensibly express the process exit code.  An Err payload that
	// looks like errors::Err but an Ok shape we don't handle (e.g.
	// `fn main() Result[i32, errors::Err]`) would silently swallow the
	// Err if we fell through to the generic non-Result path; refuse the
	// signature instead so the user picks i64 or Unit.
	okInnerTy, okShape := resultMainOkInnerType(okVI)

	errIsErrIface := resultMainErrIsErrIface(errVI)
	if !okShape && errIsErrIface {
		panic(fmt.Sprintf("fn main() return type %s is not supported as a process exit shape; use Result[Unit, errors::Err] or Result[i64, errors::Err]", name))
	}

	if !okShape {
		return false
	}
	// Err payload must be an errors::Err fat-ptr iface so we know how to
	// call .message() on it.
	if !errIsErrIface {
		return false
	}

	// Stash the returned Result so we can GEP into it.
	alloca := block.NewAlloca(retSt)
	block.NewStore(ret, alloca)

	tagGEP := block.NewGetElementPtr(retSt, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	tag := block.NewLoad(irtypes.I64, tagGEP)
	isOk := block.NewICmp(enum.IPredEQ, tag, constant.NewInt(irtypes.I64, okVI.Tag))

	parent := block.Parent
	okBlk := parent.NewBlock("main.result.ok")
	errBlk := parent.NewBlock("main.result.err")
	block.NewCondBr(isOk, okBlk, errBlk)

	// Ok branch.
	payloadGEP := okBlk.NewGetElementPtr(retSt, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))

	switch okInnerTy {
	case "Unit":
		okBlk.NewRet(constant.NewInt(irtypes.I32, 0))
	case "i64":
		payloadPtr := okBlk.NewBitCast(payloadGEP, irtypes.NewPointer(irtypes.I64))
		v := okBlk.NewLoad(irtypes.I64, payloadPtr)
		okBlk.NewRet(okBlk.NewTrunc(v, irtypes.I32))
	}

	// Err branch: extract iface fat-ptr, call vtable message slot, forward
	// the resulting TinString to _tin_main_err_exit.
	errPayloadGEP := errBlk.NewGetElementPtr(retSt, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))

	// The Err variant's payload struct stores the errors::Err iface as
	// its field 0; reuse that struct type directly so we don't depend
	// on whether buildIfaceShape happened to register the type under
	// `cg.traitFatPtrTypes` yet.
	ifaceTy, ok := errVI.PayloadType.Fields[0].(*irtypes.StructType)
	if !ok {
		errBlk.NewRet(constant.NewInt(irtypes.I32, 1))

		return true
	}

	ifaceP := errBlk.NewBitCast(errPayloadGEP, irtypes.NewPointer(ifaceTy))
	ifaceVal := errBlk.NewLoad(ifaceTy, ifaceP)
	dataPtr := errBlk.NewExtractValue(ifaceVal, 0)
	vtPtr := errBlk.NewExtractValue(ifaceVal, 1)

	msgFn, ok := cg.findErrMessageSlot(vtPtr.Type())
	if !ok {
		errBlk.NewRet(constant.NewInt(irtypes.I32, 1))

		return true
	}
	// Load the message slot from the vtable.
	msgSlotPtr := errBlk.NewGetElementPtr(msgFn.vtType, vtPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(msgFn.slotIdx)))
	msgFnVal := errBlk.NewLoad(msgFn.fnPtrType, msgSlotPtr)
	msgStr := errBlk.NewCall(msgFnVal, dataPtr)

	strPtr := errBlk.NewExtractValue(msgStr, 0)
	strLen := errBlk.NewExtractValue(msgStr, 1)
	errExitFn := cg.ensureExternDecl("_tin_main_err_exit", irtypes.Void, []*ir.Param{
		ir.NewParam("msg", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64),
	}, false)
	errBlk.NewCall(errExitFn, strPtr, strLen)
	// _tin_main_err_exit calls exit(); marking this block unreachable
	// lets LLVM drop any code after it.
	errBlk.NewUnreachable()

	return true
}

// resultMainOkInnerType classifies the Ok payload of `Result[T, errors::Err]`
// when used as main's return type.  Returns ("Unit", true), ("i64", true),
// or ("", false) for any other shape (which falls back to the generic
// non-Result main return handling).
func resultMainOkInnerType(vi *dataVariantInfo) (string, bool) {
	if vi == nil || vi.PayloadType == nil || len(vi.PayloadType.Fields) != 1 {
		return "", false
	}

	ft := vi.PayloadType.Fields[0]
	if it, ok := ft.(*irtypes.IntType); ok && it.BitSize == 64 {
		return "i64", true
	}

	if st, ok := ft.(*irtypes.StructType); ok {
		name := st.Name()
		if name == "sync__Unit" || name == "Unit" {
			return "Unit", true
		}
	}

	return "", false
}

// resultMainErrIsErrIface returns true when the Err variant's payload
// is the `errors::Err` trait-object struct (fat ptr layout: {i8* data, vt* vtable}).
func resultMainErrIsErrIface(vi *dataVariantInfo) bool {
	if vi == nil || vi.PayloadType == nil || len(vi.PayloadType.Fields) != 1 {
		return false
	}

	st, ok := vi.PayloadType.Fields[0].(*irtypes.StructType)
	if !ok {
		return false
	}

	return st.Name() == "errors__Err_iface"
}

// errMessageSlot resolves the layout of `errors::Err::message` for the
// active errors::Err vtable type so the wrapper can call the right
// vtable slot at IR-emit time.
type errMessageSlot struct {
	vtType    *irtypes.StructType
	fnPtrType *irtypes.PointerType
	slotIdx   int
}

// findErrMessageSlot returns the vtable struct's `message` slot
// metadata so tryEmitResultMainReturn can GEP into the right offset
// and load the function pointer to call.  Walks cg.traitVtableFields
// to find the index of the `message` method.
func (cg *CodeGen) findErrMessageSlot(vtPtrTy irtypes.Type) (*errMessageSlot, bool) {
	pt, ok := vtPtrTy.(*irtypes.PointerType)
	if !ok {
		return nil, false
	}

	vt, ok := pt.ElemType.(*irtypes.StructType)
	if !ok {
		return nil, false
	}

	for i, f := range vt.Fields {
		fpt, ok := f.(*irtypes.PointerType)
		if !ok {
			continue
		}

		ft, ok := fpt.ElemType.(*irtypes.FuncType)
		if !ok {
			continue
		}
		// The `message` method has signature `(*Err data) TinString`.
		// TinString is the {i8*, i64} struct; match by return type.
		retSt, ok := ft.RetType.(*irtypes.StructType)
		if !ok {
			continue
		}

		if len(retSt.Fields) != 2 {
			continue
		}

		if _, ok := retSt.Fields[0].(*irtypes.PointerType); !ok {
			continue
		}

		if it, ok := retSt.Fields[1].(*irtypes.IntType); !ok || it.BitSize != 64 {
			continue
		}

		if len(ft.Params) != 1 {
			continue
		}

		return &errMessageSlot{vtType: vt, fnPtrType: fpt, slotIdx: i}, true
	}

	return nil, false
}

// newCMainWrapper creates the C-side entry-point function under the IR
// name `_tin_c_main`, plus an `@main` alias so libc / `__libc_start_main`
// still finds the conventional entry symbol. The rename keeps stacktrace
// frames inside the wrapper distinct from the user's `fn main` (compiled
// as `_tin_user_main` and displayed as `main`); without the rename, the
// trace would show two consecutive `main`-named frames and confuse
// readers about which is which.
//
// `withArgs` controls whether the wrapper takes the libc (argc, argv)
// signature; the caller decides based on the user main's parameter list.
//
// LLVM aliases are handled by both ld.lld and GNU ld; on Mach-O the
// convention is the same alias syntax via `--defsym` equivalent.
// Returns the wrapper *ir.Func - the alias is internal bookkeeping.
func (cg *CodeGen) newCMainWrapper(withArgs bool) *ir.Func {
	var wf *ir.Func
	if withArgs {
		wf = cg.mod.NewFunc("_tin_c_main", irtypes.I32,
			ir.NewParam("argc", irtypes.I32),
			ir.NewParam("argv", irtypes.NewPointer(irtypes.I8Ptr)),
		)
	} else {
		wf = cg.mod.NewFunc("_tin_c_main", irtypes.I32)
	}

	cg.mod.Aliases = append(cg.mod.Aliases, ir.NewAlias("main", wf))

	return wf
}

// applyStacktracePostPass walks every emitted function and tags it with
// `frame-pointer="all"` when the program references stacktrace(). Required
// for the runtime's frame-pointer walker (runtime/stacktrace.c, fp_walk)
// to step through every Tin frame: LLVM at -O2 otherwise elides %rbp
// setup on leaf / short functions and the FP walk skips them.
//
// Must be the LAST step in Generate so it covers everything that
// cg.mod.NewFunc has produced - user fns, atom helpers, ADT release/retain
// helpers, coro splits, lambda thunks, test runners, REPL cells. The
// helper is shared across the three Generate exit branches (test runner,
// REPL, normal main) so none of them slip past the tagging.
//
// clang's `-fno-omit-frame-pointer` cmd-line flag does NOT propagate into
// IR-compiled functions; it only sets the default for code clang
// generates from C source. Function attributes embedded in the IR are
// the only mechanism that survives the IR -> object pipeline.
func (cg *CodeGen) applyStacktracePostPass() {
	if !cg.stacktraceUsed {
		return
	}

	for _, f := range cg.allFuncs() {
		if f.Blocks == nil {
			continue // declarations don't carry codegen attributes
		}

		f.FuncAttrs = append(f.FuncAttrs,
			ir.AttrPair{Key: "frame-pointer", Value: "all"})
	}
}

// mainTakesStringArgs reports whether the user's explicit fn main has a first
// parameter of type [string] (dynamic string array).
func mainTakesStringArgs(n *ast.FuncDecl) bool {
	if n == nil || len(n.Params) == 0 {
		return false
	}

	at, ok := n.Params[0].Type.(*ast.ArrayType)
	if !ok || at.Size >= 0 {
		return false
	}

	st, ok2 := at.Elem.(*ast.SimpleType)

	return ok2 && st.Name == "string"
}

// externHasPrimitiveTypes reports whether all parameter and return types of an
// extern function declaration are built-in (non-struct) types.  Used by
// Pre-pass 2.8 to determine which externs can be safely registered before
// struct types are populated in Pre-pass 3.
func externHasPrimitiveTypes(fd *ast.FuncDecl) bool {
	for _, p := range fd.Params {
		if !typeExprIsPrimitive(p.Type) {
			return false
		}
	}

	return typeExprIsPrimitive(fd.RetType)
}

func typeExprIsPrimitive(te ast.TypeExpr) bool {
	if te == nil {
		return true
	}

	switch t := te.(type) {
	case *ast.SimpleType:
		switch t.Name {
		case "i8", "i16", "i32", "i64", "i128",
			"u8", "u16", "u32", "u64", "u128",
			"f32", "f64", "f128",
			"byte", "char", "bool", "string", "void":
			return true
		}

		return false
	case *ast.PointerType:
		return typeExprIsPrimitive(t.Elem)
	case *ast.ArrayType:
		return typeExprIsPrimitive(t.Elem)
	default:
		return false
	}
}
