package codegen

// TypeName is a tri-form representation of a Tin type identity used across
// codegen.  See docs/plans/typename-refactor.md for the motivation; the
// short version is that one `string` parameter standing in for three
// different concepts (display, monomorph key, LLVM struct name) is the
// root cause of recurring trait-iface ambiguity bugs.  This struct fixes
// the ambiguity by carrying all three forms together; constructors keep
// the fields in sync.
//
// TypeRecord (below) is the next layer: instead of N parallel maps each
// keyed by a raw string ("structTypes", "dataDecls", "traits", ...),
// codegen tracks one record per Tin type and accesses every aspect
// through that record.  The registry is populated alongside the
// existing maps; the per-aspect maps are migrated away from
// incrementally.
//
// Construct only via the typeNameFrom* helpers below.

import (
	"fmt"
	"strings"

	"github.com/llir/llvm/ir"
	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
)

// CanonKey is the canonical-form string used as a map key for type
// records.  Distinct from `string` so map accesses at the type level
// can't be made with the wrong form (Pretty/LLVM) by accident.
type CanonKey string

// TypeRecord owns everything codegen knows about one Tin type.
// Today this info lives in ~10 parallel maps; consolidating gives a
// single source of truth per type, accessed via a handle.
//
// Construction is via cg.upsertTypeRecord (which gets-or-creates by
// CanonKey); mutation happens at the existing write sites that
// register the type in the per-aspect maps.  Writes populate the
// record alongside the old maps; readers are being migrated from the
// maps to the record incrementally.
type TypeRecord struct {
	// Canonical identity.  Set at first registration; immutable after.
	Canon  CanonKey
	Pretty string

	// LLVM lowering.  Set when the type is lowered.  Iface and Vtable
	// are non-nil only for trait types; LLVM is the user-visible IR
	// type (the struct itself for data/enum/struct, the iface fat-ptr
	// struct for traits used as a value).
	LLVM   irtypes.Type
	Iface  *irtypes.StructType
	Vtable *irtypes.StructType

	// AST-side.  Mutually exclusive: a record is at most one of
	// Data / Trait / Alias / Enum.  Held nil for primitives.
	Data   *ast.DataDecl
	Trait  *ast.TraitDecl
	Alias  ast.TypeExpr
	IsEnum bool

	// Monomorphization (for generic struct/data instances).
	TmplName  string         // template name (e.g. "Result"); empty for non-monomorphs
	InstArgs  []ast.TypeExpr // original TypeExpr args (preserves nested generic shape)
	InstParts []string       // canonical-key arg names ([]{"i64", "errors__Err"})

	// Aliases: per-source-file name(s) by which this type is reachable.
	// Lets diagnostics render the form the user wrote and lets future
	// scope-aware resolution find the record by a file-local short name.
	// Key: source file path (cg.filename).  Value: name as written there.
	Aliases map[string]string

	// constrainedFuncInstances per-type, etc. -- additional facets
	// land here as the registry grows.
}

// upsertTypeRecord returns the TypeRecord for canon, creating an empty
// one if absent.  Callers mutate the returned record to register new
// aspects (LLVM type, AST decl, alias entry, ...).
//
// The returned pointer is stable for the lifetime of the CodeGen, so
// callers can stash it and avoid repeated map lookups -- the eventual
// endpoint is for TypeName to carry the *TypeRecord directly.
func (cg *CodeGen) upsertTypeRecord(canon CanonKey) *TypeRecord {
	if cg.types == nil {
		cg.types = make(map[CanonKey]*TypeRecord)
	}

	if r, ok := cg.types[canon]; ok {
		return r
	}

	r := &TypeRecord{Canon: canon, Aliases: map[string]string{}}
	cg.types[canon] = r

	return r
}

// recordAlias notes that the current source file refers to the type
// identified by canon as alias.  Used by package-import paths to record
// the per-file shorthand (e.g. `Conn` for `net__udp__Conn` inside
// `udp.tin`).  Safe to call repeatedly; later registrations overwrite
// the same file's entry (last-write-wins).
func (cg *CodeGen) recordAlias(canon CanonKey, alias string) {
	r := cg.upsertTypeRecord(canon)
	if alias != "" && cg.filename != "" {
		r.Aliases[cg.filename] = alias
	}
}

