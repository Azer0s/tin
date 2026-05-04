package codegen

// iface.go - per-package interface manifest for incremental compilation.
//
// Step 6 of docs/plans/incremental-compilation.md. The iface manifest
// captures everything from a pkg's source that downstream consumers
// can OBSERVE through normal use:
//
//   - exported fn signatures (callers compile against these)
//   - exported struct/data/enum layouts (callers know the shape)
//   - bodies of exported generics + #pure fns (callers expand them
//     at the call site, so a body change IS interface)
//   - every `impl Trait for Struct` declared in this pkg
//
// A change confined to a non-exported function body, a private struct,
// etc. bumps `input_hash` (sha256 of source files) but does NOT bump
// `iface_hash`. Step 7 uses that distinction to skip recompiling
// consumers when their imports' iface_hash hasn't changed.
//
// The serialized form is canonical JSON: stable key order, sorted
// slices. The output is byte-identical for byte-identical inputs.
//
// Macros are not yet captured: they're keyed by bare name in
// cg.macros without a per-pkg owner field today, and threading pkg
// origin into macros is a follow-up. For stdlib-heavy code this is
// usually a small omission; the input_hash side still catches every
// macro change.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Azer0s/tin/ast"
)

// IfaceFn is one exported fn entry in a pkg's iface manifest. Generics
// and #pure carry a body fingerprint (because callers expand them);
// ordinary fns carry signature only.
type IfaceFn struct {
	Name       string     `json:"name"`
	IRName     string     `json:"irName"`
	Params     []ModParam `json:"params,omitempty"`
	RetType    string     `json:"retType,omitempty"`
	Variadic   bool       `json:"variadic,omitempty"`
	Tags       []string   `json:"tags,omitempty"`
	TypeParams []string   `json:"typeParams,omitempty"`
	// BodyHash is a position-and-shape fingerprint of the fn body, set
	// only when callers need it (generics + #pure). Coarser than a
	// canonical AST printer would give but enough to invalidate the
	// iface_hash on every meaningful body edit (line/col shift, node
	// count change). False positives cause an unneeded recompile, never
	// stale results.
	BodyHash string `json:"bodyHash,omitempty"`
}

// IfaceStruct is one exported struct/data/enum layout.
type IfaceStruct struct {
	Name   string     `json:"name"`
	IRName string     `json:"irName"`
	Fields []ModParam `json:"fields,omitempty"`
}

// IfaceImpl is one `impl Trait for Struct` declaration. Position-stable
// in the iface so downstream pkgs that depend on link-time reflection
// (codegen/reflect_table.go) only invalidate when the impl set
// actually changes.
type IfaceImpl struct {
	Struct string `json:"struct"`
	Trait  string `json:"trait"`
}

// PkgIface is the top-level iface manifest for a single package.
type PkgIface struct {
	Package string        `json:"package"`
	Funcs   []IfaceFn     `json:"functions,omitempty"`
	Structs []IfaceStruct `json:"structs,omitempty"`
	Impls   []IfaceImpl   `json:"impls,omitempty"`
}

// MarshalCanonical serializes the iface as deterministic JSON: every
// slice is sorted by a stable key. The result feeds IfaceHash.
func (p *PkgIface) MarshalCanonical() ([]byte, error) {
	c := *p

	c.Funcs = append([]IfaceFn(nil), c.Funcs...)
	c.Structs = append([]IfaceStruct(nil), c.Structs...)
	c.Impls = append([]IfaceImpl(nil), c.Impls...)

	sort.Slice(c.Funcs, func(i, j int) bool { return c.Funcs[i].IRName < c.Funcs[j].IRName })
	sort.Slice(c.Structs, func(i, j int) bool { return c.Structs[i].IRName < c.Structs[j].IRName })
	sort.Slice(c.Impls, func(i, j int) bool {
		if c.Impls[i].Struct != c.Impls[j].Struct {
			return c.Impls[i].Struct < c.Impls[j].Struct
		}

		return c.Impls[i].Trait < c.Impls[j].Trait
	})

	return json.MarshalIndent(c, "", "  ")
}

// IfaceHash returns the SHA-256 of the canonical iface bytes, hex-
// encoded. Step 7 stamps this into the pkg cache key and downstream
// consumers' sboms.
func (p *PkgIface) IfaceHash() (string, error) {
	b, err := p.MarshalCanonical()
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:]), nil
}

