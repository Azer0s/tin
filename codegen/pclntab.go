package codegen

// pclntab.go - emit a custom PC -> file:line:col table directly into the
// binary so runtime/stacktrace.c can resolve frames without DWARF or libdw.
//
// Layout per linked image (the main binary or a REPL cell .so):
//
//   section "tin_pclntab"  (ELF) / "__TIN,__pclntab" (Mach-O):
//     repeated TinPclnFnHdr structs, one per Tin function emitted into
//     this image. Each header carries direct pointers (resolved by the
//     loader under ASLR) to the function's name string, file string, and
//     PC->line:col table. The table itself is a separate `.rodata` global
//     so it can be sized per fn without bloating the header section.
//
//   private constants in `.rodata`:
//     - per-fn PC table: array of {i32 pc_off, i32 line, i32 col}
//     - per-string globals (Linkage=Private, UnnamedAddr) for names/files;
//       LLVM merges identical literals.
//
// Per-call PC-anchor splits (split_blocks_at_calls below) ensure every call
// instruction sits at the START of its own basic block, so blockaddress() of
// that block gives a precise PC for the call. fp_walk reports return-into-
// caller IPs, which we shift by -1 to land inside the call instruction; the
// PC anchor we record is the call's own PC, so the binary search lands on
// the correct entry.
//
// Constructor: codegen also emits an @llvm.global_ctors entry calling
// _tin_pclntab_register_self() at image-load time. The runtime resolves the
// per-image section bounds (Linux: __start_/__stop_ symbols; macOS:
// getsectiondata via dladdr) and adds them to a process-wide table. REPL
// cells get the same constructor automatically because they go through the
// same codegen pipeline.

import (
	"fmt"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
)

// pclntabPCEntryType is the {i8*, i32, i32} record stored per call-site
// in the per-function PC table. Field order: pc_addr (absolute address
// of the BB start under ASLR), source line, source column.
//
// We store the absolute address rather than a (pc_addr - fn_start)
// offset because PIC code (`-fPIC`, used for REPL cells and CTFE
// shims) cannot represent the cross-label subtraction at link time -
// the assembler errors with "Cannot represent a difference across
// sections". The runtime computes the offset on demand.
func (cg *CodeGen) pclntabPCEntryType() *irtypes.StructType {
	if cg.pclntabPCType == nil {
		cg.pclntabPCType = irtypes.NewStruct(irtypes.I8Ptr, irtypes.I32, irtypes.I32)
		cg.pclntabPCType.SetName("TinPclnPC")
		cg.mod.TypeDefs = append(cg.mod.TypeDefs, cg.pclntabPCType)
	}

	return cg.pclntabPCType
}

// pclntabFnHdrType is the per-function header record placed in the
// `tin_pclntab` section. Layout MUST stay in sync with TinPclnFnHdr in
// runtime/pclntab.c. Total size: 40 bytes on 64-bit (8+8+8+4+4+8).
//
//	{ i8* fn_start,
//	  i8* name,            ; pointer to .rodata cstring (no NUL needed)
//	  i8* file,            ; pointer to .rodata cstring
//	  i32 name_len,
//	  i32 file_len,
//	  TinPclnPC* pcs,      ; pointer to per-fn PC table (NULL for marker-only)
//	  i32 npcs }           ; 0 for marker-only headers (no source pos)
//
// The runtime computes max_pc_addr per-fn at registration time from the
// sorted pcs[] - no need to pre-bake it into the header.
func (cg *CodeGen) pclntabFnHdrType() *irtypes.StructType {
	if cg.pclntabHdrType == nil {
		pcArrPtr := irtypes.NewPointer(cg.pclntabPCEntryType())
		cg.pclntabHdrType = irtypes.NewStruct(
			irtypes.I8Ptr, // fn_start
			irtypes.I8Ptr, // name
			irtypes.I8Ptr, // file
			irtypes.I32,   // name_len
			irtypes.I32,   // file_len
			pcArrPtr,      // pcs
			irtypes.I32,   // npcs
		)
		cg.pclntabHdrType.SetName("TinPclnFnHdr")
		cg.mod.TypeDefs = append(cg.mod.TypeDefs, cg.pclntabHdrType)
	}

	return cg.pclntabHdrType
}