// recordLLVM stores t as the LLVM type for canon in the registry.
// Seeds Pretty from the canonical name when no pkg-qualified display
// was already registered.  This covers bare user-level structs whose
// canonical key equals their source-form name (`Box`, `Pair`, ...);
// pkg-qualified structs go through pkgStructKey which calls
// recordDisplay first, so r.Pretty is already set when this fires.
func (cg *CodeGen) recordLLVM(canon CanonKey, t irtypes.Type) {
	r := cg.upsertTypeRecord(canon)
	r.LLVM = t

	if r.Pretty == "" {
		r.Pretty = string(canon)
	}
}

// prettyTraitInst returns the user-facing source form of a trait
// instantiation -- "Seq[i64]" / "Map[string, i64]" -- given the trait's
// bare name and the AST type-args used to instantiate it.  Used at
// trait-iface registration so diagStructName(instKey) renders the
// bracketed pretty form rather than the mangled "Seq__i64" canon.
func (cg *CodeGen) prettyTraitInst(traitName string, args []ast.TypeExpr) string {
	if len(args) == 0 {
		if p := cg.prettyOf(CanonKey(traitName)); p != "" {
			return p
		}

		return traitName
	}

	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = cg.typeNameFromExpr(a).Pretty
	}

	headPretty := traitName
	if p := cg.prettyOf(CanonKey(traitName)); p != "" {
		headPretty = p
	}

	return headPretty + "[" + strings.Join(parts, ", ") + "]"
}

// recordTraitIface stores a trait's fat-pointer + vtable structs alongside
// the registry record.  Mirrors the traitFatPtrTypes + traitVtableStructTypes
// writes at trait instantiation.
func (cg *CodeGen) recordTraitIface(canon CanonKey, iface, vtable *irtypes.StructType) {
	r := cg.upsertTypeRecord(canon)

	if iface != nil {
		r.Iface = iface
	}

	if vtable != nil {
		r.Vtable = vtable
	}
}

// recordData links a DataDecl AST node to its canonical record.  Used
// at template registration (n.Name = "Result") and at monomorph
// registration (concreteName = "Result__i64__errors__Err").  Seeds
// Pretty from the decl name when the record doesn't already have a
// pkg-qualified Pretty registered.
func (cg *CodeGen) recordData(canon CanonKey, d *ast.DataDecl) {
	r := cg.upsertTypeRecord(canon)
	r.Data = d

	if r.Pretty == "" {
		r.Pretty = d.Name
	}
}

// recordTrait links a TraitDecl to its canonical record.  Seeds Pretty
// from the trait's bare name when no pkg-qualified display was already
// registered.
func (cg *CodeGen) recordTrait(canon CanonKey, t *ast.TraitDecl) {
	r := cg.upsertTypeRecord(canon)
	r.Trait = t

	if r.Pretty == "" {
		r.Pretty = t.Name
	}
}

// recordAliasType stores an AST type-alias target on the record.  Mirrors
// `cg.typeAliases[name] = target`; the source-form `name` is added to
// the canonical target's per-file Aliases map for diagnostic rendering.
func (cg *CodeGen) recordAliasType(canon CanonKey, target ast.TypeExpr) {
	r := cg.upsertTypeRecord(canon)
	r.Alias = target
}

// setTypeAlias writes `name -> target` into the registry.  Single
// entry-point for static alias writes (per-file imports, type-decl
// aliases, the rune builtin).
func (cg *CodeGen) setTypeAlias(name string, target ast.TypeExpr) {
	cg.recordAliasType(CanonKey(name), target)
}

// pushAlias writes `name -> target` and returns the previous value so the
// matching popAlias call can restore it.  Used by monomorphization to
// substitute generic params transiently -- the alias is in scope only for
// the duration of the monomorph body.
func (cg *CodeGen) pushAlias(name string, target ast.TypeExpr) (prev ast.TypeExpr, hadPrev bool) {
	if r, ok := cg.types[CanonKey(name)]; ok && r.Alias != nil {
		prev = r.Alias
		hadPrev = true
	}

	cg.recordAliasType(CanonKey(name), target)

	return prev, hadPrev
}

// popAlias restores (or clears) the alias for name, undoing the prior
// pushAlias.
func (cg *CodeGen) popAlias(name string, prev ast.TypeExpr, hadPrev bool) {
	if hadPrev {
		cg.recordAliasType(CanonKey(name), prev)

		return
	}

	if r, ok := cg.types[CanonKey(name)]; ok {
		r.Alias = nil
	}
}

