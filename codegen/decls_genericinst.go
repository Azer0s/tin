package codegen

import (
	"fmt"
	"strings"

	irtypes "github.com/llir/llvm/ir/types"

	"github.com/Azer0s/tin/ast"
)

func substituteTypeInTypeExpr(te ast.TypeExpr, subst map[string]ast.TypeExpr) ast.TypeExpr {
	if te == nil || len(subst) == 0 {
		return te
	}

	switch t := te.(type) {
	case *ast.WildcardType:
		// Anonymous `_` resolves through the conventional "_" key
		// monomorphizeDataDecl populates. Named wildcards `_: T` look up
		// by their introduced name. Unresolved wildcards pass through
		// unchanged so callers can decide whether to error.
		key := "_"
		if t.Name != "" {
			key = t.Name
		}

		if rep, ok := subst[key]; ok {
			return rep
		}
	case *ast.SimpleType:
		if rep, ok := subst[t.Name]; ok {
			return rep
		}
	case *ast.GenericType:
		changed := false

		newParams := make([]ast.TypeExpr, len(t.TypeParams))
		for i, p := range t.TypeParams {
			newP := substituteTypeInTypeExpr(p, subst)

			newParams[i] = newP
			if newP != p {
				changed = true
			}
		}

		if changed {
			return &ast.GenericType{Name: t.Name, TypeParams: newParams}
		}
	case *ast.PointerType:
		newElem := substituteTypeInTypeExpr(t.Elem, subst)
		if newElem != t.Elem {
			return &ast.PointerType{Elem: newElem}
		}
	case *ast.ArrayType:
		newElem := substituteTypeInTypeExpr(t.Elem, subst)
		if newElem != t.Elem {
			return &ast.ArrayType{Elem: newElem, Size: t.Size}
		}
	case *ast.FuncType:
		// Closure-shaped parameter / return types: walk into the param
		// and return slots so a method's `fn(t) u` param picks up the
		// outer ADT substitution and (when present) downstream method-
		// level type bindings.  Without this, a method signature like
		// `fn map[u](this Result[t,e], f fn(t) u) Result[u, e]` keeps
		// `t` symbolic inside the closure type after Result[i64, _]
		// monomorphization, and codegen sees a return type of `i64`
		// where `Option[i64]` was expected.
		newParams := make([]ast.TypeExpr, len(t.Params))

		changed := false

		for i, p := range t.Params {
			newP := substituteTypeInTypeExpr(p, subst)

			newParams[i] = newP
			if newP != p {
				changed = true
			}
		}

		newRet := substituteTypeInTypeExpr(t.RetType, subst)
		if newRet != t.RetType {
			changed = true
		}

		if changed {
			return &ast.FuncType{Params: newParams, RetType: newRet, IsVarArgs: t.IsVarArgs}
		}
	}

	return te
}

// substituteTraitQualifier walks a parsed trait-qualifier string,
// applies the type substitution map, and re-emits the canonical form.
// Returns the original string when the qualifier doesn't contain
// substitutable type parameters (no `[`).
func substituteTraitQualifier(qual string, subst map[string]ast.TypeExpr) string {
	if qual == "" || !strings.Contains(qual, "[") || len(subst) == 0 {
		return qual
	}
	// Parse the qualifier as a type expression so we can walk it
	// recursively and substitute. The qualifier syntax is the same as a
	// trait bound, so the same parser primitives apply.
	te, err := parseTypeExprFromString(qual)
	if err != nil {
		return qual
	}

	te = substituteTypeInTypeExpr(te, subst)

	return typeExprToTraitQualifier(te)
}

// parseTypeExprFromString parses a trait-qualifier-like string into a
// TypeExpr by hand. The qualifier grammar is a subset of the type-expr
// grammar (no pointers, arrays, or function types) so a small recursive
// descent suffices and we don't drag the full parser in.
func parseTypeExprFromString(s string) (ast.TypeExpr, error) {
	p := &qualParser{src: s}

	te, err := p.parseExpr()
	if err != nil {
		return nil, err
	}

	if p.pos < len(s) {
		return nil, fmt.Errorf("unexpected trailing %q", s[p.pos:])
	}

	return te, nil
}

