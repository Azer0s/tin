package codegen

import (
	"testing"

	"github.com/Azer0s/tin/ast"
)

// newTestCG builds a minimal CodeGen sufficient for typename tests.
// Only the maps the typename constructors read are populated; everything
// else stays zero-value so the test fails loudly if a code path touches
// machinery outside the contract.
func newTestCG() *CodeGen {
	cg := &CodeGen{
		types: map[CanonKey]*TypeRecord{},
	}

	cg.recordDisplay(CanonKey("errors__StringErr"), "errors::StringErr")
	cg.recordDisplay(CanonKey("net__udp__Conn"), "net::udp::Conn")
	cg.recordDisplay(CanonKey("sync__Unit"), "sync::Unit")
	// Traits register their bare entry plus the pkg-qualified canonical
	// form so callers asking for either path get the right display.
	cg.recordTrait(CanonKey("Err"), &ast.TraitDecl{Name: "Err"})
	cg.recordDisplay(CanonKey("errors__Err"), "errors::Err")
	cg.recordTrait(CanonKey("errors__Err"), &ast.TraitDecl{Name: "Err"})

	return cg
}

func TestTypeNameFromCanon_PrimitivePassesThrough(t *testing.T) {
	cg := newTestCG()
	tn := cg.typeNameFromCanon("i64")

	if tn.Pretty != "i64" || tn.Canon != "i64" || tn.LLVM != "i64" {
		t.Fatalf("primitive: got %+v, want all-equal i64", tn)
	}

	if tn.IsTraitIface() {
		t.Fatalf("primitive: IsTraitIface should be false")
	}
}

func TestTypeNameFromCanon_TraitGetsIfaceLLVM(t *testing.T) {
	cg := newTestCG()
	tn := cg.typeNameFromCanon("errors__Err")

	if tn.Canon != "errors__Err" {
		t.Errorf("Canon: got %q, want errors__Err", tn.Canon)
	}

	if tn.LLVM != "errors__Err_iface" {
		t.Errorf("LLVM: got %q, want errors__Err_iface", tn.LLVM)
	}

	if tn.Pretty != "errors::Err" {
		t.Errorf("Pretty: got %q, want errors::Err", tn.Pretty)
	}

	if !tn.IsTraitIface() {
		t.Errorf("IsTraitIface: should be true for trait")
	}
}

func TestTypeNameFromCanon_TraitIfaceInputNormalizes(t *testing.T) {
	cg := newTestCG()
	// Caller passed the LLVM form by accident; the constructor should
	// strip _iface and produce identical TypeName as if Canon was passed.
	tnFromLLVM := cg.typeNameFromCanon("errors__Err_iface")
	tnFromCanon := cg.typeNameFromCanon("errors__Err")

	if tnFromLLVM != tnFromCanon {
		t.Fatalf("ambiguous input should converge: %+v vs %+v",
			tnFromLLVM, tnFromCanon)
	}
}

func TestTypeNameFromCanon_RegisteredStructUsesDisplay(t *testing.T) {
	cg := newTestCG()
	tn := cg.typeNameFromCanon("net__udp__Conn")

	if tn.Pretty != "net::udp::Conn" {
		t.Errorf("Pretty: got %q, want net::udp::Conn", tn.Pretty)
	}
}

func TestPrettyFromCanon_GenericMonomorph(t *testing.T) {
	cg := newTestCG()
	cg.recordInstShape("Result__net__udp__Conn__errors__Err",
		"Result",
		[]ast.TypeExpr{
			&ast.SimpleType{Name: "net::udp::Conn"},
			&ast.SimpleType{Name: "errors::Err"},
		})

	tn := cg.typeNameFromCanon("Result__net__udp__Conn__errors__Err")
	want := "Result[net::udp::Conn, errors::Err]"

	if tn.Pretty != want {
		t.Errorf("nested generic display: got %q, want %q", tn.Pretty, want)
	}
}

func TestTypeNameFromExpr_RoundTripsCanon(t *testing.T) {
	cg := newTestCG()
	te := &ast.SimpleType{Name: "errors::Err"}
	tn := cg.typeNameFromExpr(te)

	if tn.Canon != "errors__Err" {
		t.Errorf("Canon: got %q, want errors__Err", tn.Canon)
	}

	if tn.LLVM != "errors__Err_iface" {
		t.Errorf("LLVM: got %q, want errors__Err_iface", tn.LLVM)
	}

	if tn.Pretty != "errors::Err" {
		t.Errorf("Pretty: got %q, want errors::Err", tn.Pretty)
	}
}
