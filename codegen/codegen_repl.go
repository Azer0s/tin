package codegen

import (
	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
)

type ReplGlobal struct {
	Name     string
	TinType  ast.TypeExpr // from the VarDecl; nil if type was inferred
	LLVMType irtypes.Type
}

// SetReplMode enables REPL code generation. cellFuncName is the IR name of
// the current cell entry function (e.g. "_repl_cell_3"). In REPL mode,
// top-level `let` bindings inside cellFuncName are promoted to LLVM globals
// and main() generation is skipped.
func (cg *CodeGen) SetReplMode(cellFuncName string) {
	cg.replMode = true
	cg.replCellFuncName = cellFuncName
	cg.replCellGlobals = make(map[string]*ir.Global)
}

// SetReplExternalGlobals marks names as externally-defined (from prior REPL cells).
// preregisterTopLevelVar will emit these as 'external' linkage so RTLD_GLOBAL
// resolves them to the canonical copy instead of creating a new definition.
func (cg *CodeGen) SetReplExternalGlobals(names []string) {
	cg.replExternalGlobals = make(map[string]bool, len(names))
	for _, n := range names {
		cg.replExternalGlobals[n] = true
	}
}

// ReplNewGlobals returns globals promoted from `let` bindings in this cell.
func (cg *CodeGen) ReplNewGlobals() []ReplGlobal { return cg.replNewGlobals }

// ReplGlobalTinTypeName returns the Tin source type name for a promoted global,
// or "" if the type cannot be reliably reconstructed from the LLVM type.
func (cg *CodeGen) ReplGlobalTinTypeName(g ReplGlobal) string {
	if g.TinType != nil {
		return g.TinType.String()
	}
	// Prefer the structural reconstructor: it preserves pointer shape
	// and demangles `<pkg>__<Trait>_iface` (vtable-typed second slot)
	// back to `<pkg>::<Trait>`, so that a let-binding of
	// `errors::new("x")` (whose LLVM type is `*errors__Err_iface`) is
	// re-injected into the next cell as `*errors::Err` rather than the
	// unresolvable `*errors::Err_iface`.
	te := llvmTypeToTinTypeExprStructural(g.LLVMType)
	if te != nil {
		if name := te.String(); name != "" && name != "any" {
			return name
		}
	}

	n := llvmTypeToTinName(g.LLVMType)
	if n == "any" {
		return "" // unresolvable - skip global registration
	}

	return n
}
