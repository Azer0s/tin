package codegen

// ctfe_cache.go - on-disk cache for CTFE'd #pure functions.
//
// Architecture (Phase C):
//
//   .build/pure-fn/<merkle>/
//     bin.so       - compiled shared object exporting tin_ctfe_<merkle>
//     fingerprint  - canonical text used to compute the merkle hash
//
// The merkle hash is computed bottom-up over the call graph: each #pure
// function's hash is sha256(fingerprint(fn) + sorted_hashes_of_direct_deps).
// Adding/removing a transitive dep changes the hash automatically without
// requiring a separate SBOM file: the hash IS the invalidation key.
//
// This file owns the cache layout and hash computation only. CTFE dispatch
// (dlopen / cgo / marshaling) lives in a follow-up file.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Azer0s/tin/ast"
)

// ctfeCacheRootDir is the path (relative to CWD) where per-fn cache entries
// live. Mirrors `.build/run/...` and `.build/test/...` for whole-program
// builds; `.build/pure-fn/...` is keyed by per-fn merkle hash instead of
// entry-file path.
const ctfeCacheRootDir = ".build/pure-fn"

// ctfeFnHashCache memoizes Merkle hashes per FuncDecl pointer for the
// duration of a single compilation. The recursion through funcDecls would
// otherwise re-walk identical subtrees; the cache caps work at O(N)
// where N is the number of unique #pure functions reachable.
type ctfeFnHashCache map[*ast.FuncDecl]string

// ctfeFnHash returns a stable hex-encoded sha256 fingerprint for fd that
// changes whenever the function body or any of its direct dependencies
// change. Returns "" if the fingerprint cannot be computed (e.g. fd is
// generic or references an unknown callee).
func (cg *CodeGen) ctfeFnHash(fd *ast.FuncDecl) string {
	if cg.ctfeFnHashes == nil {
		cg.ctfeFnHashes = ctfeFnHashCache{}
	}

	visiting := map[*ast.FuncDecl]bool{}

	return cg.ctfeFnHashRec(fd, visiting)
}

// ctfeFnHashRec computes the merkle hash with cycle protection. A self-call
// (or mutually recursive cycle) hashes the cycling node by name only on the
// recursive entry so that recursive functions still produce a stable hash.
func (cg *CodeGen) ctfeFnHashRec(fd *ast.FuncDecl, visiting map[*ast.FuncDecl]bool) string {
	if fd == nil {
		return ""
	}

	if h, ok := cg.ctfeFnHashes[fd]; ok {
		return h
	}

	if visiting[fd] {
		// Recursive cycle: break with a cycle-marker that captures the
		// function identity AND the depth at which the back-edge appears,
		// so two distinct cycle topologies (`f -> f` versus `f -> g -> f`)
		// can't accidentally collapse to the same marker.
		return fmt.Sprintf("cycle:%s@%d", fd.Name, len(visiting))
	}

	visiting[fd] = true
	defer delete(visiting, fd)

	var depHashes []string
	for _, dep := range cg.ctfeDirectCallees(fd) {
		h := cg.ctfeFnHashRec(dep, visiting)
		if h == "" {
			// One dep is un-hashable - bail out so we don't cache a bogus hash.
			return ""
		}

		depHashes = append(depHashes, h)
	}

	sort.Strings(depHashes)

	fingerprint := ctfeFnFingerprint(fd)

	sum := sha256.New()
	sum.Write([]byte(fingerprint))

	for _, h := range depHashes {
		sum.Write([]byte{0})
		sum.Write([]byte(h))
	}

	hashHex := hex.EncodeToString(sum.Sum(nil))
	cg.ctfeFnHashes[fd] = hashHex

	return hashHex
}

