// Package codegen translates a tin AST into LLVM IR using the llir/llvm library.
package codegen

import (
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/metadata"
	irtypes "github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"

	"github.com/Azer0s/tin/ast"
)

// instShape records the original template name and TypeExpr
// arg list of a generic struct/data instantiation. Used by
// wildcard-guard constraint matching so nested generics like
// `Box[Pair[_, _]]` survive the `__`-mangled IR struct name.
type instShape struct {
	Tmpl string
	Args []ast.TypeExpr
}

// CodeGen

// CodeGen holds all state needed during code generation.
type CodeGen struct {
	filename string
	mod      *ir.Module

	// declared C functions
	printfFn             *ir.Func
	sprintfFn            *ir.Func
	mallocFn             *ir.Func
	freeFn               *ir.Func
	memcpyFn             *ir.Func
	echoI128Fn           *ir.Func
	echoU128Fn           *ir.Func
	echoF128Fn           *ir.Func
	echoStringEscapedFn  *ir.Func
	printStringEscapedFn *ir.Func
	i128ToCstrFn         *ir.Func
	u128ToCstrFn         *ir.Func
	f128ToCstrFn         *ir.Func

	// Unified type registry.  One record per Tin type, keyed by
	// canonical name.  Replaces the per-aspect maps (structTypes,
	// dataDecls, traits, ...) that the codegen used pre-refactor; see
	// typename.go for the record shape and accessor helpers.
	types map[CanonKey]*TypeRecord

	// struct field order: name -> []fieldName
	structFields map[string][]string
	// struct field tags: structName -> fieldName -> first @"..." tag value (empty string = untagged)
	structFieldTags map[string]map[string]string
	// struct field Tin types: structName -> []TypeExpr per user field (same order as structFields)
	structFieldTinTypes map[string][]ast.TypeExpr
	// generic struct templates: name -> arity -> AST node (not compiled directly)
	genericStructsByArity map[string]map[int]*ast.StructDecl
	// genericStructTmplFiles: source file each template was registered
	// from. Used by monomorphization to inherit per-line `//!-Wno-`
	// directives from the template's source file.
	genericStructTmplFiles map[string]string

	// trait vtable struct types: instKey -> LLVM struct type for vtable
	// trait method order: traitName -> []method name (shared across instantiations)
	traitMethodOrder map[string][]string
	// vtable globals: "structName__instKey" -> ir.Global
	traitVtableGlobals map[string]*ir.Global
	// traitBorrowVtableGlobals: parallel "borrow" vtable per
	// (structName, instKey).  Same method slots as the regular vtable
	// but the LAST slot (data-release) is a no-op so that the iface
	// release path leaves a borrowed data pointer untouched.  Used by
	// buildPtrToTraitBorrow when the source is a stack alloca (no RC
	// header to decrement) so the iface release does not read uninit
	// bytes before the alloca.  Lazily built on first use.
	traitBorrowVtableGlobals map[string]*ir.Global
	// traitDataReleaseThunks: per-struct `void(i8*)` thunk that
	// bitcasts data ptr to the struct type and calls struct.release_ptr.
	// Stored in the LAST slot of every vtable for that struct so the
	// iface's own release_ptr can dispatch via the vtable to release
	// RC-tracked fields of the wrapped struct.
	traitDataReleaseThunks map[string]*ir.Func
	// wildcardMonos: per-(impl method, target type) wrapper functions
	// for call-site generics. Each unique W (the wildcard's resolution
	// at a call site) yields a distinct function in this map; calls
	// dispatch to it directly. The wrapper internally calls the impl's
	// original method and reconstructs the result in the target type.
	// Keyed by `<origFnName>__W_<targetTypeName>`.
	wildcardMonos map[string]*ir.Func
	// instKey -> base trait name (for generic traits)
	traitInstKeys map[string]string
	// traitAsyncMethodNames: base trait name -> names of its {#async} virtual methods (in order)
	traitAsyncMethodNames map[string][]string
	// implicit conversion registry: struct name -> []entry
	implicitConvFns map[string][]implicitConvEntry
	coerceConvFns   map[string][]coerceConvEntry

	// allowExplicitPtrCoerce gates the dangerous pointer-coercion
	// paths in coerce() that turn `int -> *T`, `*Foo -> *Bar`, and
	// `string -> *char` into bitcasts.  These are valid for explicit
	// `as` casts (raw-address punning, C interop) but never for
	// implicit conversion, so genAsExpr flips the flag on for the
	// duration of its coerce call and resets it after.  Defaults
	// false everywhere else.
	allowExplicitPtrCoerce bool
	// structVtableOrder: struct name -> ordered instKeys embedded as leading fields
	structVtableOrder map[string][]string

	// enum value registry: "EnumName.Member" -> int64
	enumValues map[string]int64

	// genericTypeAliases stores the full TypeDecl for each generic type
	// alias (those with TypeParams). Needed to expand calls like
	// `StrPair[i32]{...}` - the alias substitutes its params into the RHS
	// and re-enters monomorphization on the underlying struct. Also holds
	// the alias's own where-clause bounds so they can fire on instantiation.
	genericTypeAliases map[string]*ast.TypeDecl

	// opTraitImpls indexes built-in operator-trait method impls per struct so
	// that lookupOpMethod can pick the right variant when a struct implements
	// the same op trait for multiple right-hand types (e.g. Vec3 implementing
	// both add[Vec3, Vec3] and add[f64, Vec3]).
	//
	// Keyed by structKey + "/" + traitName ("Vec3/add"). Entries are appended
	// in source order; lookup picks the first whose non-receiver param types
	// match the call site's argument types exactly.
	opTraitImpls map[string][]opTraitImplEntry
	// bare trait name -> qualified instKey (e.g. "JsonSerializable" -> "json__JsonSerializable")
	// populated when a package registers a trait so that bare-name type lookups
	// resolve to the same fat-ptr/vtable types as qualified-name lookups.
	traitBareToQualInstKey map[string]string

	// global string counter
	strCount int

	// suppressIfaceScopeRelease tells coerceToTrait to skip registering its
	// heap-allocated iface data ptr for deferred scope-exit release. Used by
	// call sites that handle the release directly (let-binding's
	// ownsIfaceData path, genForIterTrait's loop-exit release, the
	// trait init/deinit chain wrappers in genStructLit / emitReleaseInner)
	// so the data ptr isn't released twice. Stack-discipline: callers must
	// save the prior value, set true, call coerceToTrait, restore.
	suppressIfaceScopeRelease bool

	// suppressBareTypeCheck disables the strict-bare-type visibility
	// check in tinTypeToLLVM. Set during compiler-internal recursion
	// (e.g. data-decl monomorphization re-walks template methods that
	// reference the ADT by its bare template name).  Callers save the
	// prior value, set true, then restore.
	suppressBareTypeCheck bool

	// pendingOwnsPtrViaRetain is a one-shot flag set by genVarDecl
	// just before allocating the scopeEntry: true iff the binding's
	// init expression flowed through emitOwningPtrRetainIfApplicable.
	// genVarDecl reads it into entry.ownsPtrViaRetain and resets to
	// false. Used to gate the *TinStruct release on scope exit.
	pendingOwnsPtrViaRetain bool

	// curFnReturnsResult tracks whether runAstChecks is currently
	// inside a FuncDecl whose declared return type is `Result[_, _]`.
	// Non-zero => inside such a fn. The match-as-try lint consults
	// this so it only fires when `try` would actually compile.
	curFnReturnsResult int

	// coerceTransfersSource tells buildPtrToTraitBorrow that the
	// source's scope-exit release will NOT fire (e.g. genReturn
	// skipping the source via retSkipName).  In that case the iface
	// inherits the source's single rc=1 and must not retain (which
	// would leak).  Set/cleared by callers who know the transfer is
	// happening; default false (borrow semantics, retain).
	coerceTransfersSource bool
	// lastAliasBorrowVtable is set by coerceToTrait's pointer-source
	// path when the source isn't RC-managed (stack borrow, external
	// pointer).  The fat-ptr build picks it up so the iface's
	// scope-exit release is a no-op rather than calling _tin_release
	// on a header that doesn't exist.  Cleared immediately after use.
	lastAliasBorrowVtable value.Value
	// stringPool memoizes content-hashed string globals per active
	// module so the same literal in the same module reuses a single
	// global. Cross-module dedup is handled by linkonce_odr at link
	// time. Required for per-pkg compile: each pkg module needs its
	// own copies of any string it references (private-linkage strings
	// can't cross object boundaries).
	stringPool map[*ir.Module]map[string]value.Value
	// general-purpose block label counter
	labelCount int

	// current function being built
	curFn       *ir.Func
	curScope    *scope
	moduleScope *scope // root/global scope for module-level declarations

	// currentPos tracks the source position of the AST node currently being
	// compiled. Updated in genExpr and genStmt dispatch so that error messages
	// produced deeper in the call stack can reference a meaningful location.
	currentPos ast.Pos

	// pendingDeferFnI8s holds the thunk i8* function pointers for deferred calls
	// registered in the current function (LIFO on return).
	pendingDeferFnI8s []value.Value

	// pendingDeferFrames holds the i8* pointers to the TinDeferEntry allocas
	// pushed onto the runtime defer chain for this function.  They are popped
	// (without calling) before each normal return so that _tin_panic only runs
	// defers from frames that have not yet returned.
	pendingDeferFrames []value.Value

	// pendingDeferEnvs holds the malloc'd i8* env pointers corresponding to
	// each pendingDeferFnI8s entry.  The env is allocated in genDeferThunk to
	// capture free variables; it is passed to the thunk on normal return and
	// freed afterwards.
	pendingDeferEnvs []value.Value

	// Defer chain runtime functions (lazily declared).
	deferPushFn    *ir.Func            // _tin_defer_push(entry i8*, fn i8*, env i8*)
	deferPopFn     *ir.Func            // _tin_defer_pop(n i64)
	deferEntryType *irtypes.StructType // { i8*, i8*, i8* } = TinDeferEntry layout

	// tinPanicFn is the lazily declared _tin_panic(msg i8*) extern.
	tinPanicFn *ir.Func
	// memsetFn is the lazily declared llvm.memset.p0i8.i64 intrinsic.
	memsetFn *ir.Func

	// structOwningRawPtrFields: struct key -> set of `*T` raw-pointer field
	// names that have been observed receiving the address of an early-heap-
	// promoted local. The struct's release helper cascades _tin_release
	// through each such field on drop, balancing the heap allocation done
	// when the local was promoted. Recording it per-struct (rather than
	// globally on `*T` types) keeps borrow-style `*T` fields untouched.
	structOwningRawPtrFields map[string]map[string]bool

	// fnReturnsOwningIface: set of function names that return a *Trait fat
	// ptr whose `data` field carries an escape-promoted heap block. Set
	// inside buildPtrToTraitBorrow when the source is in escapingVars; read
	// by genVarDecl so `let s = make()` flags the binding for cascade-
	// release of both the iface heap block and its data field.
	fnReturnsOwningIface map[string]bool

	// fnsTouchingExtern: closure of functions that call an extern directly
	// or transitively reach one through the call graph. Computed once after
	// all functions are loaded, consulted by checkAllUnwrappedCResources to
	// flag struct fields whose value passes through a Tin call which itself
	// hits an extern (depth >1, beyond intra-struct dispatch). nil until
	// the fixpoint runs.
	fnsTouchingExtern map[string]bool

	// structWeakFields: struct key -> set of field names declared as `weak`.
	// Weak fields are non-owning: they do not retain/release their values.
	structWeakFields map[string]map[string]bool

	// structConstFields: struct key -> set of field names declared as `const`.
	// Const fields are rejected as the target of any write (plain assign,
	// aug-assign, postfix, setfield, address-of).
	structConstFields map[string]map[string]bool

	// cLayoutStructs: struct names used as *S in extern function signatures.
	// These structs use a wrapper layout: { i32 type_id, vtable_ptrs..., i8* c_data_ptr, inline_fields... }
	// All field accesses go through c_data_ptr, which points to C memory (non-handover)
	// or inline fields within the same allocation (handover/struct-literal).
	cLayoutStructs map[string]bool

	// nativeStructTypes: for each cLayoutStruct, the C-layout LLVM struct type
	// %S.native = { field_0_type, field_1_type, ... } (no type_id, no vtable).
	// Used as the GEP target when accessing fields through c_data_ptr.
	nativeStructTypes map[string]*irtypes.StructType

	// packedStructs: struct names declared with the {#packed} tag.
	// For cLayoutStructs: the %S.native type is packed (wrapper stays unpacked).
	// For regular structs: the full LLVM struct type is packed.
	packedStructs map[string]bool

	// noCopyStructs: struct names declared with the {#no_copy} tag. Value
	// copies of these are rejected at compile time (let b = a, by-value
	// param/return, struct-lit field of value type). Holding *S is fine --
	// pointer copies just retain the cell.
	noCopyStructs map[string]bool

	// closedStructs: struct names declared with the {#closed} tag. Their
	// struct literal `S{...}` may only appear inside their own static
	// methods, so external code is forced through a constructor.
	closedStructs map[string]bool
	// anyDispatchEmitted: true once the per-type-id any-release dispatch
	// table has been registered with the runtime. Set by
	// emitAnyDispatchRegistrations so the C-main wrapper, the test
	// runner, and the implicit-main path don't double-emit the calls.
	anyDispatchEmitted bool

	// localDiagSuppressions: per-file `//!-Wno-<name>` directives keyed
	// by source line. Lazily populated on the first warning emitted for
	// a given file. The directive lives on the comment line(s) directly
	// preceding the declaration the user wants to silence.
	localDiagSuppressions map[string]map[int]map[string]bool

	// structDeclsByName: every concrete struct AST seen during compilation,
	// keyed by structKey. Used by post-passes (e.g. checkAllUnwrappedCResources)
	// that need to walk fields and methods of stdlib structs in addition to
	// the user's own -- funcDecls covers methods but not the originating decl
	// + field positions, which the warning needs.
	structDeclsByName map[string]*ast.StructDecl
	// structDeclFiles: source path each structDeclsByName entry came from,
	// so warnings on stdlib decls find their //!-Wno- suppressions.
	structDeclFiles map[string]string

	// ARC runtime functions (lazily declared).
	rcAllocFn                  *ir.Func // _tin_rc_alloc(size i64) i8*
	rcAllocLocalFn             *ir.Func // _tin_rc_alloc_local(size i64) i8*  (biased: starts shared=0)
	makeSharedFn               *ir.Func //nolint:unused // _tin_make_shared(ptr i8*); kept for future biased-rc escape transition (see ensureMakeShared)
	retainFn                   *ir.Func // _tin_retain(ptr i8*)
	releaseFn                  *ir.Func // _tin_release(ptr i8*)
	retainPtrFn                *ir.Func // _tin_retain_ptr(ptr i8*) -- provenance-aware
	releasePtrFn               *ir.Func // _tin_release_ptr(ptr i8*) -- provenance-aware
	isManagedFn                *ir.Func // _tin_is_managed(ptr i8*) -> i32
	clayoutRetainFn            *ir.Func // _tin_retain_clayout(ptr i8*, flags i32)
	clayoutReleaseFn           *ir.Func // _tin_release_clayout(ptr i8*, flags i32)
	releaseStructFn            *ir.Func // _tin_release_struct(ptr i8*) i64
	releaseFatElemArrayFn      *ir.Func // _tin_release_fat_elem_array(data i8*, count i64)
	releaseAnyElemArrayFn      *ir.Func // _tin_release_any_elem_array(data i8*, count i64)
	releaseFnElemArrayFn       *ir.Func // _tin_release_fn_elem_array(data i8*, count i64)
	releaseClosureFn           *ir.Func // _tin_release_closure(env i8*)
	releaseAnyFn               *ir.Func // _tin_release_any(tag i32, data i8*)
	anyDeepCopyFn              *ir.Func // _tin_any_deepcopy(tag i32, data i8*) i8*
	foreachStructElemReleaseFn *ir.Func // _tin_foreach_struct_elem_release(data i8*, count i64, elem_size i64, fn i8*)
	foreachFixedElemReleaseFn  *ir.Func // _tin_foreach_fixed_elem_release(data i8*, count i64, elem_size i64, fn i8*)
	releasePtrElemArrayFn      *ir.Func // _tin_release_ptr_elem_array(data i8*, count i64)
	// per-type array element release helpers: type key -> IR function
	elemReleaseHelpers map[string]*ir.Func
	// per-struct null-safe pointer release helpers: struct name -> IR function.
	// Each function has signature void @{name}__release_ptr({struct}* %ptr) and
	// null-guards before loading/releasing the struct's ARC fields and freeing the block.
	structPtrReleaseFns map[string]*ir.Func
	// per-struct deep-copy helpers: struct name -> IR function with
	// signature `{struct} @{name}__deep_copy({struct} %src)`.  The
	// body walks each field: scalars and pointers copy directly,
	// string/[T] field buffers are cloned via _tin_rc_alloc + memcpy,
	// nested struct fields recurse.  Used by the call-site auto-copy
	// dispatch when a struct arg flows into a mutating callee while
	// the caller still needs the value, so the callee's mutations stay
	// isolated to its own copy.
	structDeepCopyFns map[string]*ir.Func
	// per-element deep-copy helpers used inside fat-array deep copies:
	// elemTypeKey -> `void @__tin_deepcopy_<key>_elem(i8* elem_ptr)`.
	// Each helper loads the element value, deep-copies it via
	// deepCopyFieldValue (recursive through the same type tree as
	// struct field deep copy), and stores the isolated value back into
	// the element slot.  Reuses _tin_foreach_struct_elem_retain as the
	// iteration driver since the helper signature matches.
	elemDeepCopyHelpers map[string]*ir.Func
	// per-type null-safe heap-block release helpers: type key -> IR function.
	// Same null-guard / load-then-decrement / release-fields-on-free pattern as
	// structPtrReleaseFns but for non-named element types (fat array, string,
	// any, fat fn).  Used by releaseUnreturned so an early-heap-promoted local
	// of e.g. type [string] tears down its element strings on free instead of
	// leaking them.
	heapBlockReleaseFns map[string]*ir.Func

	// Null-safe chain release helpers for depth>1 heap-owned cLayoutStruct pointers.
	// Key: "structName__chain_N" (depth N). Recursively releases inner chain then frees block.
	chainReleaseFns map[string]*ir.Func

	// Synthesized adapter functions for non-nullary ADT variant
	// constructors used as first-class values (`.map(Option::Some)`).
	// Key: `<concreteAdtName>__<variantName>__ctor`. Lazily generated
	// the first time the constructor name is referenced without a
	// CallExpr in `genDataScopeCtorCall`.
	dataCtorAdapters map[string]*ir.Func

	// Element retain helpers (for ++ concat when source is non-temporary).
	retainPtrElemsFn          *ir.Func // _tin_retain_ptr_elems(data i8*, count i64)
	retainFatElemsFn          *ir.Func // _tin_retain_fat_elems(data i8*, count i64)
	retainFnElemsFn           *ir.Func // _tin_retain_fn_elems(data i8*, count i64)
	retainAnyElemsFn          *ir.Func // _tin_retain_any_elems(data i8*, count i64)
	foreachStructElemRetainFn *ir.Func // _tin_foreach_struct_elem_retain(data i8*, count i64, elem_size i64, fn i8*)
	// per-type array element retain helpers: type key -> IR function
	elemRetainHelpers map[string]*ir.Func

	// module system
	// exports: localName -> packageName  (from ExportDecl)
	exports map[string]string
	// importedPkgs: packageName -> true  (to avoid double-loading)
	importedPkgs map[string]bool
	// loadedSrcPaths: absolute .tin file path -> true. Separate from
	// importedPkgs because the macro CTFE shell iterates importedPkgs
	// to emit `use <pkg>` lines; mixing file-path keys in there would
	// emit nonsense imports.
	loadedSrcPaths map[string]bool
	// reportedImports tracks which `use <path>` declarations have
	// surfaced via cg.progress already. genUseDecl runs twice per
	// UseDecl (load pass + codegen iteration); without this guard the
	// -v stream would show every import twice.
	reportedImports map[string]bool

	// stdlibOverride: when non-empty, overrides the default <execDir>/stdlib search path.
	// Set via --stdlib flag.
	stdlibOverride string
	// libsRoots: additional libs/ root directories searched after stdlib/.
	// Default: [<execDir>/libs]. Extended via --lib-root flags.
	libsRoots []string

	// constrained generic functions
	// constrainedFuncs: funcName -> FuncDecl template (has Constraints)
	constrainedFuncs map[string]*ast.FuncDecl
	// genericFuncs: funcName -> FuncDecl template (has TypeParams, may have no Constraints).
	// When multiple generic free fns share a base name (arity-overload), only
	// the most recently registered one lands here; the full list is
	// preserved in genericFuncOverloads so call sites can pick by arity.
	genericFuncs map[string]*ast.FuncDecl
	// genericFuncOverloads: funcName -> every generic FuncDecl template seen
	// under that name.  Populated alongside genericFuncs; used by call-site
	// dispatch when the bare lookup's arity doesn't match the call arg count.
	// Empty / single-entry for the common (non-overloaded) case.
	genericFuncOverloads map[string][]*ast.FuncDecl
	// genericFuncHomeScopes: funcName -> scope in which the template was declared.
	// Used during monomorphization so the template body can resolve bare local names.
	genericFuncHomeScopes map[string]*scope
	// constrainedFuncInstances: "funcName__typeArg" -> compiled *ir.Func
	constrainedFuncInstances map[string]*ir.Func
	// genericMethodTemplates: "structName_methodName" -> generic method FuncDecl template.
	// Methods with their own TypeParams (e.g. map_opt[r]) are stored here instead of
	// being compiled eagerly; they are monomorphized on-demand at each call site.
	// Single-overload methods read the head of the list; multi-overload
	// dispatch (`fn get[T] where T is X` / `where T is Y` / ...) walks the
	// list and picks the first entry whose where-clauses are satisfied by
	// the inferred type substitution.
	genericMethodTemplates map[string][]*ast.FuncDecl

	// Universal runtime type ID registry.
	// Primitives use anyTag* constants (0-5).  Every named struct and
	// unique function signature gets a unique i32 starting at 6.
	structTypeIDs map[string]int32 // struct name -> compile-time type ID
	fnTypeIDs     map[string]int32 // fn signature string -> compile-time type ID
	nextTypeID    int32            // counter; starts at 6

	// Reflection metadata.
	// structImpls: struct name -> []trait name strings (for traitof/typeof)
	structImpls map[string][]string

	// coerceLastErr stashes a positioned error from the most recent
	// coerce() call when the inner trait/iface coercion path wanted
	// to reject the user's input. coerce() itself returns Value (87+
	// call sites; not refactoring that signature) so the error has
	// to ride out-of-band. genVarDecl + co clear and check this
	// after each coerce() invocation. Cleared on read.
	coerceLastErr error

	// deadStrippedMethods records methods that were dropped during
	// generic-struct monomorphization because their `where t is X`
	// guard didn't hold for the concrete instantiation. Keyed by
	// concrete struct name -> method name -> list of witnesses (one
	// per stripped impl, since a method can have multiple where-
	// guarded overloads). Consumed by call-site error reporting so
	// "undefined method" can list every failing constraint instead
	// of the generic "method not found".
	deadStrippedMethods map[string]map[string][]string

	// implEntriesByMod tracks the per-module impl-section globals so the
	// finalizer can pin them via @llvm.used. Populated by
	// emitImplSectionEntry, drained by finalizeImplSection
	// (codegen/reflect_table.go).
	implEntriesByMod map[*ir.Module][]*ir.Global
	implEntriesSeen  map[string]bool

	// llvmUsedRoots is the per-module list of globals that need to be
	// pinned in @llvm.used so the linker doesn't dead-strip them. LLVM
	// rejects multiple @llvm.used per module, so every emitter (pclntab,
	// reflect_table, future ones) appends here and a single pass at the
	// end of Generate materializes one global per module.
	llvmUsedRoots map[*ir.Module][]*ir.Global

	// llvmUsedFuncs is the per-module list of functions to include in
	// the same combined @llvm.used emission (alongside llvmUsedRoots).
	llvmUsedFuncs map[*ir.Module][]*ir.Func

	// monoMods holds dedicated content-addressed modules carrying
	// monomorphized fn bodies (step 5 of incremental compilation).
	// Keyed by mono_hash; populated by extractMonoModules during
	// finalize. cmd/tin/main.go reads these via MonoModules() to drive
	// .build/mono/<hash>/bin.o caching.
	monoMods map[string]*ir.Module
	// structFieldLLVMTypes: struct name -> []LLVM type per user field (for getfield/setfield)
	structFieldLLVMTypes map[string][]irtypes.Type

	// Trait init/deinit chaining: when a struct overrides init/deinit but the
	// trait also defines one, the trait's version is compiled separately and
	// recorded here so both are invoked during struct lifecycle.
	traitChainedInits   map[string][]*ir.Func // struct name -> extra init funcs from traits
	traitChainedDeinits map[string][]*ir.Func // struct name -> extra deinit funcs from traits

	// Defer return-value override.
	// curDeferRetSlotParam: inside a defer thunk, the i8* "ret_slot" parameter
	// passed by the outer function. Non-nil only inside genDeferThunk/genDeferLambdaThunk.
	curDeferRetSlotParam value.Value
	// curFnDeferRetAlloca: in the outer function, an i8* bitcast to the alloca of
	// { i8 valid, retType value } that a deferred thunk may write an override into.
	// Nil for void functions and functions with no defers.
	curFnDeferRetAlloca value.Value
	// curDeferThunkRetType: inside a defer lambda thunk, the lambda's declared return
	// type (e.g. *i64). Used to coerce return values (e.g. None -> null *i64).
	curDeferThunkRetType irtypes.Type

	// curFnEscapingVars: set of local variable names whose addresses escape the
	// current function (e.g. `return &s` or `let p = &s; return p`).
	// These are heap-promoted: genVarDecl uses malloc instead of alloca.
	curFnEscapingVars map[string]bool

	// curFnEscapingAliases: alias map built alongside curFnEscapingVars.
	// aliases[name] = source means `let name = &source` was found in the body.
	// Used at return sites to determine which heap-promoted variables are
	// actually returned (and thus owned by caller) vs which can be freed.
	curFnEscapingAliases map[string]string

	// heapPromotingFns: set of function names that return late-heap-promoted
	// pointers (*T via _tin_rc_alloc).  Callers use this to mark the result
	// variable as isHeapOwned so scope-exit emits the correct two-step release.
	heapPromotingFns map[string]bool

	// cLayoutWrapperNativeReturnFns: wrapper name -> cLayoutStruct name.
	// Wrappers for extern functions that return a cLayoutStruct by value
	// return the C-layout %Native struct directly instead of building a
	// Tin wrapper value.  The call site allocates the storage (stack
	// composite for non-escape, _tin_rc_alloc for escape), stores the
	// native return into it, and stamps the Tin wrapper value (typeid +
	// zero vtables + c_data_ptr) inline.  Keeps the wrapper's LLVM
	// signature 1:1 with the user's Tin declaration -- no hidden params.
	cLayoutWrapperNativeReturnFns map[string]string

	// nextCLayoutStackBind, when non-empty, signals genCallExpr to allocate
	// the cLayoutStruct extern return's out_native buffer on the caller's
	// stack with an IMMORTAL_RC sentinel instead of via _tin_rc_alloc.
	// genVarDecl sets this for the duration of a non-escape let-binding RHS
	// evaluation, then clears it.  Empty string means use the heap path.
	nextCLayoutStackBind string

	// structPtrReceiverCache memoizes structHasPointerReceiverMethod's
	// answer per struct name.  The walk over funcDecls + structImpls is
	// O(N_methods + N_traits * N_methods_per_trait), invoked once per
	// cLayout-returning let-binding; without a cache the cost compounds
	// quadratically with the number of cLayout-returning let-bindings in
	// large codebases.
	//
	// The companion `*Sig` fields snapshot the size signal that drove the
	// last cache fill: outer map lengths catch additions of new structs
	// / functions, while structImplsSumLen catches trait-impl slices
	// being EXTENDED on an existing struct key (a trait added late by
	// monomorphization or REPL cell that wouldn't change
	// len(structImpls)).  Any mismatch triggers a flush + lazy recompute.
	// True overwrites (same key, replaced value -- rare outside REPL)
	// still need an explicit invalidation; InvalidateStructPtrReceiverCache
	// is exposed for callers that know they've done one.
	structPtrReceiverCache                  map[string]bool
	structPtrReceiverCacheFuncDeclsLen      int
	structPtrReceiverCacheStructImplsN      int
	structPtrReceiverCacheStructImplsSumLen int

	// curStructLitOuterIsLocal is set by maybeMarkCLayoutStackBind when
	// the enclosing let-binding holds a StructLit whose outer struct
	// doesn't escape.  genStructLit consults this flag while emitting
	// each field initializer: if the value is a cLayoutStruct-returning
	// wrapper call, it sets nextCLayoutStackBind for that single field
	// so the inner cLayoutStruct stack-binds alongside the outer's
	// caller-frame storage.  Cleared by genVarDecl's defer.
	curStructLitOuterIsLocal bool

	// curFnAstBody is the AST body of the function or test being codegen'd.
	// Set by genFuncDeclAs and the test runner before emitting the body, used
	// by escape analysis on let-bindings (which need to walk the surrounding
	// function body, including for TestDecls that aren't in funcDecls).
	curFnAstBody ast.Node

	// fnReturnsHeapPromotedFields: map from function name to the list of
	// struct-field indices in the returned struct value whose stored
	// pointer is a heap-promoted `&local`.  Populated when emitting
	// `return Struct{field: &x}` in a function whose `x` was flagged
	// as escaping; consulted at the receiving call site so the binding
	// scope exit can release the owning pointer (otherwise the heap
	// block leaks: the per-struct release helper treats raw-pointer
	// fields as borrows and skips them).
	fnReturnsHeapPromotedFields map[string][]int
	// match subject: set before entering genWhereList when the function body
	// is a pure where-list pattern match. Used to compare atom conditions.
	matchSubject value.Value

	// TCO (tail call optimization) state for the current function.
	// tcoFuncName is the Tin name of the function being TCO'd; non-empty only
	// while compiling a TCO-eligible function body.
	tcoFuncName string
	// tcoLoopTop is the "tco_loop" block that param-update branches jump back to.
	tcoLoopTop *ir.Block
	// tcoParams lists the parameter names in declaration order, used to locate
	// the right alloca when rewriting a tail self-call as a loop-back.
	tcoParams []string

	// mutualTCOEligible is true while compiling a function that can emit
	// musttail calls for mutual tail recursion (non-RC params/return, no defers).
	mutualTCOEligible bool

	// curIsRefIterGet is true while compiling a `fn ref_iter::get` impl.
	// Switches the entry retain of `this` and the return retain of the
	// `*T` result off: the caller of these methods is a for-ref loop
	// (or a loop-like context) that already pins the source container,
	// so the borrow they emit is a pure no-op that costs four atomic
	// ops per iteration on the hot path.
	curIsRefIterGet bool

	// tcoReportFn is called when a TCO transformation is applied.
	// caller is the function being compiled; callee is the target (empty for self-TCO).
	tcoReportFn func(caller, callee string)

	// strcmpFn: lazily declared C strcmp
	strcmpFn *ir.Func
	// tinStrMemcmpFn: lazily declared _tin_str_memcmp (length-aware
	// string equality; used for Tin string == /!= so we don't read
	// past the slice boundary like strcmp would).
	tinStrMemcmpFn *ir.Func
	// anyEqFn: lazily declared _tin_any_eq runtime helper
	anyEqFn *ir.Func

	// macros: macro name -> MacroDecl AST
	macros map[string]*ast.MacroDecl

	// funcDecls: function name -> FuncDecl AST, populated during predeclaration.
	// Used by the #pure transitive side-effect checker.
	funcDecls map[string]*ast.FuncDecl

	// fnParamConventions caches per-function param conventions
	// (transparent / consumes / retains) computed lazily by
	// analyzeFunctionParamConventions on first call-site lookup.
	// Read by `ref a` to validate the call-site assertion against the
	// callee's analyzer-classified convention.
	fnParamConventions map[string]map[string]ParamConvention

	// fnParamMutated caches the per-function "does the body mutate
	// this param" decision (any compound-target write, identifier
	// reassign, or write-through-pointer).  Read at call sites to
	// auto-pick deep_copy when a struct-typed arg flows into a
	// mutating callee while the caller still needs the value.
	fnParamMutated map[string]map[string]bool

	// callArgContextStack carries (callee-name, ordered param names,
	// arg AST nodes) for the call currently being evaluated.
	// genCallExpr pushes on entry, pops on exit; genRefExpr reads the
	// top frame to look up the expected convention for the current arg
	// position and emits a compile error if the callee does not
	// classify that param as transparent.
	callArgContextStack []callArgContext

	// implicitMoveSites is the set of *ast.Identifier nodes a per-
	// function liveness pre-pass classified as the binding's single
	// read site.  When such an identifier appears as a call argument
	// codegen lowers it with move semantics: post-call release and
	// the binding's scope-exit release is elided (the binding is
	// marked ownership-moved, so subsequent reads would also error).
	// Computed once per function body before codegen via
	// computeImplicitMoveSites; reset at function boundary.
	implicitMoveSites map[*ast.Identifier]bool

	// methodMayMutateReceiverByType: structName -> methodName -> true
	// when that specific (struct, method) pair has a pointer receiver
	// and can therefore mutate `this`.  collectMutatedTargets prefers
	// this map when it can resolve the receiver expression's type:
	// distinguishing (A, foo) from (B, foo) lets value-receiver
	// foo-on-A skip the autocopy even when foo-on-B has a *B
	// receiver.  Falls back to the bare-name map (over-approximate)
	// when the receiver type cannot be inferred from the call site.
	methodMayMutateReceiverByType map[string]map[string]bool

	// methodMayMutateReceiver: method base name -> true if any
	// definition with this base name takes a pointer receiver (and
	// therefore could mutate the receiver's storage through `*this`).
	// Populated during predeclareMethod.  Used by the borrow
	// analyzer to decide whether `t.method(...)` should force `t`
	// to Owned: an entry of true means yes, conservatively; absence
	// (or false) means every definition has a value receiver and
	// the call cannot mutate the caller's binding, so the analyzer
	// can keep `t` as a candidate borrow.
	methodMayMutateReceiver map[string]bool

	// curFnSyncLocal is true when codegenning the body of a function
	// that the call-graph analyzer proved is NOT reachable from any
	// {#async} root.  Such a function never runs on a fiber, its
	// captures never cross a thread, and ARC blocks it allocates
	// stay confined to the spawn-free caller -- so retain/release
	// on those blocks can use non-atomic ops.  Codegen routes
	// _tin_rc_alloc calls through _tin_rc_alloc_local when this
	// flag is set; the runtime still upgrades to atomic on any
	// _tin_make_shared if the analysis turned out wrong.
	// See docs/15-ownership.md "Biased reference counting".
	curFnSyncLocal bool

	// spawnerReachable: function-name set populated by
	// computeSpawnerReachable.  A function is "spawner-reachable"
	// when it directly contains a spawn/await OR transitively
	// calls a function that does.  Used by the biased-RC analyzer:
	// any value flowing through such a function might end up on a
	// spawned fiber's thread, so its allocs must use the atomic
	// (shared=1) allocator.
	spawnerReachable map[string]bool

	// globalMutators: global var name -> set of fn names that may
	// mutate that global (directly or transitively).  Built once via
	// computeGlobalMutators after the call graph is populated.  The
	// borrow analyzer consults this map to relax the "no borrow of
	// global aliases" rule: `let t = some_global` is borrow-safe
	// when no callee in the body's call closure mutates the global.
	globalMutators map[string]map[string]bool

	// ctfeCache memoizes the result of tryEvalPureCallToCtfeVal keyed by a
	// fingerprint of (function name, argument values). A repeated call with
	// the same args during one compilation unit reuses the prior result
	// rather than re-walking the body. Cleared per Generate() invocation.
	ctfeCache map[string]ctfeMemoEntry

	// ctfeFnHashes memoizes the Merkle hash for each #pure FuncDecl. Used
	// by the on-disk pure-fn cache (.build/pure-fn/<hash>/) so the recursive
	// hash walk visits each function at most once per compilation.
	ctfeFnHashes ctfeFnHashCache

	// pureFnShims tracks which #pure functions had a `__tin_pure_shim_<name>`
	// emitted by emitPureFnCtfeShims. The per-fn .so cache emit consults
	// this set so the slicer knows to (a) include the shim in the slice and
	// (b) promote its linkage from internal to external for dlsym.
	pureFnShims map[string]bool

	// pureFoldDisabled, when true, makes tryEvalPureCall a no-op so the
	// generator emits the call as a runtime invocation. Driven by the
	// `--no-pure-fold` CLI flag. Useful when a faulty #pure body would
	// hang or panic the evaluator at compile time, or when comparing
	// optimized vs unoptimized binaries during compiler debugging.
	pureFoldDisabled bool

	// stacktraceUsed is set when codegen recognizes a `stacktrace()`
	// builtin call. cmd/tin/main.go branches on the post-Generate value to decide
	// whether to emit unwind tables, link libunwind, and pass `-rdynamic`
	// (see docs/plans/stacktrace-libunwind.md "Conditional unwind-table
	// emission"). When false the program pays zero binary-size or runtime
	// cost for stacktrace-related machinery.
	stacktraceUsed bool

	// pclntab state: emitted at the end of Generate (applyPclntabPostPass)
	// when stacktraceUsed is true. Replaces the libdw / DWARF dependency
	// for stacktrace symbol resolution with a Go-style PC -> file:line:col
	// table embedded in a custom binary section. See codegen/pclntab.go.
	// pclntabUsed is set when stacktrace is reachable (mirrors
	// stacktraceUsed). Distinct from debugMode: pclntabUsed only
	// enables per-inst line:col capture into instLineCol, while
	// debugMode also emits DICompileUnit / DISubprogram / DILocation
	// nodes that materialize as DWARF sections in the final binary.
	pclntabUsed bool

	// instLineCol stores per-instruction (line, col) source positions
	// captured at attach time even when debug mode is off. Pclntab's
	// post-pass walks fn.Blocks and reads from this map (preferring it
	// over !dbg metadata) to anchor per-call PC entries.
	instLineCol map[ir.Instruction]ast.Pos

	pclntabPCType  *irtypes.StructType // {i32 pc_off, i32 line, i32 col}
	pclntabHdrType *irtypes.StructType // per-fn header
	pclntabHdrs    []*ir.Global        // emitted hdrs (pinned via @llvm.used)
	// pclntabSeq is a single monotonic counter feeding suffix numbers for
	// every pclntab-internal symbol (hdr, pcs, string pool entries, split
	// block labels). Names are namespaced by their PREFIX (`__tin_pcln_hdr.`,
	// `__tin_pcs.`, `__tin_pcln_s.`, `<bb>.split.`), so cross-kind ID
	// collisions are impossible - a single monotonic ID just keeps the
	// state minimal.
	pclntabSeq              int
	pclntabStringPoolPerMod map[*ir.Module]map[string]pclntabStringEntry // per-fn-module string pools
	pclntabCtorFn           *ir.Func                                     // ctor created in pre-marker phase, finalized after
	fnSourceFiles           map[string]string                            // ir-fn-name -> source .tin path
	// fnDisplayNames maps mangled IR names back to user-readable Tin names
	// for stacktrace display. Populated at predeclare time so the original
	// AST context (package, struct receiver, generic type-args) is in hand.
	// Format examples:
	//   "sync__AtomicI64_deinit" -> "sync::AtomicI64.deinit"
	//   "make__i64"              -> "make[i64]"
	//   "_tin_user_main"         -> "main"
	//   "foo$coro"               -> "foo$coro"  (passthrough for $coro variants;
	//                                            display layer keeps the marker)
	fnDisplayNames map[string]string

	// curMethodReceiverStruct is the struct name when the codegen flow is
	// emitting a struct method (genStructMethod sets it; helpers used by
	// fn-emit pick it up). Empty when the current fn is not a method.
	curMethodReceiverStruct string

	// pureFoldBudget caps the total node-evaluation work spent on a
	// single top-level #pure call (sum across all loops, recursion, and
	// nested call expansions). When the budget is exhausted the
	// evaluator returns errNotConst and the call falls back to runtime
	// dispatch - same outcome as a non-foldable signature, but reached
	// safely instead of pathologically. 0 means "use the default"
	// (defaultPureFoldBudget); negative values are forbidden.
	pureFoldBudget int

	// topLevelVarPos records the source position where each top-level
	// `let`/`var`/`const` was declared, keyed by name. Populated as
	// declarations are processed in Generate; consumed by the
	// `sourcepos(symbol)` builtin to resolve a symbol's definition site
	// when no AST node is in hand. Nested scopes don't go in here -
	// scopeEntry.declPos covers locals separately.
	topLevelVarPos map[string]ast.Pos

	// pureFoldBudgetRemaining tracks how many evalNode visits are still
	// allowed for the currently-evaluating top-level #pure call. The
	// counter is reset at every entry into tryEvalPureCallToCtfeVal and
	// decremented once per evalNode call. When it hits zero the
	// evaluator unwinds with errCTFEBudget. Not goroutine-safe - codegen
	// is single-threaded by construction.
	pureFoldBudgetRemaining int

	// shimMod hosts every CTFE shim (the wrappers emitPureFnCtfeShims
	// produces). Kept entirely separate from cg.mod so the user binary's
	// IR never carries shim definitions; the per-fn .so emit combines
	// shimMod's text with sliced cg.mod text.
	shimMod *ir.Module

	// activeMod points at the module currently receiving NewFunc/NewGlobal
	// calls from interop.go. Equal to cg.mod outside shim emission;
	// swapped to shimMod for the duration of emitPureFnCtfeShims so the
	// wrapper machinery writes into the CTFE module instead.
	activeMod *ir.Module

	// runtimeHelperCache memoizes the `declare` for each runtime-helper
	// symbol (tin_interop_str_in, tin_runtime_init_once, etc.) per target
	// module. ensureXxx in interop.go consults this so the same wrapper
	// body can be emitted into either cg.mod or shimMod and end up calling
	// declares that live in the same module.
	runtimeHelperCache map[*ir.Module]map[string]*ir.Func

	// pkgMods maps Tin package name -> per-pkg LLVM module. Lazily populated
	// by pkgMod() in pkgmod.go. Foundation for incremental compilation
	// step 2: each pkg eventually gets its own .ll/.o so the build can
	// parallelize and cache per pkg. Empty until call sites start routing.
	pkgMods map[string]*ir.Module

	// echoedTypes tracks which named struct types have already been echoed
	// into a given target module by echoTypeInActive. Per (module, typeName)
	// idempotence prevents duplicate TypeDef entries when multiple call
	// sites cross-reference the same type from a foreign pkg module.
	echoedTypes map[*ir.Module]map[string]bool

	// externIRNames: IR names of C extern functions. Populated by ensureExternDecl.
	// Used to detect collisions when a Tin user function has the same name as a C symbol.
	externIRNames map[string]bool

	// externABIs: per-extern logical signature for callExtern's byval /
	// sret wrap.  Populated by declareABILoweredExtern; consulted by
	// callExtern.  Keyed by the *ir.Func declaration (per-pkg-module
	// distinct).
	externABIs map[*ir.Func]externABI

	// sretCallResults: for sret-lowered extern calls, the value the
	// caller sees is a `load` from the sret slot, not the `call` itself.
	// The release / retain bookkeeping (isFreshBytesAlloc,
	// isFreshCallResult) probes the IR node's runtime type to detect
	// "this is a fresh rc=1 value from a call", which fails for a load.
	// This map lets the lookup paper over the indirection by mapping each
	// sret-load back to the underlying *ir.InstCall.  Populated by
	// callExtern; read by the fresh-result detectors.
	sretCallResults map[*ir.InstLoad]*ir.InstCall

	// externTLSVars: extern thread-local global variables declared in the IR.
	// Keyed by C variable name. Populated by ensureExternTLSVar.
	externTLSVars map[string]*ir.Global

	// linkLibs: libraries to pass to the linker (from `use extern` lib entries)
	linkLibs []string

	// pkgSrcPaths: paths of all .tin source files loaded as packages.
	// Populated by loadPackageFromSource so callers can scan them for //! directives.
	pkgSrcPaths []string

	// test mode: when true, TestDecl blocks are compiled into test functions
	// and a test-runner main is generated instead of the normal implicit main.
	testMode  bool
	testDecls []*ast.TestDecl

	// noRuntimeChecks disables emission of runtime safety checks like the
	// `++=` borrowed-view (cap < 0) panic.  Trades a single icmp-and-branch
	// per write site for the chance of silently mutating shared / immortal
	// storage; only meaningful for tight loops that have been audited.
	noRuntimeChecks bool

	// explainOwnershipSpec, when non-empty, asks the borrow optimizer to
	// emit a per-binding ownership report at end of codegen. "*" prints
	// everything; "fnName" filters by function; "file.tin:fnName"
	// filters by both. Set via --explain-ownership[=spec].
	explainOwnershipSpec string
	// explainOwnershipReport accumulates "fn: bindingName ownership note"
	// lines as the analyzer classifies each binding. Flushed to stderr
	// by FinalizeExplainOwnership at the end of the compilation unit.
	explainOwnershipReport []ownershipReportEntry

	// currentFnBorrowSet is the per-function output of
	// analyzeFunctionBorrows: names of bindings the analyzer
	// classified as Borrowed for the function currently being
	// codegenned. Reset at function entry. Empty when
	// ownershipBorrowEnabled is false.
	currentFnBorrowSet map[string]bool

	// movedBindings tracks names of let-bindings that have been
	// explicitly moved via `move x` within the function body
	// currently being codegenned. genIdentifier consults this set
	// at every read and raises use-after-move on names that appear.
	// Reset at function entry alongside currentFnBorrowSet.
	movedBindings map[string]bool
	// pendingMoveSelfName is set during VarDecl codegen when the
	// initializer is a MoveExpr targeting the same binding name (the
	// pathological `let x = move x` case).  genMoveExpr checks this
	// and errors with a clearer message than the generic
	// use-after-move path would produce.
	pendingMoveSelfName string

	// partialMovedStack is a stack of "binding is partially moved
	// across this branching construct" sets.  Pushed by genIf /
	// genMatch before codegenning their branches when the pre-analysis
	// shows the binding is moved on some paths but not all.
	// genMoveExpr consults the stack and emits a balancing retain
	// before reading the value, so that the merged outer scope's
	// release path stays rc-balanced regardless of which branch
	// runs.  See docs/15-ownership.md "Per-branch move tracking".
	partialMovedStack []map[string]bool

	// diags tracks per-warning suppression / escalation preferences. Keyed
	// by canonical diagnostic name (see codegen/diag.go for constants).
	diags map[string]*diagState

	// allWarnsAsErrors escalates every diagnostic emitted via warn() to a
	// hard error. Toggled by -Werror.
	allWarnsAsErrors bool

	// hadWarnError records that at least one diagnostic was promoted to an
	// error. Inspected by Generate's caller to fail the build.
	hadWarnError bool

	// unsafeDepth tracks lexical nesting of `{#unsafe} { ... }` blocks.
	// Operations like `addr(int_literal)` are rejected with a compile
	// error when this is zero.  Pointer arithmetic is permitted at
	// zero depth only under a transient-consumption context (deref,
	// field access, index, comparison, chained arithmetic, integer
	// cast); see transientPtrAllowed.
	unsafeDepth int

	// transientPtrAllowed is set by callers that are about to consume
	// a pointer-arithmetic expression in-place (`*(p + n)`,
	// `(p + n).field`, `(p + n)[i]`, comparisons, chained arithmetic,
	// `(p + n) as i64`).  The BinExpr-arithmetic emitter accepts ptr
	// arithmetic outside `{#unsafe}` only when this is true.  Callers
	// save / restore around the recursion so a permitted parent
	// doesn't accidentally bless deeper non-transient uses.
	transientPtrAllowed bool

	// identExprContext lets genIdentifier give a fix-it that matches the
	// shape the bare type name actually appeared in: an array-literal
	// element vs a match-case pattern vs a generic expression.  Callers
	// set the value, recurse into genExpr, then restore on the way out.
	// Without this, the diagnostic falls back to the "match by type"
	// hint -- which is wrong for the much more common gh#27 case of
	// `[*Leaf] * n` where the user wanted a multi-init array fill.
	identExprContext string

	// dfSuppressWarnings is non-zero while the dataflow pass is iterating
	// a loop body to fixpoint. The first few iterations see a transient
	// state where loop-modified locals still appear to hold their init
	// values, so flow-sensitive checks would fire phantom warnings ("if
	// epoch % 500 == 0 is always true" on iteration 0 of `for let epoch
	// = 0; ...; epoch++`). dfWalkLoop suppresses warnings during the
	// fixpoint iterations, then replays one final body walk with the
	// converged input state so warnings that survive the widening still
	// fire.
	dfSuppressWarnings int

	// verboseMatchInfo dumps the Maranget pattern matrix and per-arm
	// reachability decisions for every match / where the compiler sees.
	// Toggled by -fdump-match-info; for debugging the algorithm itself.
	verboseMatchInfo bool

	// verboseDemorgan prints each boolean simplification the compiler
	// applies (De Morgan push-inward, double-negation elim, comparison
	// negation, bool-literal absorption). Toggled by -fdump-demorgan.
	verboseDemorgan bool

	// emitHeaderPath, when non-empty, instructs codegen to write a C
	// header file listing every #interop function's prototype. Toggled
	// by --emit-header=<path>.
	emitHeaderPath string

	// interopCbThunks caches per-signature thunks emitted to bridge
	// raw C function pointers into Tin's fat fn-ptr calling convention.
	// Keyed by sanitized (ret, params) signature.
	interopCbThunks map[string]*ir.Func

	// interopDispatchers caches per-signature dispatchers used to
	// invoke a Tin closure returned to C through a mmap'd trampoline.
	// The dispatcher's first IR instruction reads %r10 (set by the
	// trampoline) to recover the fat-fn-ptr address, then tail-calls
	// fn(env, args...). Keyed by sanitized (ret, params) signature.
	interopDispatchers map[string]*ir.Func

	// cFnShims caches per-signature shims that wrap a raw C function
	// pointer (returned from an extern with a fn-typed RetType) into a
	// Tin fat-fn-ptr.  The shim signature matches the Tin fat-fn-ptr's
	// inner fn type `fn(i8* env, params...) ret`; the body bitcasts env
	// back to the C fn ptr and calls through it.  Keyed by the same
	// (ret, params) signature key as interopDispatchers.
	cFnShims map[string]*ir.Func

	// makeTrampolineFn caches the `tin_make_trampoline` extern
	// declaration so multiple ensureMakeTrampoline call paths share
	// one declaration rather than each adding their own (which `opt`
	// rejects as a duplicate).
	makeTrampolineFn *ir.Func

	// coroWrappers caches the per-fn `$coro_wrap` synthesized by
	// ensureCoroWrapperFor (fat-fn-ptr slot 0).  Scope-based lookup
	// is unreliable across module/package boundaries -- this keyed
	// by the source fn's mangled name guarantees module-wide uniqueness.
	coroWrappers map[string]*ir.Func

	// interopPackedStructs is the set of `#packed` struct names
	// reachable from the program. Populated by checkAllInteropFuncs;
	// consulted by the validator and the wrapper emitter.
	interopPackedStructs map[string]bool

	// mutatedNames is the set of identifier names that are reassigned
	// anywhere inside the current function body (including closures and
	// defers). Populated per function body in genFuncDeclAs and consulted
	// by the if-condition folder (codegen/fold.go) to suppress folding of
	// identifiers whose value can change between the let binding and the
	// if-condition evaluation site. Reset to nil between functions.
	mutatedNames map[string]bool

	// userMainDecl is the AST node for the user's explicit fn main(), saved
	// during genFuncDecl so the wrapper can inspect params and return type.
	userMainDecl *ast.FuncDecl

	// Debug info (DWARF, -g flag)

	// debugMode enables DWARF debug metadata emission.
	debugMode bool
	// diFiles: source file path -> DIFile node (cached per filename).
	diFiles map[string]*metadata.DIFile
	// diCU is the single compile unit for this module.
	diCU *metadata.DICompileUnit
	// diCurrentScope is the current DWARF scope (DISubprogram or DILexicalBlock).
	diCurrentScope metadata.Field
	// diTypeCache caches diTypeFor results by type name string.
	diTypeCache map[string]metadata.Field
	// dbgDeclareFn is the lazily declared llvm.dbg.declare intrinsic.
	dbgDeclareFn *ir.Func
	// emittingARC is true while emitting ARC retain/release/deinit calls.
	// Instructions emitted in this context get line=0 !dbg so the debugger
	// does not stop on invisible compiler-generated operations.
	emittingARC bool

	// useDoubleForF128: when true, the f128 type is lowered to f64/double instead
	// of fp128. Used on Apple arm64 where long double == double and compiler-rt
	// does not provide the fp128 software-float routines (___eqtf2 etc.).
	useDoubleForF128 bool

	// Atom type and registry.
	// atomType is the named LLVM struct %__atom = type { i32 }.
	// atomCodes maps atom name -> CRC32 code (collision-resolved).
	// atomCodeToName is the reverse map for collision detection.
	// atomOrder holds insertion order for stable @__tin_atom_table output.
	atomType            *irtypes.StructType
	atomCodes           map[string]int32
	atomCodeToName      map[int32]string
	atomOrder           []string
	atomToStrFn         *ir.Func // __tin_atom_to_string(i32) {i8*,i64}
	strToAtomFn         *ir.Func // __tin_string_to_atom(i8*) %__atom
	strToAtomHandoverFn *ir.Func // __tin_string_to_atom_handover(i8*) %__atom

	// Tagged union registry: type name -> ordered variant TypeExprs (index = tag).
	// Created by "type u = i8 | string" declarations.
	unionTypeMembers map[string][]ast.TypeExpr

	// Native union registry: struct name -> UnionDecl AST.
	// Created by "union u = as_i8 i8 | as_string string" declarations.
	nativeUnionDecls map[string]*ast.UnionDecl

	// Tagged union type ID registry: union name -> compile-time i32 type ID.
	// Same purpose as structTypeIDs/dataTypeIDs - used for any boxing and typeof.
	unionTypeIDs map[string]int32

	// ADT registry: `data T = V0 | V1(...)` declarations.
	// Layout mirrors tagged unions: { i32 type_id, i8 tag, [N x i8] payload }.
	// dataDecls[name]       -> the original DataDecl AST
	// dataTypeIDs[name]     -> compile-time i32 type ID (same pool as structs/unions)
	// dataVariants[adt][v]  -> per-variant info (tag, payload struct, fields)
	// dataVariantLookup[v]  -> list of ADT names that declare a variant named v;
	//                         used to resolve bare constructor references.
	dataTypeIDs          map[string]int32
	dataVariants         map[string]map[string]*dataVariantInfo
	dataVariantLookup    map[string][]string
	dataValueReleaseFns  map[string]*ir.Func
	dataValueRetainFns   map[string]*ir.Func
	dataValueDeepCopyFns map[string]*ir.Func

	// anyDeepCopyThunks: per-struct `i8*(i8*)` thunks the runtime
	// dispatcher (_tin_any_deepcopy) calls when a type-id matches.
	// Populated lazily by ensureAnyDeepCopyThunk and registered at
	// module init alongside the per-type release thunks.
	anyDeepCopyThunks map[string]*ir.Func

	// Fiber / coroutine state

	// LLVM coroutine intrinsics (lazily declared by ensureCoroIntrinsics).
	coroIDFn      *ir.Func
	coroAllocFn   *ir.Func
	coroSizeFn    *ir.Func
	coroBeginFn   *ir.Func
	coroSuspendFn *ir.Func
	coroEndFn     *ir.Func
	coroFreeFn    *ir.Func
	coroResumeFn  *ir.Func // llvm.coro.resume - used by coroutine chaining
	coroDoneFn    *ir.Func // llvm.coro.done - used by coroutine chaining
	coroDestroyFn *ir.Func // llvm.coro.destroy - used by coroutine chaining

	// Fiber runtime functions (lazily declared by ensureFiberRuntime).
	fiberSpawnFn              *ir.Func
	fiberSpawnJoinableFn      *ir.Func // _tin_fiber_spawn_joinable: sets prejoined=1 on TinFiber
	fiberSpawnChainFn         *ir.Func // _tin_fiber_spawn_chain: stacktrace-aware
	fiberSpawnJoinableChainFn *ir.Func // _tin_fiber_spawn_joinable_chain: prejoined+stacktrace
	llvmReturnAddressFn       *ir.Func // llvm.returnaddress intrinsic for spawn-site IP capture
	// spawnFireForget: when true, activeSpawnFn() returns fiberSpawnFn (prejoined=0).
	// Set only for statement-level SpawnExprs whose result is explicitly discarded.
	// All other spawns use fiberSpawnJoinableFn (prejoined=1) by default so that
	// stored futures can be awaited later without racing against ff_reclaim and
	// pid reuse.
	spawnFireForget    bool
	fiberCompleteFn    *ir.Func
	fiberJoinFn        *ir.Func // _tin_fiber_join(pid i64, hdl i8*): register waiter
	fiberGetResultFn   *ir.Func // _tin_fiber_get_result(pid i64) -> i8*
	fiberGetPanicMsgFn *ir.Func // _tin_fiber_get_panic_msg(pid i64) -> i8* (null = ok)
	fiberCheckPanicFn  *ir.Func // _tin_fiber_take_pending_panic() -> i8*: per-fiber back-edge re-raise hook
	coroTakeResultFn   *ir.Func // _tin_coro_take_result() -> i8*: for chaining
	fiberYieldCoroFn   *ir.Func
	currentCoroHdlFn   *ir.Func // _tin_current_coro_hdl() -> i8*: TLS lookup, used by $colored yields
	fiberInitFn        *ir.Func
	fiberRunFn         *ir.Func
	ioInitFn           *ir.Func

	// REPL mode: when true, top-level `let` bindings are promoted to LLVM
	// globals, main() generation is skipped, and cg.replNewGlobals is populated.
	replMode         bool
	replCellFuncName string // e.g. "_repl_cell_3"
	replNewGlobals   []ReplGlobal
	// replExternalGlobals: globals defined by previous REPL cells.
	// Re-injected as 'external' linkage so all cells share the canonical copy.
	replExternalGlobals map[string]bool
	// replCellGlobals: globals created by this cell's let-promotions.
	// Keyed by Tin name; survives across function-scope pops so the $coro
	// variant of the cell function can find and reuse them.
	replCellGlobals map[string]*ir.Global

	// coroCallable: set of function names that need a $coro duplicate.
	// Built by colorCallGraph() after the predeclaration pass.
	coroCallable map[string]bool

	// coloredCallable: set of function names that need a $colored
	// sync variant emitted (slot 1 of the fat-fn-ptr ABI).  Built by
	// colorCallGraph(): seeds with sync fns called from {#async}
	// bodies and fns referenced as values (boxed into a fat-fn-ptr),
	// then BFS-propagates through cg.callGraph -- a colored body
	// routes its sync callees to their colored variants, so any sync
	// callee transitively reached from a colored entry point also
	// needs a colored emission.  See docs/internals/fn-coloring.md.
	coloredCallable map[string]bool

	// boxedFns: set of fn names referenced as values (without an
	// immediate call) anywhere in the program.  Populated by
	// collectBoxedFns before colorCallGraph; serves as a root set
	// for coloredCallable BFS so any fn that can flow into a
	// fat-fn-ptr value has its $colored variant emitted (slot 1).
	boxedFns map[string]bool

	// fnParkingClass caches the may-park result computed by
	// astchecks.fnBodyMayPark for each Tin fn (keyed by funcDecl name).
	// `true` means the body transitively reaches a yield / await /
	// known-parking primitive; `false` means it is pure compute.  The
	// cache is populated lazily during -Wbare-parking-async-call
	// dispatch and also serves to break recursion in the walker.
	fnParkingClass map[string]bool

	// knownParkingExterns names the C runtime primitives whose Tin-level
	// bare-call would park the calling thread.  Used to seed the
	// may-park analysis at extern-call boundaries -- the analysis can't
	// see into C bodies, so an explicit roster is the only way to
	// classify them.  Synced with runtime/*.c manually; missing entries
	// only weaken `-Wbare-parking-async-call` (false negative), they
	// don't compromise the rest of the runtime.
	knownParkingExterns map[string]bool

	// callGraph: funcName -> []callee names. Built during predeclaration.
	callGraph map[string][]string

	// funcHeuristics: function name -> heuristic analysis result.
	// Populated by computeAutoYieldHeuristics() after colorCallGraph().
	// Used by genCallSiteYieldFor to decide whether to emit a coro.suspend
	// before calling a given function.
	funcHeuristics map[string]*FuncHeuristicInfo

	// verboseHeuristics enables per-function heuristic output to stderr.
	// Activated by the -fdump-heuristics CLI flag.
	verboseHeuristics bool

	// progressFn, if non-nil, is called with a short human-readable message at
	// each notable compilation event (pass boundaries, function generation,
	// package imports, CTFE evaluations, macro expansions).
	// Set via SetProgressFunc; used by the -v CLI flag.
	progressFn func(string)

	// Per-function coro state (valid only when genCoroFuncBody is active).
	inCoroFn       bool
	curFnAutoYield bool // true in $coro variant of #async functions without #no_autoyield
	// curFnColoredSync: true while emitting a $colored sync body.
	// Switches the yield-instruction emitter from llvm.coro.suspend
	// (intrinsic, requires a coro frame) to
	// _tin_fiber_yield_coro(_tin_current_coro_hdl()) (runtime call,
	// uses TLS-tracked hdl).  A $colored body has no frame of its
	// own; it borrows the caller's via TLS.
	curFnColoredSync bool
	curCoroHdl       value.Value  // %hdl i8* in the current coro function
	curCoroID        value.Value  // %id token in the current coro function
	curCoroCleanup   *ir.Block    // cleanup block for the current coro function
	curCoroFrame     *coroFrame   // full frame for the current coro function
	curCoroRetType   irtypes.Type // original return type of current $coro function

	// yieldResumeBlocks: IR blocks that are resume-points after an explicit
	// `yield` statement.  At loop backedges, if `from` is in this set the
	// fiber was just unparked from an explicit yield - autoyield would add a
	// redundant second suspension.  Suppressing it saves one round-trip per
	// blocked iteration.  Replaces the `#no_autoyield` language annotation.
	yieldResumeBlocks map[*ir.Block]bool

	// usesAnyFiber is set to true when the program contains at least one
	// spawn/await/yield, so main() is wrapped with fiber init + run.
	usesAnyFiber bool

	// spawnDoCounter is incremented each time a `spawn do:` block is synthesized
	// to generate unique anonymous function names (__spawn_do_N).
	spawnDoCounter int

	// syncModuleLoaded is set to true once the stdlib/sync module has been
	// auto-loaded by ensureSyncModule.  Prevents double-loading.
	syncModuleLoaded bool
	// syncLoadErr holds the error from the most recent ensureSyncModule call,
	// so wrapPidInFuture can report it if Future[T] is not available.
	syncLoadErr error

	// runtimeBuiltinLoaded is set to true once runtime/builtin/ has been
	// auto-loaded. Idempotent guard - see ensureRuntimeBuiltinModules.
	runtimeBuiltinLoaded bool

	// lastSliceBase is a side-channel set by genSliceExpr to communicate the
	// base allocation pointer (i8*, before any GEP offset) to genVarDecl.
	// genVarDecl reads and clears it immediately after calling genExpr on a SliceExpr
	// so that ARC retain/release operates on the real base pointer, not the interior
	// pointer that may be stored in the slice fat-ptr's field 0.
	lastSliceBase value.Value

	// lastLambdaHadCaptures is set by genLambdaExpr to indicate whether the most
	// recently emitted lambda closure had any captured variables.  genVarDecl reads
	// and clears it to mark non-capturing closure vars noRelease=true so that
	// emitScopeRelease skips the redundant _tin_release_closure(null) call.
	lastLambdaHadCaptures bool

	// curBlock tracks the current IR block during expression generation.
	// It is updated by genExpr when control flow changes the active block
	// (e.g. await/yield emit a coro.suspend which switches to a new resume block).
	// Callers that need to continue emitting into the correct block after a
	// potentially-suspending expression should read cg.curBlock after genExpr.
	curBlock *ir.Block

	// breakStack is a stack of "after" blocks for the innermost enclosing loop.
	// pushBreak/popBreak are called around loop body generation; genBreakStmt
	// emits a branch to the top of the stack so break works correctly.
	breakStack      []*ir.Block
	breakUsedStack  []bool   // parallel to breakStack: true if any break was emitted
	breakScopeStack []*scope // parallel to breakStack: scope before the loop body (break releases up to here)

	// topLevelVarInits: deferred runtime-expression initializers for top-level
	// var declarations. They are emitted at the top of implicit/explicit main.
	topLevelVarInits []topLevelVarInit

	// allTopLevelVars: ALL top-level var declarations in declaration order.
	// Used to emit deinits in reverse order at the end of main().
	allTopLevelVars []topLevelVarInit

	// deinitAllFn / deinitArmedGlobal / atexitFn back the
	// `_tin_deinit_all` dispatcher registered via atexit() in the C
	// wrapper main. Lazily emitted by emitDeinitAllFn /
	// emitDeinitAllAtexit. Without this, top-level var deinits only
	// run on fall-through-from-main; with it, deinits run on any
	// clean-exit path (return, libc exit(N), etc.).
	deinitAllFn       *ir.Func
	deinitArmedGlobal *ir.Global
	atexitFn          *ir.Func

	// topLevelVarBareNames: bare (un-mangled) names of every top-level `var`
	// across the entry program and all imported packages. Used by the #pure
	// soundness check to reject reads/writes of mutable globals from a #pure
	// body. Populated lazily before checkAllPureFuncs runs.
	topLevelVarBareNames map[string]bool
	// topLevelConstNames is the subset of topLevelVarBareNames whose
	// declarations were `const` (not `var`). Read by #pure verification
	// to skip the "reads mutable top-level var" diagnostic for consts.
	topLevelConstNames map[string]bool

	// pkgInitFns: init functions collected from packages that declare
	// fn init(). Called at program startup after top-level var inits,
	// in import order (dependencies before dependents).
	pkgInitFns []*ir.Func

	// Function overloading

	// overloadedNames: base name (or "StructName_method") -> true when multiple
	// definitions with the same name exist in the current module.
	// Populated by a pre-scan pass before predeclaration.
	overloadedNames map[string]bool

	// overloads: base name -> slice of registered variants (mangled IR names,
	// resolved LLVM param types, arity).  Populated during predeclaration.
	overloads map[string][]*overloadEntry

	// genericMethodsSetUp: concrete generic struct name -> true once its overload
	// entries and method stubs have been predeclared (prevents double-registration
	// when genTypeDecl is called more than once for the same concrete type).
	genericMethodsSetUp map[string]bool

	// funcReturnUnsigned: IR function name -> true when the function's return
	// type is an unsigned integer (u8/u16/u32/u64/u128).  Populated during
	// predeclaration so exprIsUnsigned can correctly format CallExpr results.
	funcReturnUnsigned map[string]bool

	// currentPkg is the package name currently being compiled via
	// loadPackageFromSource (e.g. "sync", "io").  It is set before the
	// preregister pass and cleared after the package scope is restored.
	// Used by pkgStructKey to produce canonical LLVM struct names.
	currentPkg string

	// currentPkgPath is the fully-qualified package path being compiled
	// (e.g. "encoding::base16", "http").  Used to build display names for
	// typeof() so that 'encoding::base16::MyType is stable and unambiguous.
	// "std::" prefix is stripped so `use std::io` and `use io` both give "io".
	currentPkgPath string

	// returnTypeHint is the LLVM type expected at the current call site, set by
	// genVarDecl when the let binding has an explicit type annotation. It guides
	// overload resolution so that e.g. `let v f32x4 = simd::splat(3.0)` picks
	// the f32x4 overload over f64x2. Cleared immediately after the call is resolved.
	returnTypeHint irtypes.Type

	// preEvaledArgVals stashes the arg-value list when the call site
	// has to evaluate args early to disambiguate among generic overloads
	// of the same arity (see pickGenericFuncOverload).  The downstream
	// monomorphization / call-emit code consumes this and nils it so
	// the eval doesn't fire twice (and re-trigger side effects).
	preEvaledArgVals []value.Value

	// pipeCurriedRetHint passes the LHS of `a |> f(args)` through to
	// pickGenericFuncOverload so the picker can prefer the overload
	// whose return-fn first-param shape matches the LHS.  Lets
	// `[t]` and `*Seq[t]` curried-pipe overloads coexist under the
	// same bare name.  Set by genPipeExpr before evaluating the RHS,
	// cleared immediately after.
	pipeCurriedRetHint irtypes.Type

	// aliasResolving guards against cycles in tinTypeToLLVM's alias-
	// chain resolution.  Set per-resolution and cleared on exit.
	aliasResolving map[string]bool

	// lambdaSelfName carries the let-binding name through genVarDecl into
	// genLambdaExpr so the lambda body can call itself recursively.
	// Without this, `let fact = fn(n i64) i64 = fact(n-1)` would fail
	// at the recursive call site with "undefined identifier: fact" --
	// the name isn't registered in scope until AFTER the lambda body
	// returns, which is too late for the body's own references.  The
	// emitter pre-registers the lambda's IR func under this name (as a
	// fat-fn-ptr alloca built from the body's own env param) before
	// walking the body.  Cleared immediately after use.
	lambdaSelfName string

	// indexExprRawTuple is set by genTupleDestructDecl while it evaluates its
	// RHS. When the RHS is a `t[k]` whose ::index impl returns (V, bool),
	// genIndexExpr would normally auto-unwrap (extract V + panic if !ok); in
	// destructure context we want the raw tuple so the destructure step can
	// bind both names. Cleared after the RHS evaluation.
	indexExprRawTuple bool
	// dfSkipIndexCheck holds IndexExpr nodes whose -Wunchecked-index
	// pedantic check should stay silent because the access is
	// destructured via `let (v, ok) = t[k]` (the safe form for
	// custom `::index` impls).  Populated by the dataflow pass's
	// TupleDestructDecl handler before walking the IndexExpr; the
	// dfCheckUncheckedIndex visit then consults this set.
	dfSkipIndexCheck map[*ast.IndexExpr]bool
	// andersenPts holds the interprocedural points-to summary
	// computed by runAndersen.  Used by the dataflow pass's
	// -Wunchecked-returned-nil check to surface nil flow across
	// function boundaries that the intraprocedural pass can't see.
	andersenPts map[ptVar]map[ptToken]bool
	// dfCurFnName is the name of the function currently being
	// analyzed by the dataflow pass.  Set by dfAnalyzeFunc on
	// entry, cleared on exit.  Used by dfCheckExpr to key Andersen
	// points-to lookups (which are scoped per fn).
	dfCurFnName string
	// manualAllocSites records the source position where each
	// manually-allocated binding was first seen.  Used by
	// dfCheckManualAllocLeaks to point the diagnostic at the
	// `let p = mem::malloc(...)` site rather than the function
	// body's closing brace.  Reset at the start of each fn's
	// dataflow run.
	manualAllocSites map[string]ast.Pos
	// paramFrees is the interprocedural summary "fn F frees its
	// param at position I via mem::free".  Computed to fixpoint by
	// computeManualAllocSummaries before the dataflow pass runs.
	// Used at call sites: if the callee frees param i and the
	// caller passes a Live binding at position i, the binding
	// transitions to Freed (not Escaped) in the caller's state,
	// eliminating the false-positive leak warning on the
	// `let p = mem::malloc(); helper(p); // helper frees p` shape.
	paramFrees map[string]map[int]bool
	// returnsAlloc[F] is true when fn F's body has a return
	// statement whose value is a mem::malloc/calloc/realloc/alloc
	// call OR a call to another returnsAlloc fn.  Drives the
	// caller-side `let p = make()` initialisation: when the callee
	// returnsAlloc, p starts Live (the caller now owns the
	// allocation and must `mem::free` it).
	returnsAlloc map[string]bool
}

// topLevelVarInit holds a deferred runtime initializer for a top-level var.
// pkgName is "" for the entry program's own top-level vars, or the
// importing pkg name (e.g. "sync", "io") for vars declared inside an
// imported pkg's source. Used by emitDeinitAllFn to group deinits per
// pkg so the dispatcher can call `_tin_deinit_<pkg>` in reverse topo
// order rather than walking a flat list.
type topLevelVarInit struct {
	name     string
	global   *ir.Global
	initExpr ast.Node
	pkgName  string
}

// pushBreakTarget pushes afterBlock onto the break stack before generating a
// loop body.  The matching popBreakTarget must be called after.
// Must be called after the loop body scope has been pushed (cg.curScope is the
// body scope), so that cg.curScope.parent is the scope before the loop.