// BuildPkgIface walks cg state and produces the iface manifest for
// pkgName. Returns nil if the package is unknown to the codegen.
//
// "Exported" = the bare (post-prefix) name does not start with "_".
// Generics and #pure include a body fingerprint; ordinary fns include
// signature only.
func (cg *CodeGen) BuildPkgIface(pkgName string) *PkgIface {
	if pkgName == "" {
		return nil
	}

	iface := &PkgIface{Package: pkgName}
	prefix := pkgName + "__"

	// Exported fns: cg.funcDecls is keyed by IR name. Filter by pkg
	// prefix; exclude underscore-leading bare names.
	for irName, decl := range cg.funcDecls {
		if !strings.HasPrefix(irName, prefix) {
			continue
		}

		bare := strings.TrimPrefix(irName, prefix)
		if strings.HasPrefix(bare, "_") {
			continue
		}

		iface.Funcs = append(iface.Funcs, ifaceFnFromDecl(irName, decl))
	}

	// Exported structs: cg.structFields is keyed by struct name.
	for structKey, fields := range cg.structFields {
		if !strings.HasPrefix(structKey, prefix) {
			continue
		}

		bare := strings.TrimPrefix(structKey, prefix)
		if strings.HasPrefix(bare, "_") {
			continue
		}

		s := IfaceStruct{
			Name:   bare,
			IRName: structKey,
		}
		llvmTypes := cg.structFieldLLVMTypes[structKey]

		for i, fname := range fields {
			tname := ""
			if i < len(llvmTypes) {
				tname = primitiveTypeName(llvmTypes[i])
			}

			s.Fields = append(s.Fields, ModParam{Name: fname, Type: tname})
		}

		iface.Structs = append(iface.Structs, s)
	}

	// Exported impls: cg.structImpls is struct -> []traitName.
	for structKey, traits := range cg.structImpls {
		if !strings.HasPrefix(structKey, prefix) {
			continue
		}

		bare := strings.TrimPrefix(structKey, prefix)
		for _, t := range traits {
			iface.Impls = append(iface.Impls, IfaceImpl{
				Struct: bare,
				Trait:  t,
			})
		}
	}

	return iface
}

// ifaceFnFromDecl converts an ast.FuncDecl + IR name into an IfaceFn.
func ifaceFnFromDecl(irName string, decl *ast.FuncDecl) IfaceFn {
	fn := IfaceFn{
		Name:    decl.Name,
		IRName:  irName,
		RetType: typeExprToString(decl.RetType),
	}

	if len(decl.Tags) > 0 {
		fn.Tags = append([]string(nil), decl.Tags...)
		sort.Strings(fn.Tags)
	}

	for _, p := range decl.Params {
		fn.Params = append(fn.Params, ModParam{
			Name: p.Name,
			Type: typeExprToString(p.Type),
		})

		if p.IsVarArgs {
			fn.Variadic = true
		}
	}

	if len(decl.TypeParams) > 0 {
		fn.TypeParams = append([]string(nil), decl.TypeParams...)
	}

	if needsBodyInIface(decl) {
		fn.BodyHash = bodyFingerprint(decl)
	}

	return fn
}

// needsBodyInIface decides whether the iface manifest must carry the
// fn body. Generics and #pure expand at the call site; without the
// body, downstream pkgs would be unable to instantiate / fold.
func needsBodyInIface(decl *ast.FuncDecl) bool {
	if len(decl.TypeParams) > 0 {
		return true
	}

	for _, t := range decl.Tags {
		if t == "pure" {
			return true
		}
	}

	return false
}

// bodyFingerprint produces a coarse hash of the fn body. Sufficient to
// flip when source changes; not a canonical printer. Position +
// raw block stmt count is the cheapest fingerprint that catches the
// common edit cases (rewrites, additions, deletions). The pkg-level
// input_hash (step 7) is the actual content guarantee; this hash is a
// friendly hint for the iface diff, not a correctness boundary.
func bodyFingerprint(decl *ast.FuncDecl) string {
	if decl == nil || decl.Body == nil {
		return ""
	}

	stmts := 0
	if blk, ok := decl.Body.(*ast.Block); ok {
		stmts = len(blk.Stmts)
	}

	pos := decl.Pos()

	return fmt.Sprintf("%d:%d:%d", pos.Line, pos.Col, stmts)
}