// recordInstShape stores the monomorphization shape for a generic
// instance.  Also computes and stores the user-facing Pretty form once,
// using the already-registered Pretty of each Arg's record.  Reading
// r.Pretty back is then O(1); no recursive walk.
func (cg *CodeGen) recordInstShape(canon CanonKey, tmplName string, args []ast.TypeExpr) {
	r := cg.upsertTypeRecord(canon)
	r.TmplName = tmplName
	r.InstArgs = args

	parts := make([]string, len(args))
	for i, arg := range args {
		parts[i] = cg.prettyForExpr(arg)
	}

	tmplPretty := cg.prettyOf(CanonKey(tmplName))
	if tmplPretty == "" {
		tmplPretty = tmplName
	}

	r.Pretty = tmplPretty + "[" + strings.Join(parts, ", ") + "]"
}

// recordInstParts stores the canonical-key arg names for a generic
// instance.  Mirrors `cg.dataInstTypeArgs[canon] = parts`.
func (cg *CodeGen) recordInstParts(canon CanonKey, parts []string) {
	r := cg.upsertTypeRecord(canon)
	r.InstParts = parts
}

// recordEnum stores an enum's base type on the record.  Pretty seeds
// from the canonical name when no pkg-qualified display was already
// registered.
func (cg *CodeGen) recordEnum(canon CanonKey, base irtypes.Type) {
	r := cg.upsertTypeRecord(canon)
	r.LLVM = base
	r.IsEnum = true

	if r.Pretty == "" {
		r.Pretty = string(canon)
	}
}

// recordDisplay stores the canonical Pretty name on the record.  Called
// at the same place pkgStructKey writes `cg.structDisplayNames[key] = display`.
func (cg *CodeGen) recordDisplay(canon CanonKey, pretty string) {
	r := cg.upsertTypeRecord(canon)
	r.Pretty = pretty
}

// prettyOf returns the stored display name for canon, or "" when no
// record is registered.  Single source of truth -- never reconstructs.
func (cg *CodeGen) prettyOf(canon CanonKey) string {
	if r, ok := cg.types[canon]; ok {
		return r.Pretty
	}

	return ""
}

// prettyForExpr returns the display form for an AST TypeExpr.  Walks
// the surface structure (`*T`, `[T]`, `Gen[T1, T2]`, `SimpleType`) and
// looks up each leaf's Pretty in the registry.  Used at registration
// time (recordInstShape) to seed monomorph Pretty values once; readers
// then go through r.Pretty directly.  DO NOT use as a general
// "compute Pretty from a string" helper -- that's the error-prone
// pattern the registry is meant to eliminate.
func (cg *CodeGen) prettyForExpr(te ast.TypeExpr) string {
	switch t := te.(type) {
	case *ast.SimpleType:
		canon := cg.typeExprCanonicalKey(te)
		if p := cg.prettyOf(CanonKey(canon)); p != "" {
			return p
		}

		return t.Name
	case *ast.GenericType:
		parts := make([]string, len(t.TypeParams))
		for i, tp := range t.TypeParams {
			parts[i] = cg.prettyForExpr(tp)
		}

		name := t.Name
		if p := cg.prettyOf(CanonKey(name)); p != "" {
			name = p
		}

		return name + "[" + strings.Join(parts, ", ") + "]"
	case *ast.PointerType:
		return "*" + cg.prettyForExpr(t.Elem)
	case *ast.ArrayType:
		return "[" + cg.prettyForExpr(t.Elem) + "]"
	}

	return te.String()
}

// Typed read accessors over the registry.  Each returns the value
// stored on the TypeRecord for canon, or the zero value if no record
// exists (matching the existing map-lookup semantics).  Read sites
// are migrated from the per-aspect maps to these accessors so the
// maps can eventually be deleted.

// llvmTypeFor returns the LLVM type for canon, or nil if unknown.
func (cg *CodeGen) llvmTypeFor(canon CanonKey) irtypes.Type {
	if r, ok := cg.types[canon]; ok {
		return r.LLVM
	}

	return nil
}