// ctfeDirectCallees walks fd.Body once and returns the FuncDecl pointer for
// every direct callee resolvable via funcDecls. Indirect calls, package
// calls, and unresolvable names are skipped (and force the outer hash to
// fail by returning the empty list - cg.ctfeFnHashRec then bails).
func (cg *CodeGen) ctfeDirectCallees(fd *ast.FuncDecl) []*ast.FuncDecl {
	var deps []*ast.FuncDecl

	seen := map[*ast.FuncDecl]bool{}

	walkAST(fd.Body, func(n ast.Node) {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return
		}

		name := resolveCalleeName(call)
		if name == "" || strings.HasPrefix(name, ".") {
			return
		}

		if strings.Contains(name, "::") {
			// Strip pkg qualifier - we resolve via funcDecls bare-name index.
			name = name[strings.LastIndex(name, "::")+2:]
		}

		dep, found := cg.funcDecls[name]
		if !found || seen[dep] {
			return
		}

		seen[dep] = true
		deps = append(deps, dep)
	})

	return deps
}

// ctfeFnFingerprint returns a canonical text representation of fd that
// captures every byte-level difference relevant to behavior: signature,
// tags, parameter names/types, return type, and a deterministic walk of
// the body. The output is stable across compiler runs; small details
// (whitespace, formatting) in the original source are normalized away.
func ctfeFnFingerprint(fd *ast.FuncDecl) string {
	var b strings.Builder

	fmt.Fprintf(&b, "fn %s\n", fd.Name)
	fmt.Fprintf(&b, "tags %s\n", strings.Join(fd.Tags, ","))
	fmt.Fprintf(&b, "static %v extern %q virtual %v\n", fd.IsStatic, fd.IsExtern, fd.IsVirtual)

	for _, p := range fd.Params {
		fmt.Fprintf(&b, "param %s %s const=%v varargs=%v\n", p.Name, typeExprText(p.Type), p.IsConst, p.IsVarArgs)
	}

	fmt.Fprintf(&b, "ret %s\n", typeExprText(fd.RetType))

	b.WriteString("body\n")
	writeNodeFingerprint(&b, fd.Body, 0)

	return b.String()
}

// typeExprText renders a TypeExpr deterministically. Returns "<nil>" for nil
// (void / inferred return).
func typeExprText(t ast.TypeExpr) string {
	if t == nil {
		return "<nil>"
	}

	return t.String()
}

