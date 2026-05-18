package codegen

import (
	"fmt"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) genInterpolatedString(block *ir.Block, e *ast.InterpolatedString) (value.Value, error) {
	// Build a format string and argument list for printf/sprintf.
	var (
		fmtParts  []string
		args      []value.Value
		toRelease []value.Value // ARC: RC-tracked temporaries to release after snprintf
	)

	for _, part := range e.Parts {
		if !part.IsExpr {
			// Escape % in literal parts.
			escaped := strings.ReplaceAll(part.Str, "%", "%%")
			fmtParts = append(fmtParts, escaped)
		} else {
			cg.curBlock = block

			val, err := cg.genExpr(block, part.Expr)
			if err != nil {
				return nil, err
			}
			// Embedded `await` / async expressions advance cg.curBlock to
			// a fresh continuation block.  Pull that forward so the
			// subsequent format-arg emissions land in the live block;
			// without this the original `block` ends up unterminated and
			// llir's verifier panics with "missing terminator".
			if cg.curBlock != nil && cg.curBlock != block {
				block = cg.curBlock
			}

			if val == nil {
				fmtParts = append(fmtParts, "(nil)")

				continue
			}

			t := val.Type()

			// If a format specifier was provided, use it directly.
			if part.Format != "" {
				fmtSpec := part.Format
				lastChar := fmtSpec[len(fmtSpec)-1]
				prefix := fmtSpec[:len(fmtSpec)-1]

				switch lastChar {
				case 'x', 'X', 'o', 'u':
					// Unsigned/hex/octal integer format
					if it, ok := t.(*irtypes.IntType); ok {
						if it.BitSize > 32 {
							fmtParts = append(fmtParts, "%"+prefix+"ll"+string(lastChar))
							val = cg.coerce(block, val, irtypes.I64)
						} else {
							fmtParts = append(fmtParts, "%"+prefix+string(lastChar))

							if it.BitSize < 32 {
								val = block.NewZExt(val, irtypes.I32)
							}
						}

						args = append(args, val)

						continue
					}
				case 'd', 'i':
					// Signed decimal format; use zero-extend for unsigned 8-bit types.
					if it, ok := t.(*irtypes.IntType); ok {
						if it.BitSize > 32 {
							fmtParts = append(fmtParts, "%"+prefix+"ll"+string(lastChar))
							val = cg.coerce(block, val, irtypes.I64)
						} else {
							fmtParts = append(fmtParts, "%"+prefix+string(lastChar))

							if it.BitSize < 32 {
								ty8 := cg.exprByte8Type(part.Expr)
								if ty8 == "byte" || ty8 == "u8" {
									val = block.NewZExt(val, irtypes.I32)
								} else {
									val = block.NewSExt(val, irtypes.I32)
								}
							}
						}

						args = append(args, val)

						continue
					}
				case 'f', 'e', 'g', 'E', 'G':
					// Floating-point format
					fmtParts = append(fmtParts, "%"+fmtSpec)

					if irtypes.IsFloat(t) {
						if t != irtypes.Double {
							val = block.NewFPExt(val, irtypes.Double)
						}
					} else if irtypes.IsInt(t) {
						val = block.NewSIToFP(val, irtypes.Double)
					}

					args = append(args, val)

					continue
				case 's':
					// String format
					if isStringType(t) {
						fmtParts = append(fmtParts, "%"+fmtSpec)
						args = append(args, cg.extractStringPtr(block, val))

						continue
					}
				}
				// Unknown format specifier - fall through to default handling
			}

			switch {
			case isStringType(t):
				fmtParts = append(fmtParts, "%s")
				ptr := cg.extractStringPtr(block, val)
				args = append(args, ptr)
				// ARC: temporary string (call/concat result) must be released after snprintf.
				if isTemporaryProducer(part.Expr) {
					toRelease = append(toRelease, val)
				}
			case irtypes.IsInt(t):
				it := t.(*irtypes.IntType)
				switch it.BitSize {
				case 1:
					// bool: print "true" or "false"
					truePtr := cg.newGlobalString("true")
					falsePtr := cg.newGlobalString("false")
					selected := block.NewSelect(val, truePtr, falsePtr)

					fmtParts = append(fmtParts, "%s")
					args = append(args, selected)
				case 8:
					// Dispatch by Tin type: char->%c, byte->%x, u8/i8->%d
					// Use format specifiers ({c:d}, {c:x}, {c:c}) to override.
					switch cg.exprByte8Type(part.Expr) {
					case "char":
						fmtParts = append(fmtParts, "%c")
					case "byte":
						fmtParts = append(fmtParts, "%x")
					default: // "u8", "i8", ""
						fmtParts = append(fmtParts, "%d")
					}

					// byte/u8 are unsigned - zero-extend; char/i8 are signed - sign-extend.
					ty8 := cg.exprByte8Type(part.Expr)
					if ty8 == "byte" || ty8 == "u8" {
						val = block.NewZExt(val, irtypes.I32)
					} else {
						val = block.NewSExt(val, irtypes.I32)
					}

					args = append(args, val)
				case 128:
					// i128/u128: pre-convert to C string via runtime helper, use %s
					ty128 := cg.exprByte8Type(part.Expr)

					var cstr value.Value

					if ty128 == "u128" {
						cstr = block.NewCall(cg.ensureU128ToCstr(), val)
					} else {
						cstr = block.NewCall(cg.ensureI128ToCstr(), val)
					}

					fmtParts = append(fmtParts, "%s")
					args = append(args, cstr)
				default:
					if cg.exprIsUnsigned(part.Expr) {
						// Unsigned 16/32/64-bit: zero-extend to i64, use %llu.
						fmtParts = append(fmtParts, "%llu")

						it2 := t.(*irtypes.IntType)

						if it2.BitSize < 64 {
							val = block.NewZExt(val, irtypes.I64)
						}

						args = append(args, val)
					} else {
						fmtParts = append(fmtParts, "%lld")
						val = cg.coerce(block, val, irtypes.I64)

						args = append(args, val)
					}
				}
			case irtypes.IsFloat(t):
				ft := t.(*irtypes.FloatType)
				if ft.Kind == irtypes.FloatKindFP128 {
					// f128: pre-convert to C string via runtime helper, use %s
					cstr := block.NewCall(cg.ensureF128ToCstr(), val)

					fmtParts = append(fmtParts, "%s")
					args = append(args, cstr)
				} else if t == irtypes.Double {
					// f64: use %f (e.g. "1.000000")
					fmtParts = append(fmtParts, "%f")
					args = append(args, val)
				} else {
					// f32: use %g (e.g. "3" for 3.0, "1.5" for 1.5)
					fmtParts = append(fmtParts, "%g")
					val = block.NewFPExt(val, irtypes.Double)
					args = append(args, val)
				}
			default:
				// print trait: struct or fat-pointer with a print() method.
				if strVal, ok := cg.callPrintTrait(block, val); ok {
					fmtParts = append(fmtParts, "%s")
					ptr := cg.extractStringPtr(block, strVal)
					args = append(args, ptr)
					// ARC: ::print returns a fresh string; release after snprintf.
					toRelease = append(toRelease, strVal)
				} else if isFatArrayPtr(t) {
					// Fat array: pre-render as a fresh fat-string "[e1 e2 ...]".
					strVal, newBlock, aerr := cg.genArrayToFatStr(block, val)
					if aerr != nil {
						return nil, aerr
					}

					block = newBlock

					fmtParts = append(fmtParts, "%s")
					args = append(args, cg.extractStringPtr(block, strVal))
					toRelease = append(toRelease, strVal)
				} else {
					fmtParts = append(fmtParts, "%lld")
					val = cg.coerce(block, val, irtypes.I64)
					args = append(args, val)
				}
			}
		}
	}

	// Build result string using snprintf with a two-pass approach:
	//   1. snprintf(NULL, 0, fmt, ...) -> returns the required length (excluding NUL).
	//   2. _tin_rc_alloc(len+1) -> allocate exact buffer with ARC header.
	//   3. snprintf(buf, len+1, fmt, ...) -> fill buffer.
	// This avoids a fixed-size buffer and handles arbitrarily long interpolations.
	// IMPORTANT: must use _tin_rc_alloc (not malloc) so that the result is ARC-tracked
	// and _tin_release can safely read the RC header 8 bytes before the returned ptr.
	fmtStr := strings.Join(fmtParts, "")
	fmtPtr := cg.newGlobalString(fmtStr)
	snprintfFn := cg.ensureSnprintf()

	// Pass 1: measure required length.
	nullBuf := constant.NewNull(irtypes.I8Ptr)
	sizeZero := constant.NewInt(irtypes.I64, 0)
	measureArgs := []value.Value{nullBuf, sizeZero, fmtPtr}
	measureArgs = append(measureArgs, args...)
	needed := block.NewCall(snprintfFn, measureArgs...) // i32
	neededI64 := block.NewSExt(needed, irtypes.I64)
	allocSize := block.NewAdd(neededI64, constant.NewInt(irtypes.I64, 1)) // +1 for NUL

	// Pass 2: allocate with ARC header and fill.
	buf := block.NewCall(cg.ensureRCAlloc(), allocSize)
	fillArgs := []value.Value{buf, allocSize, fmtPtr}
	fillArgs = append(fillArgs, args...)
	block.NewCall(snprintfFn, fillArgs...)

	fatPtrType := stringFatPtrType()
	fatAlloca := block.NewAlloca(fatPtrType)
	ptrGep := block.NewGetElementPtr(fatPtrType, fatAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	block.NewStore(buf, ptrGep)
	lenGep := block.NewGetElementPtr(fatPtrType, fatAlloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	block.NewStore(neededI64, lenGep)

	// ARC: release temporary RC-tracked values (::print strings, concat/call results)
	// now that snprintf has consumed their data pointers.
	for _, sv := range toRelease {
		cg.emitRelease(block, sv)
	}

	// Make sure the caller sees the final block when an arg-evaluation path
	// branched (e.g. fat-array -> string conversion emits its own loop blocks).
	cg.curBlock = block

	return block.NewLoad(fatPtrType, fatAlloca), nil
}

// Fiber expression helpers

// canonicalUnitStructName returns the LLVM struct name for the sync Unit type.
// After canonical naming, this is "sync__Unit" when sync was loaded from source.
// Falls back to "Unit" for pre-compiled .tin.mod scenarios.
func (cg *CodeGen) canonicalUnitStructName() string {
	// Prefer the canonical package-prefixed name.
	if cg.structTypeFor(CanonKey("sync__Unit")) != nil {
		return "sync__Unit"
	}
	// Try the type alias (covers pre-compiled mod scenarios).
	if alias := cg.aliasTypeFor(CanonKey("sync::Unit")); alias != nil {
		if simple, ok2 := alias.(*ast.SimpleType); ok2 {
			return simple.Name
		}
	}

	return "Unit"
}

// wrapPidInFuture wraps a fiber PID (i64) in a Future[T] struct value.
// calleeName is used to look up the original return type; pass "" for void/do-block spawns.
// Returns an error if the sync module is not available or Future[T] cannot be instantiated.
func (cg *CodeGen) wrapPidInFuture(block *ir.Block, pid value.Value, calleeName string) (value.Value, error) {
	// Determine the type parameter: original function's return type, or Unit for void.
	var retTypeExpr ast.TypeExpr

	if calleeName != "" {
		if origDecl, ok := cg.funcDecls[calleeName]; ok && origDecl.RetType != nil {
			retTypeExpr = origDecl.RetType
		}
	}

	if retTypeExpr == nil {
		retTypeExpr = &ast.SimpleType{Name: "Unit"}
	}

	// Get canonical name for the concrete type parameter (e.g., "i64", "string", "[]byte").
	// Use typeExprCanonicalKey rather than llvmTypeName(tinTypeToLLVM(...)) because
	// llvmTypeName cannot distinguish [byte] from string (both are {i8*, i64} fat ptrs).
	retTypeStr := cg.typeExprCanonicalKey(retTypeExpr)

	// Ensure Future[retType] is instantiated via on-demand monomorphization.
	futureConcreteName := "Future__" + retTypeStr
	if cg.structTypeFor(CanonKey(futureConcreteName)) == nil {
		futureASTType := &ast.GenericType{
			Name:       "Future",
			TypeParams: []ast.TypeExpr{retTypeExpr},
		}
		if _, monoErr := cg.tinTypeToLLVM(futureASTType); monoErr != nil {
			return nil, fmt.Errorf("spawn: cannot instantiate Future[%s]: %w", retTypeStr, monoErr)
		}
	}

	// Call Future[T].new(pid) to construct the struct value properly
	// (sets type_id, vtable pointer, and pid field).
	makeFnName := futureConcreteName + "_new"

	se, ok := cg.curScope.lookup(makeFnName)
	if !ok {
		if cg.syncLoadErr != nil {
			return nil, fmt.Errorf("spawn: sync package failed to load: %w; ensure the tin executable is alongside the stdlib/ directory", cg.syncLoadErr)
		}

		return nil, fmt.Errorf("spawn: Future[%s] not available - sync package could not be loaded; ensure the tin executable is alongside the stdlib/ directory, or add \"use sync\" explicitly before using spawn/await", retTypeStr)
	}

	makeFn, ok := se.val.(*ir.Func)
	if !ok {
		return nil, fmt.Errorf("spawn: %s is not a function", makeFnName)
	}

	return block.NewCall(makeFn, pid), nil
}

// genInlineAsyncDrive drives an {#async} function call inline within the
// current coroutine, without spawning a new fiber.
//
// Called when `await asyncFn(args)` is encountered (no spawn) and inCoroFn==true.
// Instead of allocating a fiber and joining it, we:
//  1. Call the $coro ramp to allocate only the inner coroutine frame.
//  2. Resume the inner coroutine until it completes.
//  3. Whenever the inner coroutine yields, yield the outer coroutine too -
//     the scheduler will resume both in turn.
//  4. When the inner coroutine is done, take its result via _tin_coro_take_result.
//
// Returns (nil, nil) when the callee has no $coro variant (not {#async});
// the caller should fall through to the regular spawn+join path.