type qualParser struct {
	src string
	pos int
}

func (p *qualParser) skipSpaces() {
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
		p.pos++
	}
}

func (p *qualParser) parseExpr() (ast.TypeExpr, error) {
	p.skipSpaces()

	// Identifier (with optional `::` segments).
	start := p.pos

	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == ':' && p.pos+1 < len(p.src) && p.src[p.pos+1] == ':' {
			p.pos += 2

			continue
		}

		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			p.pos++

			continue
		}

		break
	}

	if start == p.pos {
		return nil, fmt.Errorf("expected identifier at %d", p.pos)
	}

	name := p.src[start:p.pos]

	// Special-case "_" - wildcard.
	if name == "_" {
		return &ast.WildcardType{}, nil
	}

	p.skipSpaces()

	// Optional `[T, ...]`.
	if p.pos < len(p.src) && p.src[p.pos] == '[' {
		p.pos++ // consume `[`

		var args []ast.TypeExpr

		for {
			p.skipSpaces()

			if p.pos < len(p.src) && p.src[p.pos] == ']' {
				p.pos++

				break
			}

			arg, err := p.parseExpr()
			if err != nil {
				return nil, err
			}

			args = append(args, arg)

			p.skipSpaces()

			if p.pos < len(p.src) && p.src[p.pos] == ',' {
				p.pos++

				continue
			}

			if p.pos < len(p.src) && p.src[p.pos] == ']' {
				p.pos++

				break
			}

			return nil, fmt.Errorf("expected , or ] at %d", p.pos)
		}

		return &ast.GenericType{Name: name, TypeParams: args}, nil
	}

	return &ast.SimpleType{Name: name}, nil
}

// typeExprToTraitQualifier renders a TypeExpr in the canonical
// trait-qualifier form (matching the parser's accepted shape). Inverse
// of parseTypeExprFromString for the subset that appears in qualifiers.
func typeExprToTraitQualifier(te ast.TypeExpr) string {
	switch t := te.(type) {
	case *ast.SimpleType:
		return t.Name
	case *ast.GenericType:
		out := t.Name + "["

		for i, tp := range t.TypeParams {
			if i > 0 {
				out += ", "
			}

			out += typeExprToTraitQualifier(tp)
		}

		out += "]"

		return out
	case *ast.WildcardType:
		if t.Name != "" {
			return "_: " + t.Name
		}

		return "_"
	case *ast.ArrayType:
		if t.Size < 0 {
			return "[" + typeExprToTraitQualifier(t.Elem) + "]"
		}

		return fmt.Sprintf("[%s; %d]", typeExprToTraitQualifier(t.Elem), t.Size)
	case *ast.PointerType:
		if t.IsConst {
			return "const *" + typeExprToTraitQualifier(t.Elem)
		}

		return "*" + typeExprToTraitQualifier(t.Elem)
	}

	return ""
}