// pclntabSectionName returns the per-target section to attach pclntab
// globals to. ELF uses a plain C-identifier-style name so the linker
// synthesizes __start_/__stop_ symbols; Mach-O uses the SEGMENT,SECTION
// form (16-char limit on the section name itself).
func (cg *CodeGen) pclntabSectionName() string {
	if cg.targetIsDarwin() {
		return "__TIN,__pclntab"
	}

	return "tin_pclntab"
}

// targetIsDarwin reports whether the target triple targets macOS / iOS.
// Used to switch ELF/Mach-O specifics (section naming, constructor code).
func (cg *CodeGen) targetIsDarwin() bool {
	t := cg.mod.TargetTriple
	return strings.Contains(t, "darwin") ||
		strings.Contains(t, "macos") ||
		strings.Contains(t, "ios") ||
		strings.Contains(t, "apple")
}

// applyPclntabPostPass is the entry point called from Generate after every
// function has been emitted but before the IR is serialized. Order:
//
//  1. Split BBs so every call instruction sits at a BB start (anchor
//     points for blockaddress).
//  2. Walk each function and emit one per-call PC entry plus a header.
//  3. Emit the @llvm.global_ctors constructor that registers this image's
//     pclntab range with the runtime at load time.
//
// Gated on cg.stacktraceUsed: programs that don't reach stacktrace() pay
// neither the BB-split cost nor the section bytes.
func (cg *CodeGen) applyPclntabPostPass() {
	if !cg.stacktraceUsed {
		return
	}

	for _, f := range cg.allFuncs() {
		if f.Blocks == nil {
			continue
		}

		cg.splitBlocksAtCalls(f)
	}

	// Create the constructor BEFORE the marker loop so it gets its own
	// marker header. Without this, an IP that lands in the constructor
	// at runtime (rare but possible during fiber-spawn warmup that
	// captures a trace before all init returns) would be wrongly
	// attributed to the closest preceding fn in the binary search.
	cg.createPclntabConstructorFn()

	for _, f := range cg.allFuncs() {
		if f.Blocks == nil {
			continue
		}

		cg.emitPclntabForFn(f)
	}

	// Pin headers + emit the @llvm.global_ctors entry. This must run
	// AFTER the marker loop because llvm.used references the headers
	// (including the constructor's own marker) by name.
	cg.finalizePclntabConstructor()
}

// splitBlocksAtCalls walks fn.Blocks and ensures every InstCall is the
// FIRST instruction of its containing block. A block of the form
//
//	bb0: a; b; call f(); c; call g(); d; ret
//
// becomes
//
//	bb0:    a; b; br %bb0.split.0
//	bb0.split.0: call f(); c; br %bb0.split.1
//	bb0.split.1: call g(); d; ret
//
// blockaddress(@fn, %bb0.split.N) then yields the precise call PC for the
// pclntab entry. The optimizer is constrained to keep these blocks
// distinct (and not merge them with predecessors) once a blockaddress
// reference exists, but it can still reorder unrelated instructions
// within each block - the only cost is a per-call unconditional branch
// that codegen folds to a fall-through label in the final machine code.
//
// PHI fixup: when a block is split, its terminator (the one that
// branched to a successor with a PHI referring to bb) ends up in the
// LAST split block, not bb. We track bb -> lastSplit and rewrite every
// PHI's incoming label after splitting so the verifier sees the correct
// predecessor.
func (cg *CodeGen) splitBlocksAtCalls(fn *ir.Func) {
	out := make([]*ir.Block, 0, len(fn.Blocks))
	// Map original *Block -> the block that NOW carries the original
	// terminator (i.e. the one whose successors expect to see it as
	// their predecessor in PHIs).
	rewrite := map[*ir.Block]*ir.Block{}

	for _, bb := range fn.Blocks {
		split := cg.splitOneBlockAtCalls(bb)
		out = append(out, split...)

		if len(split) > 1 {
			rewrite[bb] = split[len(split)-1]
		}
	}

	fn.Blocks = out

	if len(rewrite) == 0 {
		return
	}

	for _, bb := range fn.Blocks {
		for _, inst := range bb.Insts {
			phi, ok := inst.(*ir.InstPhi)
			if !ok {
				continue
			}

			for _, inc := range phi.Incs {
				pred, ok := inc.Pred.(*ir.Block)
				if !ok {
					continue
				}

				if newPred, found := rewrite[pred]; found {
					inc.Pred = newPred
				}
			}
		}
	}
}