// writeNodeFingerprint walks n and emits a deterministic textual record of
// its structure to b. The same shape always produces the same output; two
// equivalent ASTs hash to the same value.
func writeNodeFingerprint(b *strings.Builder, n ast.Node, depth int) {
	indent := func() {
		for i := 0; i < depth; i++ {
			b.WriteByte(' ')
		}
	}

	if n == nil {
		indent()
		b.WriteString("nil\n")

		return
	}

	indent()

	switch v := n.(type) {
	case *ast.Block:
		b.WriteString("Block\n")
		for _, s := range v.Stmts {
			writeNodeFingerprint(b, s, depth+1)
		}
	case *ast.ReturnStmt:
		b.WriteString("Return\n")
		writeNodeFingerprint(b, v.Value, depth+1)
	case *ast.IfStmt:
		b.WriteString("If\n")
		writeNodeFingerprint(b, v.Cond, depth+1)
		writeNodeFingerprint(b, v.Then, depth+1)

		for _, ei := range v.ElseIfs {
			indent()
			b.WriteString(" elif\n")
			writeNodeFingerprint(b, ei.Cond, depth+2)
			writeNodeFingerprint(b, ei.Body, depth+2)
		}

		if v.Else != nil {
			indent()
			b.WriteString(" else\n")
			writeNodeFingerprint(b, v.Else, depth+2)
		}
	case *ast.ForStmt:
		fmt.Fprintf(b, "For kind=%d var=%s\n", v.Kind, v.VarName)
		writeNodeFingerprint(b, v.Init, depth+1)
		writeNodeFingerprint(b, v.Cond, depth+1)
		writeNodeFingerprint(b, v.Post, depth+1)
		writeNodeFingerprint(b, v.Iter, depth+1)
		writeNodeFingerprint(b, v.Body, depth+1)
	case *ast.VarDecl:
		fmt.Fprintf(b, "Var %s const=%v type=%s\n", v.Name, v.IsConst, typeExprText(v.Type))
		writeNodeFingerprint(b, v.Value, depth+1)
	case *ast.AssignStmt:
		b.WriteString("Assign\n")
		writeNodeFingerprint(b, v.Target, depth+1)
		writeNodeFingerprint(b, v.Value, depth+1)
	case *ast.AugAssignStmt:
		fmt.Fprintf(b, "AugAssign %s\n", v.Op)
		writeNodeFingerprint(b, v.Target, depth+1)
		writeNodeFingerprint(b, v.Value, depth+1)
	case *ast.PostfixStmt:
		fmt.Fprintf(b, "Postfix %s\n", v.Op)
		writeNodeFingerprint(b, v.Expr, depth+1)
	case *ast.ExprStmt:
		b.WriteString("ExprStmt\n")
		writeNodeFingerprint(b, v.Expr, depth+1)
	case *ast.BinExpr:
		fmt.Fprintf(b, "Bin %s\n", v.Op)
		writeNodeFingerprint(b, v.Left, depth+1)
		writeNodeFingerprint(b, v.Right, depth+1)
	case *ast.UnaryExpr:
		fmt.Fprintf(b, "Unary %s post=%v\n", v.Op, v.Post)
		writeNodeFingerprint(b, v.Expr, depth+1)
	case *ast.TernaryExpr:
		b.WriteString("Ternary\n")
		writeNodeFingerprint(b, v.Cond, depth+1)
		writeNodeFingerprint(b, v.Then, depth+1)
		writeNodeFingerprint(b, v.Else, depth+1)
	case *ast.CallExpr:
		b.WriteString("Call ")
		writeNodeFingerprint(b, v.Func, 0)

		for _, a := range v.Args {
			writeNodeFingerprint(b, a, depth+1)
		}
	case *ast.IndexExpr:
		b.WriteString("Index\n")
		writeNodeFingerprint(b, v.Expr, depth+1)
		writeNodeFingerprint(b, v.Index, depth+1)
	case *ast.FieldAccess:
		fmt.Fprintf(b, "Field %s\n", v.Field)
		writeNodeFingerprint(b, v.Expr, depth+1)
	case *ast.ScopeAccess:
		fmt.Fprintf(b, "Scope %s\n", strings.Join(v.Path, "::"))
	case *ast.Identifier:
		fmt.Fprintf(b, "Ident %s\n", v.Name)
	case *ast.IntLit:
		if v.Big != nil {
			fmt.Fprintf(b, "IntBig %s\n", v.Big.Text(16))
		} else {
			fmt.Fprintf(b, "Int %d\n", v.Value)
		}
	case *ast.FloatLit:
		fmt.Fprintf(b, "Float %g\n", v.Value)
	case *ast.BoolLit:
		fmt.Fprintf(b, "Bool %v\n", v.Value)
	case *ast.StringLit:
		fmt.Fprintf(b, "Str %q\n", v.Value)
	case *ast.CharLit:
		fmt.Fprintf(b, "Char %d\n", v.Value)
	case *ast.NilLit:
		b.WriteString("Nil\n")
	case *ast.WildcardExpr:
		b.WriteString("Wild\n")
	case *ast.ArrayLit:
		fmt.Fprintf(b, "Array %d\n", len(v.Elems))
		for _, e := range v.Elems {
			writeNodeFingerprint(b, e, depth+1)
		}
	case *ast.ArrayFillLit:
		fmt.Fprintf(b, "ArrayFill count=%d\n", v.Count)
		writeNodeFingerprint(b, v.Value, depth+1)
	case *ast.TupleLit:
		fmt.Fprintf(b, "Tuple %d\n", len(v.Elems))
		for _, e := range v.Elems {
			writeNodeFingerprint(b, e, depth+1)
		}
	case *ast.StructLit:
		fmt.Fprintf(b, "StructLit %s\n", v.TypeName)
		for _, f := range v.Fields {
			fmt.Fprintf(b, " field %s\n", f.Name)
			writeNodeFingerprint(b, f.Value, depth+1)
		}

		for _, p := range v.Positional {
			writeNodeFingerprint(b, p, depth+1)
		}
	case *ast.RangeExpr:
		b.WriteString("Range\n")
		writeNodeFingerprint(b, v.Start, depth+1)
		writeNodeFingerprint(b, v.End, depth+1)
	case *ast.SliceExpr:
		b.WriteString("Slice\n")
		writeNodeFingerprint(b, v.Expr, depth+1)
		writeNodeFingerprint(b, v.Start, depth+1)
		writeNodeFingerprint(b, v.End, depth+1)
	case *ast.AsExpr:
		fmt.Fprintf(b, "As %s\n", typeExprText(v.Type))
		writeNodeFingerprint(b, v.Expr, depth+1)
	case *ast.IsExpr:
		fmt.Fprintf(b, "Is %s var=%s\n", typeExprText(v.Type), v.VarName)
		writeNodeFingerprint(b, v.Expr, depth+1)
		writeNodeFingerprint(b, v.Pattern, depth+1)
	case *ast.TaggedBlock:
		fmt.Fprintf(b, "Tagged %s\n", strings.Join(v.Tags, ","))
		writeNodeFingerprint(b, v.Body, depth+1)
	case *ast.WhereList:
		fmt.Fprintf(b, "Where %d\n", len(v.Clauses))
		for _, c := range v.Clauses {
			indent()
			b.WriteString(" clause\n")
			writeNodeFingerprint(b, c.Cond, depth+2)
			writeNodeFingerprint(b, c.Body, depth+2)
		}
	case *ast.MatchStmt:
		fmt.Fprintf(b, "Match istype=%v cases=%d\n", v.IsType, len(v.Cases))
		writeNodeFingerprint(b, v.Expr, depth+1)

		for _, c := range v.Cases {
			indent()
			fmt.Fprintf(b, " case %s %s\n", c.VarName, typeExprText(c.VarType))
			writeNodeFingerprint(b, c.Pattern, depth+2)
			writeNodeFingerprint(b, c.Guard, depth+2)
			writeNodeFingerprint(b, c.Body, depth+2)
		}

		if v.Default != nil {
			indent()
			b.WriteString(" default\n")
			writeNodeFingerprint(b, v.Default, depth+2)
		}
	case *ast.InterpolatedString:
		fmt.Fprintf(b, "Interp %d\n", len(v.Parts))
		for _, p := range v.Parts {
			indent()
			if p.IsExpr {
				fmt.Fprintf(b, " expr fmt=%s\n", p.Format)
				writeNodeFingerprint(b, p.Expr, depth+2)
			} else {
				fmt.Fprintf(b, " str %q\n", p.Str)
			}
		}
	default:
		fmt.Fprintf(b, "Unknown %T\n", n)
	}
}