// substituteMethod returns a copy of m with type params substituted and
// the self-parameter type renamed from genericName to concreteName.
func substituteMethod(m *ast.FuncDecl, genericName, concreteName string, subst map[string]ast.TypeExpr) *ast.FuncDecl {
	newParams := make([]ast.Param, len(m.Params))
	for i, p := range m.Params {
		newType := substituteTypeInTypeExpr(p.Type, subst)
		// rename the self parameter from the generic struct name to concrete.
		// Handle both value receiver (SimpleType) and pointer receiver (*SimpleType).
		if st, ok := newType.(*ast.SimpleType); ok && st.Name == genericName {
			newType = &ast.SimpleType{Name: concreteName}
		} else if pt, ok := newType.(*ast.PointerType); ok {
			if st, ok2 := pt.Elem.(*ast.SimpleType); ok2 && st.Name == genericName {
				newType = &ast.PointerType{Elem: &ast.SimpleType{Name: concreteName}}
			} else if gt, ok2 := pt.Elem.(*ast.GenericType); ok2 && gt.Name == genericName {
				newType = &ast.PointerType{Elem: &ast.SimpleType{Name: concreteName}}
			}
		}

		newParams[i] = ast.Param{Name: p.Name, Type: newType, IsConst: p.IsConst, IsVarArgs: p.IsVarArgs}
	}

	newRet := substituteTypeInTypeExpr(m.RetType, subst)
	newBody := substituteStructNameInBody(m.Body, genericName, concreteName)

	// Substitute the trait-qualifier string so methodScopeName produces
	// the same key the impl-bound side computes. Without this, a
	// monomorphized method on `tryable[t, Result[_, e]]` stays as the
	// unsubstituted "tryable[t, Result[_, e]]" qualifier even when
	// genTraitVtables looks for the substituted "tryable[T_concrete,
	// Result[T_concrete, E_concrete]]" key - and the trait-vtable
	// emission rejects the impl as missing.
	newQualifier := substituteTraitQualifier(m.TraitQualifier, subst)

	out := &ast.FuncDecl{
		Name:           m.Name,
		TraitQualifier: newQualifier,
		TypeParams:     m.TypeParams,
		Constraints:    m.Constraints,
		Params:         newParams,
		RetType:        newRet,
		Body:           newBody,
		Tags:           m.Tags,
		IsStatic:       m.IsStatic,
		IsExtern:       m.IsExtern,
		IsVirtual:      m.IsVirtual,
		// Preserve the wildcard-return marker through monomorphization
		// so call-site generics can opt in based on the original
		// declaration's intent.
		RetTypeHasWildcard: m.RetTypeHasWildcard || typeExprContainsWildcard(m.RetType),
	}
	out.SetPos(m.Pos())

	return out
}

// substituteStructNameInBody walks the AST body and replaces any StructLit
// with TypeName == genericName to use concreteName instead.
func substituteStructNameInBody(node ast.Node, genericName, concreteName string) ast.Node {
	if node == nil {
		return nil
	}

	switch n := node.(type) {
	case *ast.StructLit:
		newFields := make([]ast.StructLitField, len(n.Fields))
		for i, f := range n.Fields {
			newFields[i] = ast.StructLitField{Name: f.Name, Value: substituteStructNameInBody(f.Value, genericName, concreteName)}
		}

		typeName := n.TypeName
		// Only rename bare (no TypeArgs) struct literals.  If TypeArgs are present,
		// genStructLit will resolve the concrete name at codegen time via typeAliases
		// (set by monomorphizeFunc).  Pre-renaming here AND dropping the TypeArgs
		// causes genStructLit to use the wrong concrete struct (e.g. box__i64 instead
		// of box__string when r=string), producing a type-mismatch panic.
		if typeName == genericName && len(n.TypeArgs) == 0 {
			typeName = concreteName
		}

		return &ast.StructLit{TypeName: typeName, TypeArgs: n.TypeArgs, Fields: newFields, Positional: n.Positional}
	case *ast.Block:
		newStmts := make([]ast.Node, len(n.Stmts))
		for i, s := range n.Stmts {
			newStmts[i] = substituteStructNameInBody(s, genericName, concreteName)
		}

		return &ast.Block{Stmts: newStmts}
	case *ast.ReturnStmt:
		return &ast.ReturnStmt{Value: substituteStructNameInBody(n.Value, genericName, concreteName)}
	case *ast.VarDecl:
		return &ast.VarDecl{Name: n.Name, Type: n.Type, Value: substituteStructNameInBody(n.Value, genericName, concreteName), IsConst: n.IsConst}
	case *ast.IfStmt:
		newIf := *n

		newIf.Cond = substituteStructNameInBody(n.Cond, genericName, concreteName)
		if n.Then != nil {
			if b, ok := substituteStructNameInBody(n.Then, genericName, concreteName).(*ast.Block); ok {
				newIf.Then = b
			}
		}

		if n.Else != nil {
			if b, ok := substituteStructNameInBody(n.Else, genericName, concreteName).(*ast.Block); ok {
				newIf.Else = b
			}
		}

		return &newIf
	case *ast.CallExpr:
		newArgs := make([]ast.Node, len(n.Args))
		for i, a := range n.Args {
			newArgs[i] = substituteStructNameInBody(a, genericName, concreteName)
		}

		return &ast.CallExpr{Func: substituteStructNameInBody(n.Func, genericName, concreteName), Args: newArgs}
	case *ast.BinExpr:
		return &ast.BinExpr{Left: substituteStructNameInBody(n.Left, genericName, concreteName), Op: n.Op, Right: substituteStructNameInBody(n.Right, genericName, concreteName)}
	case *ast.FieldAccess:
		return &ast.FieldAccess{Expr: substituteStructNameInBody(n.Expr, genericName, concreteName), Field: n.Field, IsPtr: n.IsPtr}
	}

	return node
}