// splitOneBlockAtCalls implements the per-block split. Returns the
// (possibly multiple) blocks that replace bb. Blocks are returned in
// the order they should appear in fn.Blocks.
func (cg *CodeGen) splitOneBlockAtCalls(bb *ir.Block) []*ir.Block {
	// Skip blocks that have no terminator. Splitting transfers the
	// original terminator to the tail block; if there isn't one, we'd
	// produce an unterminated tail and crash llir/llvm at serialize
	// time. An unterminated bb means IR construction left it open
	// (typically transient mid-emission state) - leaving it alone is
	// correct: the unsplit form is no worse than the broken split form,
	// and the original codegen path will close it later.
	if bb.Term == nil {
		return []*ir.Block{bb}
	}

	// Find the first split point past index 0. If none, bb is unchanged.
	// Skip leading PHI nodes - they must remain at the top of their
	// containing block (LLVM rule: phi precedes any non-phi instruction).
	insts := bb.Insts
	firstNonPhi := 0
	for firstNonPhi < len(insts) {
		if _, isPhi := insts[firstNonPhi].(*ir.InstPhi); !isPhi {
			break
		}

		firstNonPhi++
	}

	splitAt := -1

	for i := firstNonPhi + 1; i < len(insts); i++ {
		if _, ok := insts[i].(*ir.InstCall); ok {
			splitAt = i

			break
		}
	}

	if splitAt < 0 {
		return []*ir.Block{bb}
	}

	// Form the tail: everything from splitAt onward, plus the original
	// terminator. Recurse on the tail to handle subsequent calls.
	tail := ir.NewBlock(fmt.Sprintf("%s.split.%d", bb.Name(), cg.pclntabSeq))
	cg.pclntabSeq++

	tail.Insts = append([]ir.Instruction{}, insts[splitAt:]...)
	tail.Term = bb.Term
	tail.Parent = bb.Parent

	bb.Insts = insts[:splitAt]
	bb.Term = ir.NewBr(tail)

	rest := cg.splitOneBlockAtCalls(tail)

	return append([]*ir.Block{bb}, rest...)
}

