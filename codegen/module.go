package codegen

// module.go - module file (.tin.mod) serialization and deserialization.
//
// A .tin.mod file is a JSON file that describes all symbols exported by a Tin
// package.  It acts like a C header: the importing compiler reads declarations
// to emit extern references; the actual code is supplied by linking the
// compiled object file.
//
// Symbol naming convention
// A function `foo` exported as package `mylib` gets the IR name `mylib__foo`.
// Importing modules declare `mylib__foo` as extern and register the scope key
// `mylib.foo` pointing to it, so `mylib::foo(...)` resolves correctly.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/Azer0s/tin/ast"
)

// Module file format

// ModFunc describes an exported function.
type ModFunc struct {
	// LocalName is the unqualified name (e.g. "print").
	LocalName string `json:"name"`
	// IRName is the mangled LLVM symbol (e.g. "io__print").
	IRName string `json:"irName"`
	// ExternName, if non-empty, is the underlying C symbol this wraps (e.g. "puts").
	// The loader will create a C extern declaration + fat-pointer wrapper instead
	// of expecting a pre-compiled object file.
	ExternName string `json:"externName,omitempty"`
	// Params holds the Tin type string for each parameter (e.g. "string", "*i8").
	Params []ModParam `json:"params"`
	// RetType holds the Tin type string of the return value ("" = void).
	RetType string `json:"retType"`
	// Variadic is true for variadic functions.
	Variadic bool `json:"variadic,omitempty"`
}

// ModParam is a named parameter entry in the module file.
type ModParam struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// ModStruct describes an exported struct type.
type ModStruct struct {
	// LocalName is the unqualified struct name.
	LocalName string `json:"name"`
	// IRName is the mangled IR type name (e.g. "io__point").
	IRName string `json:"irName"`
	// Fields lists field names and their Tin types.
	Fields []ModParam `json:"fields"`
	// Methods lists exported method signatures (bodies not included).
	Methods []ModFunc `json:"methods,omitempty"`
}

// ModTypeAlias describes a type alias exported by a package.
type ModTypeAlias struct {
	Name   string `json:"name"`
	Target string `json:"target"`
}

// ModMacro describes an exported macro.
type ModMacro struct {
	// Name is the macro name (without trailing !).
	Name string `json:"name"`
	// Body is the backtick expansion string (e.g. "for true" for loop).
	Body string `json:"body"`
	// Tags lists the macro control tags (e.g. ["no_parens", "no_excl"]).
	Tags []string `json:"tags,omitempty"`
	// Params lists parameter names for parametric macros.
	Params []string `json:"params,omitempty"`
}

// ModFile is the top-level structure written to / read from a .tin.mod file.
type ModFile struct {
	Package string         `json:"package"`
	Funcs   []ModFunc      `json:"functions,omitempty"`
	Structs []ModStruct    `json:"structs,omitempty"`
	Types   []ModTypeAlias `json:"types,omitempty"`
	// Macros lists exported macros so they can be imported cross-module.
	Macros []ModMacro `json:"macros,omitempty"`
	// ReExports lists other packages that are re-exported under this package.
	// e.g. "export { io, math } as std" -> ReExports: ["io", "math"]
	ReExports []string `json:"reExports,omitempty"`
}

// I/O

// WriteModFile serializes mf to filename.
func WriteModFile(filename string, mf *ModFile) error {
	data, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal module file: %w", err)
	}

	return os.WriteFile(filename, data, 0o644)
}

// ReadModFile reads and deserializes a .tin.mod file.
func ReadModFile(filename string) (*ModFile, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	var mf ModFile
	if err := json.Unmarshal(data, &mf); err != nil {
		return nil, fmt.Errorf("parse module file %s: %w", filename, err)
	}

	return &mf, nil
}

// Type serialization helpers

// typeExprToString converts an AST type expression to its Tin source form.
// The result can be parsed back with parser.ParseType.
func typeExprToString(te ast.TypeExpr) string {
	if te == nil {
		return ""
	}

	switch t := te.(type) {
	case *ast.SimpleType:
		return t.Name
	case *ast.PointerType:
		if t.IsConst {
			return "const *" + typeExprToString(t.Elem)
		}

		return "*" + typeExprToString(t.Elem)
	case *ast.ArrayType:
		if t.Size < 0 {
			return "[" + typeExprToString(t.Elem) + "]"
		}

		return fmt.Sprintf("[%s; %d]", typeExprToString(t.Elem), t.Size)
	case *ast.FuncType:
		parts := make([]string, len(t.Params))
		for i, p := range t.Params {
			parts[i] = typeExprToString(p)
		}

		ret := ""
		if t.RetType != nil {
			ret = " " + typeExprToString(t.RetType)
		}

		suffix := ""
		if t.IsVarArgs {
			suffix = ", ..."
		}

		return "fn(" + strings.Join(parts, ", ") + suffix + ")" + ret
	case *ast.GenericType:
		params := make([]string, len(t.TypeParams))
		for i, p := range t.TypeParams {
			params[i] = typeExprToString(p)
		}

		return t.Name + "[" + strings.Join(params, ", ") + "]"
	case *ast.UnionTypeExpr:
		parts := make([]string, len(t.Types))
		for i, u := range t.Types {
			parts[i] = typeExprToString(u)
		}

		return strings.Join(parts, " | ")
	}

	return ""
}