// genTypeDecl handles "type X = SomeType [override = ...]" declarations.
// For simple aliases (type char = u8) the alias was already recorded in
// preregister; this function handles the struct-monomorphization case
// (type point = tuple[f32]) which requires actual LLVM type generation.
// expandGenericAlias handles a TypeDecl whose RHS names a generic type
// alias rather than a concrete struct template. It substitutes the outer
// synthetic decl's type arguments into the alias's RHS and then calls
// genTypeDecl on the expanded decl. Enforces the alias's where-bounds at
// expansion time with concrete types so the error message names the
// alias's constraint, not the underlying struct's.
func (cg *CodeGen) expandGenericAlias(synth *ast.TypeDecl, aliasTmpl *ast.TypeDecl, aliasInstance *ast.GenericType) error {
	subst := make(map[string]ast.TypeExpr, len(aliasTmpl.TypeParams))
	for i, paramName := range aliasTmpl.TypeParams {
		if i < len(aliasInstance.TypeParams) {
			subst[paramName] = aliasInstance.TypeParams[i]
		}
	}
	// Enforce the alias's own bounds before expanding.
	for _, c := range aliasTmpl.Constraints {
		argTE, ok := subst[c.TypeParam]
		if !ok {
			continue
		}

		concreteName := typeExprToString(argTE)
		if ok, witness := cg.typeBoundSatisfied(concreteName, c.Bound); !ok {
			return fmt.Errorf("%d:%d: type %s[%s]: type %q does not satisfy constraint \"where %s is %s\" (failing sub-check: \"%s\")",
				c.Pos.Line, c.Pos.Col, aliasTmpl.Name, concreteName, concreteName,
				c.TypeParam, typeBoundString(c.Bound), typeBoundString(witness))
		}
	}
	// Expand the alias RHS with the concrete substitution. The RHS is
	// likely a GenericType (`Pair[string, T]`) whose T we substitute.
	expandedRHS := substituteTypeInTypeExpr(aliasTmpl.Type, subst)

	expandedDecl := &ast.TypeDecl{
		Name: synth.Name,
		Type: expandedRHS,
	}

	return cg.genTypeDecl(expandedDecl)
}

