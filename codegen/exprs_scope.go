package codegen

import (
	"strconv"
	"strings"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

func (cg *CodeGen) genScopeAccess(block *ir.Block, e *ast.ScopeAccess) (value.Value, error) {
	// e.g. weather.sunny -> look up "weather.sunny" in enum registry.
	if len(e.Path) == 2 {
		key := e.Path[0] + "." + e.Path[1]
		if val, ok := cg.enumValues[key]; ok {
			baseType := cg.enumTypeFor(CanonKey(e.Path[0]))
			if it, ok2 := baseType.(*irtypes.IntType); ok2 {
				return constant.NewInt(it, val), nil
			}

			return constant.NewInt(irtypes.I32, val), nil
		}
	}

	// Nullary ADT variant: `Option[i32]::None`, `Tree[i64]::Leaf`.
	if v, handled, err := cg.genDataScopeCtorCall(block, e, nil); handled {
		return v, err
	}
	// Try identifier lookup.
	joined := strings.Join(e.Path, ".")

	entry, ok := cg.curScope.lookup(joined)
	if ok {
		if entry.isAlloc {
			ptrType := entry.val.Type().(*irtypes.PointerType)

			return block.NewLoad(ptrType.ElemType, entry.val), nil
		}

		return entry.val, nil
	}
	// For 3+ segment paths like std::math::floor, try dropping the first segment:
	// "math.floor" after failing "std.math.floor".
	if len(e.Path) >= 3 {
		tail := strings.Join(e.Path[1:], ".")

		entry, ok = cg.curScope.lookup(tail)
		if ok {
			if entry.isAlloc {
				ptrType := entry.val.Type().(*irtypes.PointerType)

				return block.NewLoad(ptrType.ElemType, entry.val), nil
			}

			return entry.val, nil
		}
	}
	// Try last element.
	last := e.Path[len(e.Path)-1]

	entry, ok = cg.curScope.lookup(last)
	if ok {
		if entry.isAlloc {
			ptrType := entry.val.Type().(*irtypes.PointerType)

			return block.NewLoad(ptrType.ElemType, entry.val), nil
		}

		return entry.val, nil
	}
	// Try struct static method: TypeName::method or TypeName[T]::method
	// Also handles package-qualified names:
	//   pkg::TypeName[T,U]::method  (2-element path, type is path[0])
	//   pkg::TypeName::method       (3-element path, pkg=path[0], type=path[1])
	// Scope key is "TypeName_method" (set when struct is compiled with static methods).
	if len(e.Path) >= 2 {
		baseName := e.Path[0]

		// 3-element path: pkg::StructName::method - struct is path[1], not path[0].
		if len(e.Path) == 3 {
			baseName = e.Path[1]
		}

		typeParamStr := ""
		if i := strings.Index(baseName, "["); i >= 0 {
			typeParamStr = strings.TrimSuffix(baseName[i+1:], "]")
			baseName = baseName[:i]
		}

		// Strip package qualifier (e.g. "collections::HashMap" -> "HashMap") for scope lookup.
		bareBaseName := baseName
		if idx2 := strings.LastIndex(bareBaseName, "::"); idx2 >= 0 {
			bareBaseName = bareBaseName[idx2+2:]
		}

		staticKey := bareBaseName + "_" + last

		entry, ok = cg.curScope.lookup(staticKey)
		if ok {
			if entry.isAlloc {
				ptrType := entry.val.Type().(*irtypes.PointerType)

				return block.NewLoad(ptrType.ElemType, entry.val), nil
			}

			return entry.val, nil
		}
		// On-demand monomorphization: if bareBaseName is a generic struct template
		// and we have concrete type params, monomorphize now and retry.
		// typeParamStr may be comma-separated for multi-param generics (e.g.
		// "string,string"); each piece can itself be a nested generic
		// (`*rc::Cell[i64]`), a type alias, or a qualified name. Parse each
		// to a TypeExpr first so the canonical-key step handles all shapes
		// (alias chains, pointers, packages) uniformly.
		if typeParamStr != "" {
			if _, isGeneric := cg.genericStructsByArity[bareBaseName]; isGeneric {
				rawParts := splitTopLevelTypeArgs(typeParamStr)
				resolvedParts := make([]string, len(rawParts))
				resolvedTEs := make([]ast.TypeExpr, len(rawParts))

				for i, te := range rawParts {
					resolvedParts[i] = cg.typeExprCanonicalKey(te)
					resolvedTEs[i] = te
				}

				concreteName := bareBaseName + "__" + strings.Join(resolvedParts, "__")
				if cg.structTypeFor(CanonKey(concreteName)) == nil {
					synthDecl := &ast.TypeDecl{
						Name: concreteName,
						Type: &ast.GenericType{Name: bareBaseName, TypeParams: resolvedTEs},
					}

					if mErr := cg.genTypeDecl(synthDecl); mErr != nil {
						return nil, cg.nodeErr(e, "instantiating %s: %v", cg.diagStructName(concreteName), mErr)
					}
				}

				concreteStaticKey := concreteName + "_" + last

				entry, ok = cg.curScope.lookup(concreteStaticKey)
				if ok {
					if entry.isAlloc {
						ptrType := entry.val.Type().(*irtypes.PointerType)

						return block.NewLoad(ptrType.ElemType, entry.val), nil
					}

					return entry.val, nil
				}
			}
		}
	}

	return nil, cg.nodeErr(e, "undefined: %s", strings.Join(e.Path, "::"))
}

// exprToTypeParamKey converts a parsed expression node into a canonical type-key
// string (same format as typeExprCanonicalKey). Used when generic type params are
// parsed as expressions (e.g. Channel[*T] -> IndexExpr with UnaryExpr{"*","T"}).
// Returns "" when the expression cannot be interpreted as a type.
func (cg *CodeGen) exprToTypeParamKey(node ast.Node) string {
	switch n := node.(type) {
	case *ast.Identifier:
		return n.Name
	case *ast.TypeRefNode:
		// Parser-emitted carrier for type-arg-position FuncType (and
		// other shapes parseExpr can't yield natively).  Delegate to the
		// canonical-key encoder so the result round-trips via
		// parseTypeParamStr.
		return cg.typeExprCanonicalKey(n.Type)
	case *ast.UnaryExpr:
		if n.Op == "*" {
			inner := cg.exprToTypeParamKey(n.Expr)
			if inner != "" {
				return "*" + inner
			}
		}
	case *ast.DerefExpr:
		// In expression context, *T is parsed as DerefExpr{Expr: T}.
		// When T is used as a type parameter (e.g. Channel[*counter_t]),
		// convert back to the canonical pointer type key "*counter_t".
		inner := cg.exprToTypeParamKey(n.Expr)
		if inner != "" {
			return "*" + inner
		}
	case *ast.ArrayLit:
		// []T represented as an empty array literal of one element - best-effort.
	case *ast.ScopeAccess:
		// Strip the package qualifier so the key matches the canonical
		// monomorphized struct name (e.g. `rc::Cell` -> `Cell`, since
		// genStructDecl registers concrete instances under `Cell__T`).
		// Keeping the prefix would produce `rc::Cell__T` which never
		// matches an existing struct.
		name := strings.Join(n.Path, "::")
		if idx := strings.LastIndex(name, "::"); idx >= 0 {
			name = name[idx+2:]
		}

		return name
	case *ast.IndexExpr:
		// Nested generic type arg, e.g. `G[G[i64]].make(...)`: the inner
		// `G[i64]` parses as IndexExpr. Recurse to produce the canonical
		// `G__i64` key so the outer instantiation finds the right struct.
		baseKey := cg.exprToTypeParamKey(n.Expr)
		if baseKey == "" {
			return ""
		}
		// Comma-encoded multi-arg case from the parser (e.g. `K,V`).
		if argID, ok := n.Index.(*ast.Identifier); ok && strings.Contains(argID.Name, ",") {
			parts := []string{baseKey}
			for _, raw := range strings.Split(argID.Name, ",") {
				parts = append(parts, strings.TrimSpace(raw))
			}

			return strings.Join(parts, "__")
		}

		argKey := cg.exprToTypeParamKey(n.Index)
		if argKey == "" {
			return ""
		}

		return baseKey + "__" + argKey
	}

	return ""
}

// chanElemTypeFromName extracts the channel element LLVM type from a concrete
// channel struct name, e.g. "sync__Channel__*counter_t" -> *%counter_t.
// Returns nil if the name doesn't follow the Channel__<elemKey> pattern or
// if the element type cannot be resolved.
func (cg *CodeGen) chanElemTypeFromName(structName string) irtypes.Type {
	const sep = "Channel__"

	idx := strings.LastIndex(structName, sep)
	if idx < 0 {
		return nil
	}

	elemKey := structName[idx+len(sep):]
	if elemKey == "" {
		return nil
	}

	te := parseTypeParamStr(elemKey)

	lt, err := cg.tinTypeToLLVM(te)
	if err != nil || lt == nil {
		return nil
	}

	return lt
}

// parseTypeParamStr converts a type-key string (as produced by
// typeExprCanonicalKey or by the parser's typeNodeToString for static
// method calls) back into an ast.TypeExpr.
//
// Supported shapes:
//
//	"foo"             -> SimpleType{"foo"}
//	"*foo"            -> PointerType{SimpleType{"foo"}}
//	"[]foo"           -> ArrayType{SimpleType{"foo"}}
//	"pkg::foo"        -> SimpleType{"pkg::foo"}    (handed off as-is)
//	"foo[bar]"        -> GenericType{"foo", [SimpleType{"bar"}]}
//	"foo[bar,baz]"    -> GenericType{"foo", [SimpleType{"bar"}, SimpleType{"baz"}]}
//	"*pkg::foo[bar]"  -> PointerType{GenericType{"pkg::foo", [SimpleType{"bar"}]}}
//
// Composes recursively, so any combination of *, [], pkg::, and [...]
// resolves correctly. Whitespace inside the brackets is trimmed per arg.
func parseTypeParamStr(s string) ast.TypeExpr {
	s = strings.TrimSpace(s)
	if s == "" {
		return &ast.SimpleType{Name: ""}
	}

	if strings.HasPrefix(s, "*") {
		return &ast.PointerType{Elem: parseTypeParamStr(s[1:])}
	}

	if strings.HasPrefix(s, "[]") {
		return &ast.ArrayType{Elem: parseTypeParamStr(s[2:]), Size: -1}
	}
	// `fn(params)ret` (or `fn#async(params)ret`) form emitted by
	// typeExprCanonicalKey for FuncType.  Walk to the matching `)` respecting
	// nested parens / brackets so a param like
	// `fn(Channel[i64], fn(i64)i64)bool` round-trips.
	isAsync := false

	bodyStart := -1

	switch {
	case strings.HasPrefix(s, "fn#async("):
		isAsync = true
		bodyStart = len("fn#async(")
	case strings.HasPrefix(s, "fn("):
		bodyStart = len("fn(")
	}

	if bodyStart > 0 {
		paren := 1

		end := -1

		for i := bodyStart; i < len(s); i++ {
			switch s[i] {
			case '(':
				paren++
			case ')':
				paren--
				if paren == 0 {
					end = i
				}
			}

			if end >= 0 {
				break
			}
		}

		if end > 0 {
			inner := s[bodyStart:end]

			var params []ast.TypeExpr

			if strings.TrimSpace(inner) != "" {
				params = splitTopLevelTypeArgs(inner)
			}

			retStr := strings.TrimSpace(s[end+1:])

			var ret ast.TypeExpr

			if retStr != "" {
				ret = parseTypeParamStr(retStr)
			}

			return &ast.FuncType{Params: params, RetType: ret, IsAsync: isAsync}
		}
	}
	// Look for the FIRST top-level `[` so we can split base[args]. Bracket
	// depth tracking keeps `Cell[*rc::Cell[i64]]` from splitting at the
	// inner `[`.
	depth := 0

	for i, c := range s {
		switch c {
		case '[':
			if depth == 0 {
				inner := s[i+1:]
				if !strings.HasSuffix(inner, "]") {
					return &ast.SimpleType{Name: s}
				}

				inner = inner[:len(inner)-1]
				baseName := s[:i]
				// Bare `[T]` / `[T; N]` is the fat- or fixed-array form,
				// not a generic instantiation. Without this branch a
				// wildcard guard like `where t is [*_]` would compare an
				// ArrayType against a GenericType{Name:""} and fail.
				if baseName == "" {
					if semi := strings.LastIndex(inner, ";"); semi >= 0 {
						elem := strings.TrimSpace(inner[:semi])
						sizeStr := strings.TrimSpace(inner[semi+1:])

						sz := -1
						if n, err := strconv.Atoi(sizeStr); err == nil {
							sz = n
						}

						return &ast.ArrayType{Elem: parseTypeParamStr(elem), Size: sz}
					}

					return &ast.ArrayType{Elem: parseTypeParamStr(inner), Size: -1}
				}

				return &ast.GenericType{Name: baseName, TypeParams: splitTopLevelTypeArgs(inner)}
			}

			depth++
		case ']':
			depth--
		}
	}

	return &ast.SimpleType{Name: s}
}

// splitTopLevelTypeArgs splits a comma-separated type-arg list while
// respecting nested `[...]` groups, then parses each piece. Used by
// parseTypeParamStr to handle multi-arg generics that may themselves
// contain commas inside their own bracket lists (e.g.
// `HashMap[string, List[i64]]`).
func splitTopLevelTypeArgs(s string) []ast.TypeExpr {
	var out []ast.TypeExpr

	bdepth := 0
	pdepth := 0
	start := 0

	for i, c := range s {
		switch c {
		case '[':
			bdepth++
		case ']':
			bdepth--
		case '(':
			pdepth++
		case ')':
			pdepth--
		case ',':
			if bdepth == 0 && pdepth == 0 {
				out = append(out, parseTypeParamStr(s[start:i]))
				start = i + 1
			}
		}
	}

	if start < len(s) {
		out = append(out, parseTypeParamStr(s[start:]))
	}

	return out
}

// tryResolveStructTypeName tries to interpret expr as a struct (or generic struct)
// type name, returning (structName, typeArgStr). structName is the base struct
// name registered in cg.structTypes or cg.genericStructsByArity; typeArgStr is the
// concrete type parameter (e.g. "i64" for Channel[i64]) or "" for non-generic.
// Returns ("", "") when expr does not resolve to a known struct type.
func (cg *CodeGen) tryResolveStructTypeName(expr ast.Node) (string, string) {
	switch e := expr.(type) {
	case *ast.Identifier:
		if cg.structTypeFor(CanonKey(e.Name)) != nil {
			return e.Name, ""
		}

		if _, ok := cg.genericStructsByArity[e.Name]; ok {
			return e.Name, ""
		}
		// Check type alias.
		if ta := cg.aliasTypeFor(CanonKey(e.Name)); ta != nil {
			if st, ok2 := ta.(*ast.SimpleType); ok2 {
				if cg.structTypeFor(CanonKey(st.Name)) != nil {
					return st.Name, ""
				}

				if _, ok3 := cg.genericStructsByArity[st.Name]; ok3 {
					return st.Name, ""
				}
			}
		}
	case *ast.ScopeAccess:
		// pkg.Type or pkg::Type - resolve via type alias.
		key := strings.Join(e.Path, ".")
		if ta := cg.aliasTypeFor(CanonKey(key)); ta != nil {
			if st, ok2 := ta.(*ast.SimpleType); ok2 {
				if cg.structTypeFor(CanonKey(st.Name)) != nil {
					return st.Name, ""
				}

				if _, ok3 := cg.genericStructsByArity[st.Name]; ok3 {
					return st.Name, ""
				}
			}
		}

		key2 := strings.Join(e.Path, "::")
		if ta := cg.aliasTypeFor(CanonKey(key2)); ta != nil {
			if st, ok2 := ta.(*ast.SimpleType); ok2 {
				if cg.structTypeFor(CanonKey(st.Name)) != nil {
					return st.Name, ""
				}

				if _, ok3 := cg.genericStructsByArity[st.Name]; ok3 {
					return st.Name, ""
				}
			}
		}
	case *ast.IndexExpr:
		// Generic instantiation: Channel[i64] or sync::Channel[i64]
		base, _ := cg.tryResolveStructTypeName(e.Expr)
		if base == "" {
			return "", ""
		}

		if typeArgID, ok := e.Index.(*ast.Identifier); ok {
			return base, typeArgID.Name
		}

		// Non-identifier index (e.g. *counter_t -> DerefExpr of "counter_t" in
		// expression context). Convert to canonical type-key string so Channel[*T]
		// resolves correctly.
		if typeArgStr := cg.exprToTypeParamKey(e.Index); typeArgStr != "" {
			return base, typeArgStr
		}

		return base, ""
	}

	return "", ""
}

// isStaticMethodIR returns true when fn's first parameter is NOT a receiver
// of structName's type (meaning the method is a static/constructor method).
func (cg *CodeGen) isStaticMethodIR(fn *ir.Func, structName string) bool {
	if len(fn.Sig.Params) == 0 {
		return true
	}

	st := cg.structTypeFor(CanonKey(structName))
	if st == nil {
		return true
	}

	first := fn.Sig.Params[0]
	if first.Equal(st) {
		return false // instance method: first param is the struct value
	}

	if pt, isPtr := first.(*irtypes.PointerType); isPtr && pt.ElemType.Equal(st) {
		return false // pointer receiver
	}

	return true
}
