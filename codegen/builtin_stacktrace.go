package codegen

// builtin_stacktrace.go - codegen for the runtime `stacktrace()` builtin.
//
// `stacktrace()` returns `[atom]` where each entry is one frame on the
// live call stack, top-of-stack first. Entries are interned atoms of the
// form documented in docs/plans/stacktrace-libunwind.md ("Atom format").
//
// Codegen here is intentionally lean: it sets cg.stacktraceUsed (so
// main.go can flip on `-funwind-tables` / `-rdynamic` / `-lunwind` post-
// Generate), allocates a heap buffer of i32 atom codes via the ARC
// allocator, calls the runtime helper to fill it, then assembles the
// {%__atom*, i64} fat-pointer that Tin code reads as `[atom]`.
//
// All the work behind the runtime call lives in runtime/stacktrace.c.

import (
	"sort"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// stacktraceDefaultCap is the frame-buffer size emitted when the user
// calls `stacktrace()` with no argument. The runtime clamps anything
// outside [1, 1024], but the codegen only ever needs one canonical size
// for the no-arg form. 64 covers virtually every real-world trace and
// keeps the stack/heap pressure trivial (256 bytes).
const stacktraceDefaultCap = 64

// stacktraceMaxCap matches TIN_ST_MAX_CAP in runtime/stacktrace.c.
// Variable-cap calls (`stacktrace(n, ...)`) need a buffer that satisfies
// the largest possible cap; the runtime saturates anything bigger so we
// don't have to validate at codegen time. Keep in sync with the runtime.
const stacktraceMaxCap = 1024

// stacktraceFlagBits maps user-visible filter atoms to the bitfield
// values consumed by tin_capture_stacktrace's `flags` arg. Keep in
// sync with the TIN_ST_HIDE_* macros in runtime/runtime.h.
//
// Adding a new filter requires changes in three places: this map, the
// macro in runtime.h, and the frame_decision check in
// runtime/stacktrace.c. Touching only one is a silent no-op.
var stacktraceFlagBits = map[string]int32{
	"hide_libc":    0x1,
	"hide_unknown": 0x2,
	"hide_runtime": 0x4,
	"hide_main":    0x8,
}

// detectStacktraceUsage walks the top-level statements of `prog` looking
// for any `stacktrace(...)` call and sets cg.stacktraceUsed accordingly.
// Runs before linkage assignment so funcs.go can promote internal-linkage
// Tin user fns to external when stacktrace is reachable; without that
// promotion, dladdr would never resolve them (STB_LOCAL never reaches
// the dynsym, even under `-rdynamic`).
//
// walkAST descends into MacroDecl bodies, so a stacktrace() call that
// only appears via macro expansion (e.g. inside a `log::error!` macro
// that captures the trace) still flips the flag. Without that the
// linkage stays internal and frames render as ??+0x<addr>. Macros
// that use the call indirectly (CTFE-synthesized string interpolation)
// remain a known false-negative; declare them with `stacktrace` written
// literally somewhere in the body to opt in.
//
// Shadow safety: a user `fn stacktrace(...)` of their own would
// shadow the builtin. We mirror the call-site check (exprs_call.go's
// `_, shadowed := cg.curScope.lookup("stacktrace")`) at file scope:
// if any top-level fn / var / const named `stacktrace` exists, the
// user is shadowing on purpose and we skip the gate.
//
// Conservative by design - a `stacktrace` call inside dead code (an
// `if false:` branch, an unreachable `where` arm) still flips the flag.
// False positives only cost binary size, never correctness.
func (cg *CodeGen) detectStacktraceUsage(stmts []ast.Node) {
	if topLevelShadowsName(stmts, "stacktrace") {
		return
	}

	for _, s := range stmts {
		walkAST(s, func(n ast.Node) {
			if cg.stacktraceUsed {
				return
			}

			ce, ok := n.(*ast.CallExpr)
			if !ok {
				return
			}

			id, ok := ce.Func.(*ast.Identifier)
			if !ok {
				return
			}

			if id.Name == "stacktrace" {
				cg.stacktraceUsed = true
			}
		})

		if cg.stacktraceUsed {
			return
		}
	}
}

// topLevelShadowsName reports whether `prog.Stmts` defines a fn / var /
// const at module scope under the given name. Used by
// detectStacktraceUsage to skip the gate when the user has shadowed
// the builtin - without this, an unrelated `fn stacktrace(...)` in
// user code would falsely promote linkage and link libunwind.
func topLevelShadowsName(stmts []ast.Node, name string) bool {
	for _, s := range stmts {
		switch v := s.(type) {
		case *ast.FuncDecl:
			if v.Name == name {
				return true
			}
		case *ast.VarDecl:
			if v.Name == name {
				return true
			}
		case *ast.TopLevelVar:
			if v.Name == name {
				return true
			}
		}
	}

	return false
}

// ensureCaptureStacktrace lazily declares the runtime entry point
//
//	int32 tin_capture_stacktrace(int32* out, int32 cap, int32 flags)
//
// matching runtime/stacktrace.c.
func (cg *CodeGen) ensureCaptureStacktrace() *ir.Func {
	return cg.ensureExternDecl(
		"tin_capture_stacktrace",
		irtypes.I32,
		[]*ir.Param{
			ir.NewParam("out", irtypes.NewPointer(irtypes.I32)),
			ir.NewParam("cap", irtypes.I32),
			ir.NewParam("flags", irtypes.I32),
		},
		false,
	)
}

// parseStacktraceOpts converts an opts atom-array literal into a
// runtime-flag bitfield. The opts must be a literal `[atom]` (e.g.
// `['hide_libc, 'hide_unknown]`) so codegen can fold to a constant;
// non-literal expressions are rejected so the user gets a clear
// compile-time error rather than silent runtime confusion. Returns
// the bitfield and a nil error on success.
//
// Errors are routed through cg.nodeErr so the diagnostic carries the
// opts argument's source position. Without that the user sees an
// unlocated message like "got *ast.Identifier" with no file:line.
func (cg *CodeGen) parseStacktraceOpts(arg ast.Node) (int32, error) {
	lit, ok := arg.(*ast.ArrayLit)
	if !ok {
		return 0, cg.nodeErr(arg, "stacktrace: opts argument must be a literal [atom] (got %T)", arg)
	}

	var flags int32

	for _, e := range lit.Elems {
		atomLit, ok := e.(*ast.AtomLit)
		if !ok {
			return 0, cg.nodeErr(e, "stacktrace: opts entries must be atom literals (got %T)", e)
		}

		bit, ok := stacktraceFlagBits[atomLit.Name]
		if !ok {
			known := make([]string, 0, len(stacktraceFlagBits))
			for k := range stacktraceFlagBits {
				known = append(known, "'"+k)
			}

			sort.Strings(known) // stable diag output for tests + readers

			return 0, cg.nodeErr(atomLit, "stacktrace: unknown opt %q (known: %s)",
				"'"+atomLit.Name, strings.Join(known, ", "))
		}

		flags |= bit
	}

	return flags, nil
}

// genBuiltinStacktrace emits IR for `stacktrace([cap [, opts]])`.
// Returns a `[atom]` slice whose backing storage is ARC-tracked, so
// Tin's normal RC release path frees it when the slice goes out of
// scope.
//
// Argument forms:
//
//	stacktrace()            cap=64, flags=0
//	stacktrace(cap)         cap from arg, flags=0
//	stacktrace(cap, opts)   cap from arg, flags from atom-literal array
//
// `opts` is restricted to a literal `[atom]` because each entry maps to
// a fixed runtime bit; allowing computed values would force a runtime
// translation table without buying anything (the filter set is small
// and stable).
//
// Marking cg.stacktraceUsed = true here is what tells main.go to flip on
// libunwind / unwind-tables / -rdynamic at link time.
func (cg *CodeGen) genBuiltinStacktrace(block *ir.Block, capArg, optsArg ast.Node, _ ast.Pos) (value.Value, error) {
	cg.stacktraceUsed = true

	// Choose buffer size and runtime-cap argument. The constant-cap path
	// (no arg) lets us tune the alloc to the exact 64 slots actually used;
	// the variable-cap path can't predict the runtime cap so it pays the
	// 1024-slot worst case (4 KiB per call, returned to the heap on slice
	// release).
	var (
		bufSlots int64
		capValue value.Value
	)
	if capArg == nil {
		bufSlots = int64(stacktraceDefaultCap)
		capValue = constant.NewInt(irtypes.I32, int64(stacktraceDefaultCap))
	} else {
		bufSlots = int64(stacktraceMaxCap)

		argVal, err := cg.genExpr(block, capArg)
		if err != nil {
			return nil, err
		}

		if cg.curBlock != nil && cg.curBlock != block {
			block = cg.curBlock
		}

		capValue = cg.coerce(block, argVal, irtypes.I32)
	}

	// Parse the optional opts array literal into a constant flag bitfield.
	var flags int32

	if optsArg != nil {
		f, err := cg.parseStacktraceOpts(optsArg)
		if err != nil {
			return nil, err
		}

		flags = f
	}

	bufSize := constant.NewInt(irtypes.I64, bufSlots*4)
	rawBuf := block.NewCall(cg.ensureRCAlloc(), bufSize)
	bufI32 := block.NewBitCast(rawBuf, irtypes.NewPointer(irtypes.I32))

	// Fill the buffer. Attach the current source position to the call
	// instruction explicitly: the runtime's libdwfl resolver maps this
	// IP back to a "symbol@file:line:col" atom, so a missing DILocation
	// would render as "...:0:0".
	captureCall := block.NewCall(
		cg.ensureCaptureStacktrace(),
		bufI32,
		capValue,
		constant.NewInt(irtypes.I32, int64(flags)),
	)
	cg.attachCurrentDbgLoc(captureCall)
	count := captureCall

	atomPtrType := irtypes.NewPointer(cg.atomType)
	bufAtom := block.NewBitCast(rawBuf, atomPtrType)

	fatType := irtypes.NewStruct(atomPtrType, irtypes.I64)
	fatAlloca := block.NewAlloca(fatType)
	ptrGep := block.NewGetElementPtr(fatType, fatAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(bufAtom, ptrGep)
	lenGep := block.NewGetElementPtr(fatType, fatAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(block.NewZExt(count, irtypes.I64), lenGep)

	return block.NewLoad(fatType, fatAlloca), nil
}