func (cg *CodeGen) genTypeDecl(n *ast.TypeDecl) error {
	// Tagged union alias: "type u = i8 | string"
	if ut, ok := n.Type.(*ast.UnionTypeExpr); ok {
		return cg.genTaggedUnionTypeDecl(n.Name, ut)
	}
	// Register generic type aliases (those with their own TypeParams) so
	// `StrPair[i32]{...}` can resolve by substituting the alias's params
	// into its RHS and re-monomorphizing the underlying struct.
	// Checking for `len(n.TypeParams) > 0 && alias is generic or compound`
	// covers the StrPair/Pair case without interfering with concrete
	// aliases like `type BoxI32 = Box[i32]` which have no TypeParams.
	if len(n.TypeParams) > 0 {
		cg.genericTypeAliases[n.Name] = n
	}

	gt, ok := n.Type.(*ast.GenericType)
	if !ok {
		// Simple alias - already registered in preregister. Nothing to do.
		return nil
	}

	arity := len(gt.TypeParams)

	var tmpl *ast.StructDecl

	isTmpl := false

	qualGtName := cg.typeExprCanonicalKey(&ast.SimpleType{Name: gt.Name})
	if arityMap, ok := cg.genericStructsByArity[qualGtName]; ok {
		tmpl, isTmpl = arityMap[arity]
	}
	// If the referenced name is ITSELF a generic type alias, expand it
	// recursively: substitute the outer alias's type params into the
	// inner alias's RHS and rerun genTypeDecl on the expanded decl. This
	// lets a chain like `type Wrapper[T] = StrPair[T]` work.
	if !isTmpl {
		if aliasTmpl, isAlias := cg.genericTypeAliases[gt.Name]; isAlias && len(gt.TypeParams) == len(aliasTmpl.TypeParams) {
			return cg.expandGenericAlias(n, aliasTmpl, gt)
		}
	}

	if !isTmpl {
		// GenericType refers to something other than a generic struct
		// (e.g. a generic trait instantiation used as a type alias).
		cg.recordAliasType(CanonKey(n.Name), n.Type)

		return nil
	}

	// Generic alias with its own type params (e.g.
	// `type StrPair[T] = Pair[string, T]`): defer instantiation. The
	// alias template is already in cg.genericTypeAliases (registered
	// above); each USAGE like `StrPair[i32]{...}` resolves through
	// expandGenericAlias, which substitutes the concrete args into the
	// body and re-runs genTypeDecl. Eagerly substituting here would
	// build a concrete struct whose field types still reference the
	// alias's unresolved type params (T), which the strict bare-type
	// resolver flags as "unknown type".
	if len(n.TypeParams) > 0 {
		// Check whether ANY of the GenericType's TypeParams is a
		// template var of THIS alias (i.e. the alias is "open" - its
		// body still mentions its own params). If so, leave it as a
		// template; usage triggers instantiation. Closed forms
		// (`type GI[unused] = G[i64]`) still fall through to the
		// monomorphization path below.
		open := false

		for _, ta := range gt.TypeParams {
			if st, ok := ta.(*ast.SimpleType); ok {
				for _, p := range n.TypeParams {
					if p == st.Name {
						open = true

						break
					}
				}
			}

			if open {
				break
			}
		}

		if open {
			return nil
		}
	}

	// Concrete generic alias (no type params on the alias itself, like
	// `type GI = G[i64]`): instead of building a duplicate struct named
	// GI, monomorphize G[i64] under its canonical name (G__i64) and
	// register GI as an alias for that canonical name. Without this,
	// `let outer = G[GI]{v: inner}` would resolve GI as a fresh struct
	// distinct from G__i64 and the store would type-mismatch on inner.
	//
	// Skip when the alias declaration carries method overrides -- those
	// overrides need to live on a distinct struct (the alias name), so
	// keep the existing monomorphize-as-separate-struct path for that
	// case. See examples/type_alias.tin "override show method".
	if len(n.TypeParams) == 0 && len(n.Overrides) == 0 {
		canonicalName := cg.typeExprCanonicalKey(gt)
		if canonicalName != n.Name {
			if cg.structTypeFor(CanonKey(canonicalName)) == nil {
				if err := cg.genTypeDecl(&ast.TypeDecl{
					Name: canonicalName,
					Type: gt,
				}); err != nil {
					return err
				}
			}

			cg.recordAliasType(CanonKey(n.Name), &ast.SimpleType{Name: canonicalName})
			cg.recordAlias(CanonKey(canonicalName), n.Name)

			return nil
		}
	}

	// Build type-parameter substitution: tmpl.TypeParams[i] -> gt.TypeParams[i]
	subst := make(map[string]ast.TypeExpr)

	for i, paramName := range tmpl.TypeParams {
		if i < len(gt.TypeParams) {
			subst[paramName] = gt.TypeParams[i]
		}
	}

	// Validate generic type constraints (e.g. "where t is addable").
	// Build a string-keyed map from the substitution for constraint checking.
	typeSubst := make(map[string]string, len(subst))
	for param, te := range subst {
		typeSubst[param] = typeExprToString(te)
	}
	// Struct-template's own constraints (e.g. the template says T must be
	// ord). These apply to any instantiation regardless of the type alias.
	for _, c := range tmpl.Constraints {
		concreteName, ok := typeSubst[c.TypeParam]
		if !ok {
			continue
		}

		if ok, witness := cg.typeBoundSatisfied(concreteName, c.Bound); !ok {
			return fmt.Errorf("%d:%d: struct %s[%s]: type %q does not satisfy constraint \"where %s is %s\" (failing sub-check: \"%s\")",
				c.Pos.Line, c.Pos.Col, tmpl.Name, concreteName, concreteName,
				c.TypeParam, typeBoundString(c.Bound), typeBoundString(witness))
		}
	}
	// Type-alias's own constraints. These only make sense on a concrete
	// instantiation (e.g. StrPair[i32]), not on the template declaration
	// where every alias type parameter is still symbolic. Detect that by
	// checking whether any of the template's type-parameter names appears
	// in the RHS's type arguments; if so, skip the check and let the
	// instantiation path re-check with concrete substitutes.
	if len(n.Constraints) > 0 && !typeArgsContainAnyOf(gt.TypeParams, n.TypeParams) {
		aliasSubst := make(map[string]string, len(n.TypeParams))

		for i, paramName := range n.TypeParams {
			if i < len(gt.TypeParams) {
				aliasSubst[paramName] = typeExprToString(gt.TypeParams[i])
			}
		}

		for _, c := range n.Constraints {
			concreteName, ok := aliasSubst[c.TypeParam]
			if !ok {
				continue
			}

			if ok, witness := cg.typeBoundSatisfied(concreteName, c.Bound); !ok {
				return fmt.Errorf("%d:%d: type %s[%s]: type %q does not satisfy constraint \"where %s is %s\" (failing sub-check: \"%s\")",
					c.Pos.Line, c.Pos.Col, n.Name, concreteName, concreteName,
					c.TypeParam, typeBoundString(c.Bound), typeBoundString(witness))
			}
		}
	}

	// Build the concrete struct by substituting type params in every field and trait.
	// Implements must be substituted so that e.g. Future[t](Awaitable[t]) ->
	// Future__i64(Awaitable[i64]) uses the correct concrete trait instance key.
	var concreteImpls []ast.TypeExpr
	for _, impl := range tmpl.Implements {
		concreteImpls = append(concreteImpls, substituteTypeInTypeExpr(impl, subst))
	}

	concrete := &ast.StructDecl{
		Name:       n.Name,
		Implements: concreteImpls,
		Tags:       tmpl.Tags,
		ScopedTags: tmpl.ScopedTags,
	}
	for _, f := range tmpl.Fields {
		concrete.Fields = append(concrete.Fields, ast.StructField{
			Pos:       f.Pos,
			Name:      f.Name,
			Type:      substituteTypeInTypeExpr(f.Type, subst),
			Tags:      f.Tags,
			IsForward: f.IsForward,
			IsWeak:    f.IsWeak,
			IsOwn:     f.IsOwn,
			IsConst:   f.IsConst,
			IsVar:     f.IsVar,
		})
	}

	// Build method set: start with template methods (substituted), then
	// apply overrides from the TypeDecl.
	overrideSet := make(map[string]*ast.FuncDecl)
	for _, ov := range n.Overrides {
		overrideSet[ov.Name] = ov
	}

	for _, m := range tmpl.Methods {
		if ov, ok := overrideSet[m.Name]; ok {
			concrete.Methods = append(concrete.Methods, ov)

			delete(overrideSet, m.Name)

			continue
		}

		// Method-level where guards: a method on a generic struct whose
		// `where t is X` clause does NOT hold for the concrete
		// instantiation is dead-stripped from this concrete struct.
		// Calling the method on the wrong instantiation then produces
		// the dead-strip diagnostic at the call site (see emitDeadStripError)
		// listing every failing constraint -- one entry per stripped
		// overload, since the same method name can have multiple
		// where-guarded variants.
		if witness := cg.methodConstraintWitness(m, typeSubst); witness != "" {
			if cg.deadStrippedMethods[n.Name] == nil {
				cg.deadStrippedMethods[n.Name] = make(map[string][]string)
			}

			cg.deadStrippedMethods[n.Name][m.Name] = append(cg.deadStrippedMethods[n.Name][m.Name], witness)

			continue
		}

		concrete.Methods = append(concrete.Methods, substituteMethod(m, tmpl.Name, n.Name, subst))
	}
	// Any overrides that don't shadow a template method are appended.
	for _, ov := range n.Overrides {
		if _, already := overrideSet[ov.Name]; !already {
			continue // already applied above
		}

		concrete.Methods = append(concrete.Methods, ov)
	}

	// Where-guard ambiguity check: if two methods share the same name and
	// signature and BOTH satisfied their where-guards for this concrete
	// instantiation, the call site can't pick between them -- without
	// this check the user gets a misleading "no matching overload" error
	// at the call site instead.
	if amb := findAmbiguousMethods(concrete.Methods); amb != nil {
		return cg.ambiguousMethodError(n.Name, amb)
	}

	// Propagate the template's scoped tags onto the fresh concrete's
	// members. Must happen before the pre-registration loops below that
	// inspect m.Tags (for #async, overloads, predeclare).
	if err := cg.propagateStructScopedTags(concrete); err != nil {
		return err
	}

	// Register the concrete struct type (opaque first, just like preregister).
	// n.Name is already the full concrete name (e.g. "Future__sync__Unit"), so
	// no package prefix should be applied.
	if cg.structTypeFor(CanonKey(n.Name)) == nil {
		st := irtypes.NewStruct()
		st.SetName(n.Name)
		cg.recordLLVM(CanonKey(n.Name), st)
		cg.mod.TypeDefs = append(cg.mod.TypeDefs, st)
	}

	// Register type-param substitutions as aliases so that expressions inside
	// method bodies (e.g. `let out T`, `sizeof(T)`) resolve to the concrete type.
	type prevEntry struct {
		val    ast.TypeExpr
		hadVal bool
	}

	prevAliases := make(map[string]prevEntry, len(subst))

	for param, typeExpr := range subst {
		prev, had := cg.pushAlias(param, typeExpr)
		prevAliases[param] = prevEntry{val: prev, hadVal: had}
	}

	// Compile struct methods at module scope so they are visible everywhere,
	// not just inside the function that triggered the on-demand monomorphization.
	// Clear currentPkg so that the concrete struct (whose name already embeds
	// the canonical form) does not get an additional package prefix applied.
	prevScope := cg.curScope
	if cg.moduleScope != nil && cg.curScope != cg.moduleScope {
		cg.curScope = cg.moduleScope
	}

	prevPkg := cg.currentPkg
	cg.currentPkg = ""

	// Detect overloaded method names and predeclare all concrete methods exactly
	// once. This mirrors pass 1.8/1.9 for non-generic structs and is required so
	// that overload entries in cg.overloads are registered before genStructDecl
	// compiles method bodies and before call sites resolve variants.
	// The guard prevents double-registration when genTypeDecl is called more than
	// once for the same concrete type name.
	if !cg.genericMethodsSetUp[n.Name] {
		cg.genericMethodsSetUp[n.Name] = true

		methodCounts := make(map[string]int)

		for _, m := range concrete.Methods {
			if len(m.TypeParams) > 0 || m.IsExtern != "" {
				continue
			}

			key := methodScopeName(n.Name, m)
			methodCounts[key]++
		}

		for key, count := range methodCounts {
			if count > 1 {
				cg.overloadedNames[key] = true
			}
		}

		for _, m := range concrete.Methods {
			if len(m.TypeParams) > 0 {
				continue
			}

			if preErr := cg.predeclareMethod(n.Name, m); preErr != nil {
				cg.currentPkg = prevPkg
				cg.curScope = prevScope

				return preErr
			}

			if !isAsyncTag(m.Tags) || m.IsExtern != "" {
				continue
			}

			scopeKey := methodScopeName(n.Name, m)
			cg.coroCallable[scopeKey] = true

			cg.funcDecls[scopeKey] = m

			if preErr := cg.predeclareCoroVariant(m, scopeKey, false); preErr != nil {
				cg.currentPkg = prevPkg
				cg.curScope = prevScope

				return preErr
			}
		}
	}

	// Monomorphization re-walks the template's field declarations,
	// which reference user-supplied type arguments by their bare names
	// (e.g. `Result[string, string]` substituted into Tuple's `b` slot).
	// Those args were already validated for visibility at the original
	// call site (the user wrote them in scope); the bare-type check in
	// tinTypeToLLVM would otherwise mis-fire here because monomorphization
	// runs against moduleScope rather than the caller's scope.  Suppress
	// it for the duration of the field walk -- the only types resolved
	// here are compiler-substituted from the user's already-accepted
	// type arguments.  Mirrors the same suppression in monomorphizeDataDecl.
	prevSuppress := cg.suppressBareTypeCheck
	cg.suppressBareTypeCheck = true
	err := cg.genStructDecl(concrete)
	cg.suppressBareTypeCheck = prevSuppress
	cg.currentPkg = prevPkg
	cg.curScope = prevScope
	// Record the original TypeExpr arg list so wildcard-guard
	// matching (`where t is Box[Pair[_, _]]`) can compare against
	// the structured form instead of the lossy `__`-mangled name.
	if err == nil {
		cg.recordInstShape(CanonKey(n.Name), tmpl.Name, gt.TypeParams)
	}

	// genStructDecl tagged structDeclFiles[concrete.Name] with cg.filename
	// -- but cg.filename here is whoever instantiated the generic, NOT the
	// template's source. Override so per-line `//!-Wno-` directives on
	// the template (e.g. Channel's _ptr field) silence the diagnostic
	// for every monomorphization.
	//
	// The template was originally registered under its OWN package's key
	// (e.g. "sync__Channel"), but currentPkg has been restored to the
	// instantiating context. Look up the template by both the
	// current-pkg-prefixed key and the bare name, then sweep for any
	// "<pkg>__<tmplName>" entry as a final fallback so cross-package
	// generic instantiations also pick up the template's source path.
	if tmplFile := cg.lookupTemplateFile(tmpl.Name); tmplFile != "" {
		cg.structDeclFiles[concrete.Name] = tmplFile
	}

	// Restore previous type aliases (removes the T->string etc. temporaries).
	for param := range subst {
		entry := prevAliases[param]
		cg.popAlias(param, entry.val, entry.hadVal)
	}

	return err
}

// buildTraitFatPtrType computes (and caches) the LLVM fat-pointer type for a
// trait: { i8*, vtable_struct* }.  The vtable struct has one fn-ptr slot per
// trait method, each with signature (i8* self, ...) -> ret.
// traitImplKey returns a unique string key for a trait impl TypeExpr.
// Package qualifiers are converted from "::" to "__" so that
// "sync::Awaitable" and "Awaitable" (if it is the same trait) keep distinct
// keys per package when needed, while still being safely usable as identifiers.
// For "named" -> "named"; for "iter[i64]" -> "iter__i64".
