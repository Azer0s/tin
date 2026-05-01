package codegen

// reflect_table.go - emit per-impl section entries for the link-time
// reflection table (D1 of incremental-compilation plan).
//
// Each `impl Trait for Struct` declared in a Tin package becomes one
// global of type {i32 type_id, i32 _reserved, i8* trait_name} in a
// custom section that runtime/reflect_table.c walks at startup. The
// section name varies by target triple: "tin_impl" on ELF (so the
// linker auto-synthesizes __start_tin_impl / __stop_tin_impl), or
// "__DATA,__tin_impl" on Mach-O (read at runtime via getsectiondata).
//
// Each per-pkg LLVM module also gets its own @llvm.compiler.used array
// pinning the impls it owns - without that pin, --gc-sections (lld) or
// -dead_strip (ld64) would drop entries that no IR code references
// (the runtime walks them by section, not by symbol).

import (
	"fmt"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	irtypes "github.com/llir/llvm/ir/types"
)

// implSectionStructType returns the LLVM struct matching TinImplEntry
// in runtime/reflect_table.c. Layout is fixed by the runtime ABI;
// changing it requires bumping both sides.
func (cg *CodeGen) implSectionStructType() *irtypes.StructType {
	return irtypes.NewStruct(irtypes.I32, irtypes.I32, irtypes.I8Ptr)
}

// implSectionAttr returns the section attribute string for the
// current target triple.
func (cg *CodeGen) implSectionAttr() string {
	if cg.targetIsDarwin() {
		return "__DATA,__tin_impl"
	}

	return "tin_impl"
}

// emitImplSectionEntry materializes one `impl traitName for structKey`
// as a section global in the current active module. Idempotent per
// (structKey, traitName, module): repeat calls in the same pkg-codegen
// pass do nothing.
func (cg *CodeGen) emitImplSectionEntry(structKey, traitName string) {
	id, ok := cg.structTypeIDs[structKey]
	if !ok {
		return
	}

	mod := cg.activeModule()

	if cg.implEntriesByMod == nil {
		cg.implEntriesByMod = map[*ir.Module][]*ir.Global{}
	}

	if cg.implEntriesSeen == nil {
		cg.implEntriesSeen = map[string]bool{}
	}

	key := fmt.Sprintf("%p|%s|%s", mod, structKey, traitName)
	if cg.implEntriesSeen[key] {
		return
	}

	cg.implEntriesSeen[key] = true

	entryTy := cg.implSectionStructType()

	// Trait name -> private NUL-terminated global, GEP'd to i8* for the
	// section entry.
	traitBytes := append([]byte(traitName), 0)
	traitArr := irtypes.NewArray(uint64(len(traitBytes)), irtypes.I8)
	traitConst := constant.NewCharArray(traitBytes)

	traitG := mod.NewGlobalDef(
		fmt.Sprintf("__tin_impl_trait_name__%s__%s",
			sanitizeImplSym(structKey), sanitizeImplSym(traitName)),
		traitConst,
	)
	traitG.Linkage = enum.LinkagePrivate
	traitG.Immutable = true
	traitG.UnnamedAddr = enum.UnnamedAddrUnnamedAddr

	traitGEP := constant.NewGetElementPtr(traitArr, traitG,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 0))
	traitGEP.InBounds = true

	entryConst := constant.NewStruct(entryTy,
		constant.NewInt(irtypes.I32, int64(id)),
		constant.NewInt(irtypes.I32, 0),
		traitGEP,
	)

	entryG := mod.NewGlobalDef(
		fmt.Sprintf("__tin_impl_entry__%s__%s",
			sanitizeImplSym(structKey), sanitizeImplSym(traitName)),
		entryConst,
	)
	entryG.Linkage = enum.LinkageInternal
	entryG.Section = cg.implSectionAttr()
	entryG.Align = 8

	cg.implEntriesByMod[mod] = append(cg.implEntriesByMod[mod], entryG)
}

// finalizeImplSection emits one @llvm.compiler.used per pkg module so
// linker dead-strip leaves the impl entries alone. Call once after all
// pkg codegen has run, before module serialization.
func (cg *CodeGen) finalizeImplSection() {
	for mod, entries := range cg.implEntriesByMod {
		if len(entries) == 0 {
			continue
		}

		used := make([]constant.Constant, 0, len(entries))
		for _, g := range entries {
			used = append(used, constant.NewBitCast(g, irtypes.I8Ptr))
		}

		usedArrTy := irtypes.NewArray(uint64(len(used)), irtypes.I8Ptr)
		usedInit := constant.NewArray(usedArrTy, used...)

		// llvm.used (NOT llvm.compiler.used) is the form the linker honors:
		// --gc-sections (lld) and -dead_strip (ld64) treat every symbol
		// listed here as a GC root, so the section data survives even when
		// no IR code names it. compiler.used only protects from optimizer
		// passes, not from the linker.
		usedG := mod.NewGlobalDef("llvm.used", usedInit)
		usedG.Linkage = enum.LinkageAppending
		usedG.Section = "llvm.metadata"
	}
}

// sanitizeImplSym maps Tin display names ("pkg::Type", "Foo[T]") into
// characters legal in an LLVM symbol name without losing identity.
func sanitizeImplSym(s string) string {
	r := strings.NewReplacer(
		"::", "__",
		"[", "_",
		"]", "_",
		" ", "_",
		",", "_",
		"*", "p",
		"&", "r",
		"(", "_",
		")", "_",
	)

	return r.Replace(s)
}