// ctfeCacheDir returns ".build/pure-fn/<hash>" - the directory where the
// compiled shared object for a #pure function with the given Merkle hash
// is cached. Existence of the directory does NOT imply a hit; callers
// must ctfeCacheHit to confirm the bin is on disk.
func ctfeCacheDir(hash string) string {
	return filepath.Join(ctfeCacheRootDir, hash)
}

// ctfeCacheHit reports whether the cache entry for hash is complete and
// usable. The directory layout expects exactly one artifact (bin.so) and
// the hash itself is the invalidation key.
func ctfeCacheHit(hash string) bool {
	if hash == "" {
		return false
	}

	info, err := os.Stat(filepath.Join(ctfeCacheDir(hash), "bin.so"))

	return err == nil && !info.IsDir()
}

// WritePureFnCacheManifest records (hash -> shim symbol name) alongside
// bin.so so a stale or hash-collided entry can be detected on lookup.
// SHA-256 collisions are astronomically unlikely; the manifest mostly
// catches developer mistakes (e.g. a stale cache from a code state where
// two distinct fns hashed the same after a hash-function change). Read
// with readCacheManifest.
func WritePureFnCacheManifest(hash, shimName string) error {
	dir := ctfeCacheDir(hash)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(dir, "name"), []byte(shimName), 0o644)
}

// readCacheManifest returns the shim symbol name recorded in the manifest,
// or "" if the manifest doesn't exist (legacy cache entry from before we
// wrote one, or a hand-crafted entry from a test).
func readCacheManifest(hash string) string {
	data, err := os.ReadFile(filepath.Join(ctfeCacheDir(hash), "name"))
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(data))
}

// ctfeCacheBinPath returns ".build/pure-fn/<hash>/bin.so".
func ctfeCacheBinPath(hash string) string {
	return filepath.Join(ctfeCacheDir(hash), "bin.so")
}