// structTypeFor returns the registered LLVM struct type for canon, or
// nil.  Narrows llvmTypeFor's return when the caller specifically needs
// a struct (the most common shape in codegen).
func (cg *CodeGen) structTypeFor(canon CanonKey) *irtypes.StructType {
	t := cg.llvmTypeFor(canon)
	if t == nil {
		return nil
	}

	st, _ := t.(*irtypes.StructType)

	return st
}

// dataDeclFor returns the DataDecl for canon, or nil.
func (cg *CodeGen) dataDeclFor(canon CanonKey) *ast.DataDecl {
	if r, ok := cg.types[canon]; ok {
		return r.Data
	}

	return nil
}

// traitFor returns the TraitDecl for canon, or nil.
func (cg *CodeGen) traitFor(canon CanonKey) *ast.TraitDecl {
	if r, ok := cg.types[canon]; ok {
		return r.Trait
	}

	return nil
}

// ifaceFor returns the trait fat-pointer struct for canon, or nil.
func (cg *CodeGen) ifaceFor(canon CanonKey) *irtypes.StructType {
	if r, ok := cg.types[canon]; ok {
		return r.Iface
	}

	return nil
}

// vtableFor returns the trait vtable struct for canon, or nil.
func (cg *CodeGen) vtableFor(canon CanonKey) *irtypes.StructType {
	if r, ok := cg.types[canon]; ok {
		return r.Vtable
	}

	return nil
}

// instShapeFor returns the monomorphization shape for canon (template
// name + structured TypeExpr args) and whether one is recorded.
func (cg *CodeGen) instShapeFor(canon CanonKey) (instShape, bool) {
	if r, ok := cg.types[canon]; ok && r.TmplName != "" {
		return instShape{Tmpl: r.TmplName, Args: r.InstArgs}, true
	}

	return instShape{}, false
}

// instPartsFor returns the canonical-key arg names for canon, or nil.
func (cg *CodeGen) instPartsFor(canon CanonKey) []string {
	if r, ok := cg.types[canon]; ok {
		return r.InstParts
	}

	return nil
}

// displayFor returns the explicit Pretty display name registered for
// canon, or "".  Callers that want a sensible fallback should use
// typeNameFromCanon(...).Pretty instead.
func (cg *CodeGen) displayFor(canon CanonKey) string {
	if r, ok := cg.types[canon]; ok {
		return r.Pretty
	}

	return ""
}

// isEnumFor reports whether canon was registered as an enum type.
func (cg *CodeGen) isEnumFor(canon CanonKey) bool {
	if r, ok := cg.types[canon]; ok {
		return r.IsEnum
	}

	return false
}

// enumTypeFor returns the enum base type registered for canon, or nil
// when no record exists or the record is not an enum.  Equivalent to
// the old `cg.enumTypes[canon]` lookup.
func (cg *CodeGen) enumTypeFor(canon CanonKey) irtypes.Type {
	if r, ok := cg.types[canon]; ok && r.IsEnum {
		return r.LLVM
	}

	return nil
}

// aliasTypeFor returns the AST type-alias target for canon, or nil.
// Transient param substitutions (pushAlias / popAlias) keep the
// registry's Alias field in sync, so this matches the legacy
// `cg.typeAliases[canon]` lookup at every point in compilation.
func (cg *CodeGen) aliasTypeFor(canon CanonKey) ast.TypeExpr {
	if r, ok := cg.types[canon]; ok {
		return r.Alias
	}

	return nil
}

// DisplayIn returns the user-facing name of this type in the context of
// the named source file.  Prefers the per-file alias when one was
// recorded (so the user sees the form they wrote: bare `Unit` in files
// that use the builtin form, `sync::Unit` in files that import via sync,
// `Conn` in udp.tin, etc.) and falls back to the canonical Pretty.
func (r *TypeRecord) DisplayIn(fileID string) string {
	if r == nil {
		return ""
	}

	if alias, ok := r.Aliases[fileID]; ok && alias != "" {
		return alias
	}

	return r.Pretty
}

// keep ir import used (for future phases that store *ir.Func in the record)
var _ ir.Module