// emitPclntabForFn emits the per-fn header + PC table for one function.
// Walks fn's blocks; for each block whose first instruction is a call (or
// any instruction with !dbg), emits one PC entry { pc_off, line, col }
// where pc_off = blockaddress(@fn, %bb) - @fn (resolved at link time).
//
// The per-fn header references the PC table via direct pointer (i.e. the
// linker patches in the .rodata address) so we don't need section-relative
// offsets and dodge the Mach-O 16-char section-name limit for the PC table.
func (cg *CodeGen) emitPclntabForFn(fn *ir.Func) {
	// We emit a header for EVERY fn in the module (whether or not it
	// has Tin source context), but only attach PC entries when the fn
	// is source-tracked. The headers-without-PCs serve as upper-bound
	// markers in the resolver: an IP inside a compiler-generated
	// wrapper would otherwise fall into the slack range of an adjacent
	// source-tracked fn (typical: the C-side `main` wrapper sits right
	// after `_tin_user_main` in the binary). With its own header, the
	// binary search finds the wrapper directly, the per-fn PC table
	// is empty, and the resolver returns "no source" - falling through
	// to dladdr's `main+0x<off>` fallback at the right level.
	hasSource := cg.fnSourceFiles != nil
	if hasSource {
		_, hasSource = cg.fnSourceFiles[fn.Name()]
	}

	var pcEntries []constant.Constant

	fnPtrI8 := constant.NewBitCast(fn, irtypes.I8Ptr)

	if !hasSource {
		// Emit a marker-only header (no PC table) for fns without
		// source context. The resolver returns 0 on a hit here, which
		// routes the frame to the dladdr fallback in resolve_frame.
		hdrInit := constant.NewStruct(cg.pclntabFnHdrType(),
			fnPtrI8,
			constant.NewNull(irtypes.I8Ptr),
			constant.NewNull(irtypes.I8Ptr),
			constant.NewInt(irtypes.I32, 0),
			constant.NewInt(irtypes.I32, 0),
			constant.NewNull(irtypes.NewPointer(cg.pclntabPCEntryType())),
			constant.NewInt(irtypes.I32, 0),
		)
		id := cg.pclntabSeq
		cg.pclntabSeq++

		// Route hdr into the fn's owning module so blockaddress refs are
		// valid (LLVM forbids blockaddress in another module than the
		// fn). For the no-source marker case there's no blockaddress,
		// but keeping the convention simplifies the resolver assumption
		// "every hdr is in the same .o as the fn it points to."
		hdrMod := fnHomeModule(fn, cg.mod)
		hdr := hdrMod.NewGlobalDef(
			fmt.Sprintf("__tin_pcln_hdr.%d", id),
			hdrInit,
		)
		hdr.Section = cg.pclntabSectionName()
		hdr.Immutable = true
		hdr.UnnamedAddr = enum.UnnamedAddrUnnamedAddr
		hdr.Linkage = enum.LinkageInternal

		cg.pclntabHdrs = append(cg.pclntabHdrs, hdr)

		return
	}

	for i, bb := range fn.Blocks {
		if len(bb.Insts) == 0 {
			continue
		}

		line, col, ok := cg.firstInstSourcePos(bb)
		if !ok {
			continue
		}

		// LLVM forbids blockaddress on the entry block (LangRef:
		// "The address may not be taken of the entry block of a
		// function"). The entry block's runtime address IS the
		// function's address, so emit the fn pointer itself.
		var pcAddr constant.Constant

		if i == 0 {
			pcAddr = fnPtrI8
		} else {
			pcAddr = constant.NewBlockAddress(fn, bb)
		}

		entry := constant.NewStruct(cg.pclntabPCEntryType(),
			pcAddr,
			constant.NewInt(irtypes.I32, int64(line)),
			constant.NewInt(irtypes.I32, int64(col)),
		)
		pcEntries = append(pcEntries, entry)
	}

	if len(pcEntries) == 0 {
		// Function has no source-attributable IRs (compiler-generated
		// helper, all dbg loc=0). Skip - saves a header for thunks the
		// user never sees in a trace.
		return
	}

	// Per-fn PC table global. Use private linkage so it stays inside this
	// translation unit and gets DCE'd along with the fn header it backs
	// (linker --gc-sections traces the header -> table reference).
	id := cg.pclntabSeq
	cg.pclntabSeq++

	// Route the pcs / hdr / strings into the fn's OWNING module.
	// blockaddress(@fn, %bb) is only valid in the same module as @fn
	// (LLVM rejects it in a declaration). Pcs hold blockaddresses, so
	// the pcs global must live where the fn body lives.
	hdrMod := fnHomeModule(fn, cg.mod)

	pcArrTy := irtypes.NewArray(uint64(len(pcEntries)), cg.pclntabPCEntryType())
	pcArrInit := constant.NewArray(pcArrTy, pcEntries...)
	pcArr := hdrMod.NewGlobalDef(
		fmt.Sprintf("__tin_pcs.%d", id),
		pcArrInit,
	)
	pcArr.Linkage = enum.LinkagePrivate
	pcArr.Immutable = true
	pcArr.UnnamedAddr = enum.UnnamedAddrUnnamedAddr

	// Decay [N x %TinPclnPC] -> %TinPclnPC* via in-bounds GEP at index 0.
	pcArrPtr := constant.NewGetElementPtr(pcArrTy, pcArr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	pcArrPtr.InBounds = true

	name := cg.unmangleTinName(fn.Name())
	file := cg.fnSourceFile(fn)
	namePtr, nameLen := cg.pclntabStringInMod(hdrMod, name)
	filePtr, fileLen := cg.pclntabStringInMod(hdrMod, file)

	hdrInit := constant.NewStruct(cg.pclntabFnHdrType(),
		constant.NewBitCast(fn, irtypes.I8Ptr),
		namePtr,
		filePtr,
		constant.NewInt(irtypes.I32, int64(nameLen)),
		constant.NewInt(irtypes.I32, int64(fileLen)),
		pcArrPtr,
		constant.NewInt(irtypes.I32, int64(len(pcEntries))),
	)
	hdr := hdrMod.NewGlobalDef(
		fmt.Sprintf("__tin_pcln_hdr.%d", id),
		hdrInit,
	)
	hdr.Section = cg.pclntabSectionName()
	hdr.Immutable = true
	hdr.UnnamedAddr = enum.UnnamedAddrUnnamedAddr

	// Internal linkage keeps the symbol STB_LOCAL (unique per TU; no
	// cross-TU collisions when the same module gets compiled into both
	// the main binary and a REPL cell). The section content still goes
	// into tin_pclntab; --gc-sections pruning is countered by appending
	// to @llvm.used in finalizePclntabConstructor.
	hdr.Linkage = enum.LinkageInternal

	cg.pclntabHdrs = append(cg.pclntabHdrs, hdr)
}

// fnHomeModule returns the LLVM module that defines fn. fn.Parent is
// set by llir/llvm when fn is added via Module.NewFunc; for any fn
// codegen creates, it always reflects the owning module. Falls back to
// fallback if Parent is somehow nil (defensive — shouldn't happen for
// fns codegen produced).
func fnHomeModule(fn *ir.Func, fallback *ir.Module) *ir.Module {
	if fn != nil && fn.Parent != nil {
		return fn.Parent
	}

	return fallback
}

// pclntabStringInMod interns a UTF-8 string into mod and returns
// (i8* to first byte, byte length). Per-module dedup so each pkg
// module pays for its own copies; cross-module dedup happens at link
// time via weak_odr (same as runtime/newGlobalString).
func (cg *CodeGen) pclntabStringInMod(mod *ir.Module, s string) (constant.Constant, int) {
	if cg.pclntabStringPoolPerMod == nil {
		cg.pclntabStringPoolPerMod = map[*ir.Module]map[string]pclntabStringEntry{}
	}

	perMod, ok := cg.pclntabStringPoolPerMod[mod]
	if !ok {
		perMod = map[string]pclntabStringEntry{}
		cg.pclntabStringPoolPerMod[mod] = perMod
	}

	if g, ok := perMod[s]; ok {
		return g.ptr, g.len
	}

	data := []byte(s)
	arrTy := irtypes.NewArray(uint64(len(data)), irtypes.I8)
	ca := constant.NewCharArray(data)

	g := mod.NewGlobalDef(fmt.Sprintf("__tin_pcln_s.%d", cg.pclntabSeq), ca)
	g.Linkage = enum.LinkagePrivate
	g.Immutable = true
	g.UnnamedAddr = enum.UnnamedAddrUnnamedAddr

	cg.pclntabSeq++

	gep := constant.NewGetElementPtr(arrTy, g,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	gep.InBounds = true

	perMod[s] = pclntabStringEntry{ptr: gep, len: len(data)}

	return gep, len(data)
}

// pclntabString interns a UTF-8 string into a private global and returns
// (i8* to first byte, byte length). Reuses an existing global when the
// same content was interned earlier in this module.
func (cg *CodeGen) pclntabString(s string) (constant.Constant, int) {
	if g, ok := cg.pclntabStringPool[s]; ok {
		return g.ptr, g.len
	}

	if cg.pclntabStringPool == nil {
		cg.pclntabStringPool = map[string]pclntabStringEntry{}
	}

	data := []byte(s)
	arrTy := irtypes.NewArray(uint64(len(data)), irtypes.I8)
	ca := constant.NewCharArray(data)

	g := cg.mod.NewGlobalDef(fmt.Sprintf("__tin_pcln_s.%d", cg.pclntabSeq), ca)
	g.Linkage = enum.LinkagePrivate
	g.Immutable = true
	g.UnnamedAddr = enum.UnnamedAddrUnnamedAddr

	cg.pclntabSeq++

	gep := constant.NewGetElementPtr(arrTy, g,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	gep.InBounds = true

	cg.pclntabStringPool[s] = pclntabStringEntry{ptr: gep, len: len(data)}

	return gep, len(data)
}

// fnSourceFile returns the source file path for an LLVM function. Best
// effort: when the fn was not emitted from a .tin source (compiler-
// synthesized helper), returns "<runtime>".
func (cg *CodeGen) fnSourceFile(fn *ir.Func) string {
	if file, ok := cg.fnSourceFiles[fn.Name()]; ok && file != "" {
		return file
	}

	if cg.filename != "" {
		return cg.filename
	}

	return "<runtime>"
}

// firstInstSourcePos returns the (line, col) of the first instruction in
// bb that has a recorded source position, plus ok=true. Reads from
// cg.instLineCol (the side map populated at attach time, available even
// in non-debug builds), falling back to !dbg DILocation metadata when the
// side map has no entry (defensive: covers any inst attached via paths
// other than attachCurrentDbgLoc).
func (cg *CodeGen) firstInstSourcePos(bb *ir.Block) (int, int, bool) {
	for _, inst := range bb.Insts {
		if cg.instLineCol != nil {
			if pos, ok := cg.instLineCol[inst]; ok && pos.Line != 0 {
				return pos.Line, pos.Col, true
			}
		}

		if line, col, ok := instDbgLineCol(inst); ok {
			return line, col, true
		}
	}

	return 0, 0, false
}

// createPclntabConstructorFn creates the constructor function shell
// (added to cg.mod.Funcs) without yet wiring it into @llvm.global_ctors.
// We split this from finalizePclntabConstructor so the marker-emit loop
// in applyPclntabPostPass picks the constructor up and emits a marker
// header for it - preventing misattribution of any IP that lands in the
// constructor at runtime.
func (cg *CodeGen) createPclntabConstructorFn() {
	if cg.pclntabCtorFn != nil {
		return
	}

	hdrPtr := irtypes.NewPointer(cg.pclntabFnHdrType())
	registerImage := cg.ensureExternDecl(
		"_tin_pclntab_register_image",
		irtypes.Void,
		[]*ir.Param{
			ir.NewParam("start", hdrPtr),
			ir.NewParam("end", hdrPtr),
			ir.NewParam("marker", irtypes.I8Ptr),
		},
		false,
	)

	ctor := cg.mod.NewFunc("__tin_pclntab_ctor", irtypes.Void)
	ctor.Linkage = enum.LinkageInternal
	entry := ctor.NewBlock("entry")

	var startArg, endArg constant.Constant
	var markerArg constant.Constant

	if cg.targetIsDarwin() {
		startArg = constant.NewNull(hdrPtr)
		endArg = constant.NewNull(hdrPtr)
		markerArg = constant.NewBitCast(ctor, irtypes.I8Ptr)
	} else {
		startG := cg.mod.NewGlobal("__start_tin_pclntab", cg.pclntabFnHdrType())
		startG.Linkage = enum.LinkageExternWeak
		stopG := cg.mod.NewGlobal("__stop_tin_pclntab", cg.pclntabFnHdrType())
		stopG.Linkage = enum.LinkageExternWeak

		startArg = startG
		endArg = stopG
		markerArg = constant.NewNull(irtypes.I8Ptr)
	}

	entry.NewCall(registerImage, startArg, endArg, markerArg)
	entry.NewRet(nil)

	cg.pclntabCtorFn = ctor
}

// finalizePclntabConstructor pins all emitted headers (including the
// constructor's own marker, added by the loop after createPclntab-
// ConstructorFn) via @llvm.used and registers the constructor with
// @llvm.global_ctors so it fires at image-load time.
func (cg *CodeGen) finalizePclntabConstructor() {
	if cg.pclntabCtorFn == nil || len(cg.pclntabHdrs) == 0 {
		return
	}

	used := make([]constant.Constant, 0, len(cg.pclntabHdrs))
	for _, hdr := range cg.pclntabHdrs {
		used = append(used, constant.NewBitCast(hdr, irtypes.I8Ptr))
	}

	usedArrTy := irtypes.NewArray(uint64(len(used)), irtypes.I8Ptr)
	usedInit := constant.NewArray(usedArrTy, used...)
	usedG := cg.mod.NewGlobalDef("llvm.used", usedInit)
	usedG.Linkage = enum.LinkageAppending
	usedG.Section = "llvm.metadata"

	ctorFnPtrTy := irtypes.NewPointer(irtypes.NewFunc(irtypes.Void))
	ctorTy := irtypes.NewStruct(
		irtypes.I32,
		ctorFnPtrTy,
		irtypes.I8Ptr,
	)
	entryConst := constant.NewStruct(ctorTy,
		constant.NewInt(irtypes.I32, 65535),
		cg.pclntabCtorFn,
		constant.NewNull(irtypes.I8Ptr),
	)
	arrTy := irtypes.NewArray(1, ctorTy)
	arrInit := constant.NewArray(arrTy, entryConst)

	g := cg.mod.NewGlobalDef("llvm.global_ctors", arrInit)
	g.Linkage = enum.LinkageAppending
}

// pclntabStringEntry is one cached interned string in cg.pclntabStringPool.
type pclntabStringEntry struct {
	ptr constant.Constant
	len int
}

// unmangleTinName rewrites a Tin codegen-mangled IR symbol name back into a
// human-readable form for display in stacktraces. Resolution order:
//
//  1. Look up cg.fnDisplayNames (populated at predeclare time from AST
//     context - pkg, struct receiver, original method name). This is the
//     source of truth when the IR fn was emitted from a Tin FuncDecl.
//  2. Fall back to a heuristic transform for IR fns we never recorded
//     a display name for (compiler-generated thunks, monomorphized
//     generics whose template wasn't predeclared, $coro variants):
//       - `_tin_user_main` -> `main`
//       - leading `_tin_` / `__tin_` (runtime helpers) -> kept as-is
//       - `name$coro` -> recurse on `name` then append `$coro`
//       - generic-mono `tmpl__inst` is best-effort: stays as `tmpl::inst`
//         since we can't distinguish it from a pkg-qualified name without
//         the AST.
func (cg *CodeGen) unmangleTinName(name string) string {
	if cg.fnDisplayNames != nil {
		if d, ok := cg.fnDisplayNames[name]; ok {
			return d
		}
	}

	return unmangleTinNameHeuristic(name)
}

// unmangleTinNameHeuristic is the fallback transform when no recorded
// display name exists. Handles compiler-generated thunks and the $coro
// variant suffix.
func unmangleTinNameHeuristic(name string) string {
	if name == "_tin_user_main" {
		return "main"
	}

	if strings.HasPrefix(name, "__tin_") || strings.HasPrefix(name, "_tin_") {
		return name
	}

	if strings.HasSuffix(name, "$coro") {
		base := name[:len(name)-len("$coro")]
		return unmangleTinNameHeuristic(base) + "$coro"
	}

	return strings.ReplaceAll(name, "__", "::")
}

// recordFnDisplayName builds and stores a user-visible display name for the
// given IR-mangled fn name, derived from the original AST node. Called from
// genFuncDecl right after the *ir.Func is created so the AST context (pkg,
// struct receiver, original source name) is in hand.
//
// Display rules (matching Tin source syntax):
//   - top-level fn `foo` in pkg `bar`           -> `bar::foo`
//   - top-level fn `foo` no pkg                 -> `foo`
//   - method `m` on struct `S` in pkg `p`       -> `p::S.m`
//   - method `m` on struct `S` no pkg           -> `S.m`
//   - $coro variants get the suffix appended at lookup time
//     (we register the base fn's display; the $coro IR fn is stored
//     separately when codegen creates it).
//
// We DON'T attempt to demangle generic instantiation suffixes here -
// monomorphizeFunc creates a fresh FuncDecl with name `tmpl__inst` that
// re-enters predeclare; the recursive call records its own (already-
// mangled) form. A future pass could capture the type-arg substitution
// and render `name[i64]`-style; for now generics fall back to the
// heuristic transform.
func (cg *CodeGen) recordFnDisplayName(irName string, n *ast.FuncDecl) {
	if n == nil || irName == "" {
		return
	}

	if cg.fnDisplayNames == nil {
		cg.fnDisplayNames = map[string]string{}
	}

	srcName := n.Name
	if srcName == "main" && !n.IsStatic {
		// Mirror the _tin_user_main rename's display side: the user's
		// `fn main` shows up as `main`, not the IR-mangled form.
		srcName = "main"
	}

	// Strip a leading `pkg__` prefix from the receiver struct name when
	// the same pkg is already going to be emitted as the qualifier - the
	// receiver names imported from another package come pre-qualified
	// (e.g. `sync__AtomicI64`) and we don't want the package to appear
	// twice (`sync::sync__AtomicI64.deinit`).
	recv := cg.curMethodReceiverStruct
	if cg.currentPkg != "" && strings.HasPrefix(recv, cg.currentPkg+"__") {
		recv = recv[len(cg.currentPkg)+2:]
	}

	// Trait-qualified methods get their trait baked into the symbol
	// segment using `::` (matching Tin source syntax) so that the
	// stdlib source parser (which splits the atom on `@` and never on
	// spaces) sees a clean identifier in the symbol field. Without
	// this disambiguation, all `read` methods on `MyReader` -
	// intrinsic plus each trait impl - would render identically and
	// collide in trace consumers that key on symbol name.
	var symBase string
	if n.TraitQualifier != "" {
		symBase = n.TraitQualifier + "::" + srcName
	} else {
		symBase = srcName
	}

	var display string

	switch {
	case recv != "" && cg.currentPkg != "":
		display = cg.currentPkg + "::" + recv + "." + symBase
	case recv != "":
		display = recv + "." + symBase
	case cg.currentPkg != "":
		display = cg.currentPkg + "::" + symBase
	default:
		display = symBase
	}

	cg.fnDisplayNames[irName] = display

	// Mirror the entry under the $coro variant name so the coro entry
	// point shows up correctly without an extra recordFnDisplayName call
	// at coro-emit time.
	cg.fnDisplayNames[irName+"$coro"] = display + "$coro"
}
