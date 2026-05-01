package codegen

// mono_cache.go - step 5 of docs/plans/incremental-compilation.md.
//
// Each generic instantiation (e.g. parse[Point]) gets carved out of its
// home pkg's LLVM module into a content-addressed mono module. The
// mono module compiles to .build/mono/<hash>/bin.o; consumer pkgs see
// only an extern declaration. Multiple consumers of the SAME (template,
// type-args) pair share one cached .o.
//
// Architecture:
//   1. After codegen completes, walk constrainedFuncInstances.
//   2. For each instance: compute mono_hash from (template name +
//      sorted concrete type-args + opt-level + triple), find the fn
//      (and any $coro variant) in its home pkg module.
//   3. Move the fn(s) to a freshly created mono module keyed by hash.
//   4. The cross-module declares pass (addCrossModuleDeclares) then
//      automatically inserts a `declare` in any consumer module that
//      still references the symbol.
//
// The build driver (main.go) reads cg.MonoModules() and compiles each
// to a content-addressed cache entry under .build/mono/<hash>/bin.o.

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/llir/llvm/ir"
)

// MonoModules returns the dedicated content-addressed modules carrying
// monomorphized fn bodies. main.go feeds each into pkgCacheLookup +
// clang -c so identical (template, type-args) pairs reuse one .o.
func (cg *CodeGen) MonoModules() []*ir.Module {
	if len(cg.monoMods) == 0 {
		return nil
	}

	hashes := make([]string, 0, len(cg.monoMods))
	for h := range cg.monoMods {
		hashes = append(hashes, h)
	}

	sort.Strings(hashes)

	out := make([]*ir.Module, 0, len(hashes))
	for _, h := range hashes {
		out = append(out, cg.monoMods[h])
	}

	return out
}

// MonoModuleHashes returns the content-addressed hashes paired with
// MonoModules() in matching order.
func (cg *CodeGen) MonoModuleHashes() []string {
	if len(cg.monoMods) == 0 {
		return nil
	}

	hashes := make([]string, 0, len(cg.monoMods))
	for h := range cg.monoMods {
		hashes = append(hashes, h)
	}

	sort.Strings(hashes)

	return hashes
}

// extractMonoModules carves every monomorphized fn body into its own
// content-addressed mono module. Idempotent: subsequent calls do
// nothing if cg.monoMods is already populated.
//
// Must run BEFORE finalizePerPkgModules (which adds the cross-module
// declares that point at the relocated fn definitions).
func (cg *CodeGen) extractMonoModules() {
	if cg.monoMods != nil {
		return
	}

	cg.monoMods = map[string]*ir.Module{}

	if len(cg.constrainedFuncInstances) == 0 {
		return
	}

	type pendingMove struct {
		fn   *ir.Func
		hash string
	}

	moves := make([]pendingMove, 0, len(cg.constrainedFuncInstances))

	// Stable iteration over the instance map by IR name. The cache key
	// must be deterministic across runs for the cache to ever hit.
	names := make([]string, 0, len(cg.constrainedFuncInstances))
	for n := range cg.constrainedFuncInstances {
		names = append(names, n)
	}

	sort.Strings(names)

	for _, irName := range names {
		fn := cg.constrainedFuncInstances[irName]
		if fn == nil || fn.Parent == nil || len(fn.Blocks) == 0 {
			continue
		}
		// Skip fns whose body spawns inner closures or transitively
		// references private synthesized helpers from their pkg. After
		// extraction those references would become cross-module calls,
		// and the ARC retain/release accounting that codegen emitted
		// inline assumes the symbols live alongside the caller (e.g.
		// closure env allocation + `spawn` wiring). Extracting them
		// without also pulling in the entire local closure graph
		// breaks ARC bookkeeping (double-free under generic + spawn).
		// Conservative: leave these in place; weak_odr on the original
		// pkg module's copy already lets the linker dedupe duplicates
		// across consumers, so we lose the cache-share win for these
		// fns but keep correctness.
		if fnReferencesLocalHelpers(fn) {
			continue
		}

		hash := monoHashFor(irName, fn)
		moves = append(moves, pendingMove{fn: fn, hash: hash})

		// Sibling: any $coro variant of this fn lives alongside it
		// (in the same home module). Move both so the consumer sees
		// declares for both.
		coroName := irName + "$coro"

		for _, peer := range cg.allFuncs() {
			if peer.Name() == coroName {
				moves = append(moves, pendingMove{fn: peer, hash: hash})

				break
			}
		}
	}

	for _, mv := range moves {
		cg.relocateFnToMonoModule(mv.fn, mv.hash)
	}
}

// monoHashFor produces the content-addressed key for a monomorphized
// fn. The key includes the IR name (which already encodes template +
// concrete type args via predeclareFuncAs's mangling) plus the fn's
// own LLVM-text body. Including the body is intentionally aggressive:
// a hash collision on (irName) without body would let two distinct
// instantiations share a cache entry by accident if the mangler
// decided to reuse a name.
func monoHashFor(irName string, fn *ir.Func) string {
	sum := sha256.New()
	sum.Write([]byte(irName))
	sum.Write([]byte{0})
	sum.Write([]byte(fn.LLString()))

	return hex.EncodeToString(sum.Sum(nil))
}

// relocateFnToMonoModule moves fn from its current parent module into
// the mono module keyed by hash. The parent's Funcs slice loses the
// entry; the mono module's Funcs slice gains it. The fn's own
// .Parent pointer is updated. Idempotent per (fn, mono module).
func (cg *CodeGen) relocateFnToMonoModule(fn *ir.Func, hash string) {
	if fn.Parent == nil {
		return
	}

	target := cg.monoMods[hash]
	if target == nil {
		target = ir.NewModule()
		target.TargetTriple = cg.mod.TargetTriple
		target.DataLayout = cg.mod.DataLayout
		cg.monoMods[hash] = target
	}

	// Remove from current parent.
	src := fn.Parent

	for i, f := range src.Funcs {
		if f == fn {
			src.Funcs = append(src.Funcs[:i], src.Funcs[i+1:]...)

			break
		}
	}

	// Add to mono target.
	target.Funcs = append(target.Funcs, fn)
	fn.Parent = target
}

// fnReferencesLocalHelpers returns true when fn's body calls a coroutine
// intrinsic, spawns a fiber, or uses any helper that codegen synthesizes
// alongside the fn (closure environments, lambda fns, etc.). These
// cases tightly couple the fn to other private symbols in its home
// pkg; extracting just this fn into a mono module would create a
// cross-module reference network the simple ARC retain/release accounting
// emitted at codegen time wasn't designed for, and the result is
// double-frees under generic + spawn (see notes in extractMonoModules).
//
// Scans the LLVM textual form for call instructions whose callee
// matches a sensitive prefix. Matching the textual form (rather than
// the IR object graph) keeps the check shallow and stable across
// llir/llvm versions.
func fnReferencesLocalHelpers(fn *ir.Func) bool {
	body := fn.LLString()
	if strings.Contains(body, "@_tin_fiber_spawn") ||
		strings.Contains(body, "@_tin_fiber_join") ||
		strings.Contains(body, "@_tin_coro_malloc") ||
		strings.Contains(body, "@llvm.coro.") {
		return true
	}

	return false
}