// TypeName identifies a Tin type across the three forms codegen tracks.
//
//	Pretty -- "errors::Err"        -- user-facing (diagnostics)
//	Canon  -- "errors__Err"        -- monomorph key, map key
//	LLVM   -- "errors__Err_iface"  -- the struct.Name() emitted in IR
//
// For non-trait types LLVM == Canon.  For traits LLVM appends "_iface".
// For unqualified primitives (`i64`, `string`, ...) all three are equal.
//
// rec is a back-pointer to the registry record for this type, set at
// construction by the typeNameFrom* helpers.  Callers that need to read
// multiple fields of the record (LLVM type, AST decl, monomorph shape,
// ...) can do so through tn.rec without re-hashing on cg.types -- the
// caller already paid that cost when the TypeName was first built.
// nil for primitives and other types that have no registered record.
type TypeName struct {
	Pretty string
	Canon  string
	LLVM   string
	rec    *TypeRecord
}

// Record returns the registry record this TypeName refers to, or nil
// when the type has no registered record (e.g. primitives, anonymous
// fat pointers).  Lets call sites holding a TypeName access every facet
// of the type without going through cg.types[tn.Canon] again.
func (tn TypeName) Record() *TypeRecord {
	return tn.rec
}

// IsTraitIface reports whether this name refers to a trait fat-pointer
// struct.  Equivalent to LLVM != Canon.
func (tn TypeName) IsTraitIface() bool {
	return tn.LLVM != tn.Canon
}

// String returns Pretty so a TypeName interpolates cleanly in user-visible
// strings.  Code that needs Canon or LLVM must reach in explicitly.
func (tn TypeName) String() string {
	return tn.Pretty
}

// typeNameFromLLVM derives all three forms from an LLVM type.  The hot path
// for inference: a generic call captures the LLVM type of an argument and
// needs to round-trip it through monomorphization.  Stripping `_iface`
// here is what would close the trait-leak ambiguity at the source if
// callers stopped passing bare strings.
func (cg *CodeGen) typeNameFromLLVM(t irtypes.Type) TypeName {
	if t == nil {
		return TypeName{Pretty: "void", Canon: "void", LLVM: "void"}
	}

	if st, ok := t.(*irtypes.StructType); ok && st.Name() != "" {
		llvm := st.Name()
		canon := llvm

		if isTraitFatPtrShape(t) && strings.HasSuffix(llvm, "_iface") {
			canon = strings.TrimSuffix(llvm, "_iface")
		}

		return TypeName{
			Pretty: cg.prettyFromCanon(canon),
			Canon:  canon,
			LLVM:   llvm,
			rec:    cg.types[CanonKey(canon)],
		}
	}
	// Anonymous / primitive: fall back to llvmTypeName, which already
	// strips _iface for trait pointers and renders primitives as their
	// Tin surface names ("i64", "string", ...).  Same string serves all
	// three forms.  No registry record for these.
	n := llvmTypeName(t)

	return TypeName{Pretty: n, Canon: n, LLVM: n}
}

// typeNameFromCanon constructs a TypeName from a canonical-form string.
// Use when the only handle to a type is a Canon string previously stashed
// in a cache key or substitution map.
func (cg *CodeGen) typeNameFromCanon(canon string) TypeName {
	canonBase := strings.TrimSuffix(canon, "_iface")

	llvm := canonBase
	if cg.isTraitCanon(canonBase) {
		llvm = canonBase + "_iface"
	}

	return TypeName{
		Pretty: cg.prettyFromCanon(canonBase),
		Canon:  canonBase,
		LLVM:   llvm,
		rec:    cg.types[CanonKey(canonBase)],
	}
}

// typeNameFromExpr maps an ast.TypeExpr to TypeName.  Canon comes from the
// existing typeExprCanonicalKey logic (resolving aliases, recursing into
// generics).  This is the bridge from source-level AST to the three-form
// representation.
func (cg *CodeGen) typeNameFromExpr(te ast.TypeExpr) TypeName {
	return cg.typeNameFromCanon(cg.typeExprCanonicalKey(te))
}

// prettyFromCanon returns the stored Pretty for a canonical name, or
// the canonical string itself when no record exists.  No reconstruction
// happens here -- every place that needs a Pretty value sets it at the
// matching write site (pkgStructKey, recordInstShape, recordTrait,
// recordData, ...).  See feedback_no_pretty_reconstruction.md.
func (cg *CodeGen) prettyFromCanon(canon string) string {
	canon = strings.TrimSuffix(canon, "_iface")

	if p := cg.prettyOf(CanonKey(canon)); p != "" {
		return p
	}

	return canon
}

// canonNameToTypeExpr reconstructs an ast.TypeExpr from a canonical name
// string when no structured `dataInstShape` is available.  Used by the
// constraint checker's structural-match path.  Delegates to
// parseTypeParamStr after first unmangling the canonical `__`-separator
// generic form to its bracketed shape (the previous flow was
// `parseTypeParamStr(prettyStructName(...))`).
//
// The `__`-split is lossy for nested generics; the caller (constraint
// checker) prefers the dataInstShape path when available, falling back
// here only when no structured shape was recorded.
func canonNameToTypeExpr(canon string) ast.TypeExpr {
	return parseTypeParamStr(canonToBracketed(canon))
}

// canonToBracketed converts a canonical-form generic name to its bracketed
// equivalent: `Box__i64` -> `Box[i64]`, `HashMap__string__i64` ->
// `HashMap[string, i64]`.  Plain names (no `__`) and shapes already in
// non-generic form (`*i64`, `[]i64`) pass through unchanged.
func canonToBracketed(canon string) string {
	if strings.HasPrefix(canon, "*") || strings.HasPrefix(canon, "[") {
		return canon
	}

	idx := strings.Index(canon, "__")
	if idx < 0 {
		return canon
	}

	base := canon[:idx]
	rest := canon[idx+2:]
	args := strings.ReplaceAll(rest, "__", ", ")

	return base + "[" + args + "]"
}

// isTraitCanon reports whether a canonical name refers to a registered
// trait.  Accepts both bare ("Err") and pkg-qualified ("errors__Err")
// shapes; the registry stores traits keyed by the bare name so the
// qualified form splits on its last `__`.
func (cg *CodeGen) isTraitCanon(canon string) bool {
	if cg.traitFor(CanonKey(canon)) != nil {
		return true
	}

	if idx := strings.LastIndex(canon, "__"); idx >= 0 {
		if cg.traitFor(CanonKey(canon[idx+2:])) != nil {
			return true
		}
	}

	return false
}

// fmtArgType renders an LLVM type using Tin surface names where possible so
// error messages read naturally.  Named struct types route through TypeName
// (recovers monomorphized generic shape and strips trait `_iface` suffixes);
// primitives, fat pointers, and aggregates handle themselves inline.  The
// single entry point used at every diagnostic site that needs to surface an
// LLVM type to the user.
func (cg *CodeGen) fmtArgType(t irtypes.Type) string {
	if t == nil {
		return "<nil>"
	}

	if isFatArrayPtr(t) {
		st := t.(*irtypes.StructType)
		elem := st.Fields[0].(*irtypes.PointerType).ElemType

		if it, ok := elem.(*irtypes.IntType); ok && it.BitSize == 8 {
			return "string"
		}

		return "[" + cg.fmtArgType(elem) + "]"
	}

	if isAtomType(t) {
		return "atom"
	}

	if it, ok := t.(*irtypes.IntType); ok {
		switch it.BitSize {
		case 1:
			return "bool"
		case 8, 16, 32, 64, 128:
			return fmt.Sprintf("i%d", it.BitSize)
		}
	}

	if ft, ok := t.(*irtypes.FloatType); ok {
		switch ft.Kind { //nolint:exhaustive // half/X86_FP80/PPC_FP128 are unused by tin
		case irtypes.FloatKindHalf:
			return "f16"
		case irtypes.FloatKindFloat:
			return "f32"
		case irtypes.FloatKindDouble:
			return "f64"
		case irtypes.FloatKindFP128:
			return "f128"
		}
	}

	if pt, ok := t.(*irtypes.PointerType); ok {
		if pt.ElemType == nil {
			return "*void"
		}

		return "*" + cg.fmtArgType(pt.ElemType)
	}

	if at, ok := t.(*irtypes.ArrayType); ok {
		return fmt.Sprintf("[%d x %s]", at.Len, cg.fmtArgType(at.ElemType))
	}

	if st, ok := t.(*irtypes.StructType); ok {
		if st.Name() != "" {
			return cg.typeNameFromLLVM(t).Pretty
		}
		// Anonymous tuple-like struct: render as (T1, T2, ...).
		parts := make([]string, len(st.Fields))
		for i, f := range st.Fields {
			parts[i] = cg.fmtArgType(f)
		}

		return "(" + strings.Join(parts, ", ") + ")"
	}

	if _, ok := t.(*irtypes.VoidType); ok {
		return "void"
	}

	return t.String()
}
