// Package codegen translates a tin AST into LLVM IR using the llir/llvm library.
package codegen

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
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
	// tinRecoverFn is the lazily declared _tin_recover() -> TinString extern.
	tinRecoverFn *ir.Func
	// sliceSubsliceFn is the lazily declared _tin_slice_subslice extern.
	sliceSubsliceFn *ir.Func
	// sliceConvertIntFn is the lazily declared _tin_slice_convert_int extern.
	sliceConvertIntFn *ir.Func
	// bytesFromBufFn is the lazily declared _tin_bytes_from_buf extern.
	bytesFromBufFn *ir.Func
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
	retainFn                   *ir.Func // _tin_retain(ptr i8*)
	releaseFn                  *ir.Func // _tin_release(ptr i8*)
	releaseStructFn            *ir.Func // _tin_release_struct(ptr i8*) i64
	releaseFatElemArrayFn      *ir.Func // _tin_release_fat_elem_array(data i8*, count i64)
	releaseAnyElemArrayFn      *ir.Func // _tin_release_any_elem_array(data i8*, count i64)
	releaseFnElemArrayFn       *ir.Func // _tin_release_fn_elem_array(data i8*, count i64)
	releaseClosureFn           *ir.Func // _tin_release_closure(env i8*)
	releaseAnyFn               *ir.Func // _tin_release_any(tag i32, data i8*)
	foreachStructElemReleaseFn *ir.Func // _tin_foreach_struct_elem_release(data i8*, count i64, elem_size i64, fn i8*)
	foreachFixedElemReleaseFn  *ir.Func // _tin_foreach_fixed_elem_release(data i8*, count i64, elem_size i64, fn i8*)
	releasePtrElemArrayFn      *ir.Func // _tin_release_ptr_elem_array(data i8*, count i64)
	// per-type array element release helpers: type key -> IR function
	elemReleaseHelpers map[string]*ir.Func
	// per-struct null-safe pointer release helpers: struct name -> IR function.
	// Each function has signature void @{name}__release_ptr({struct}* %ptr) and
	// null-guards before loading/releasing the struct's ARC fields and freeing the block.
	structPtrReleaseFns map[string]*ir.Func
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
	genericMethodTemplates map[string]*ast.FuncDecl

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
	// finalize. main.go reads these via MonoModules() to drive
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

	// tcoReportFn is called when a TCO transformation is applied.
	// caller is the function being compiled; callee is the target (empty for self-TCO).
	tcoReportFn func(caller, callee string)

	// strcmpFn: lazily declared C strcmp
	strcmpFn *ir.Func
	// anyEqFn: lazily declared _tin_any_eq runtime helper
	anyEqFn *ir.Func

	// macros: macro name -> MacroDecl AST
	macros map[string]*ast.MacroDecl

	// funcDecls: function name -> FuncDecl AST, populated during predeclaration.
	// Used by the #pure transitive side-effect checker.
	funcDecls map[string]*ast.FuncDecl

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
	// builtin call. main.go branches on the post-Generate value to decide
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
	// Operations like raw pointer arithmetic and `addr(int_literal)` are
	// rejected with a compile error when this is zero.
	unsafeDepth int

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
	dataTypeIDs         map[string]int32
	dataVariants        map[string]map[string]*dataVariantInfo
	dataVariantLookup   map[string][]string
	dataValueReleaseFns map[string]*ir.Func
	dataValueRetainFns  map[string]*ir.Func

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
	fiberSpawnChainFn         *ir.Func // _tin_fiber_spawn_chain: stacktrace-aware (Phase 4)
	fiberSpawnJoinableChainFn *ir.Func // _tin_fiber_spawn_joinable_chain: prejoined+stacktrace
	llvmReturnAddressFn       *ir.Func // llvm.returnaddress intrinsic for spawn-site IP capture
	// spawnFireForget: when true, activeSpawnFn() returns fiberSpawnFn (prejoined=0).
	// Set only for statement-level SpawnExprs whose result is explicitly discarded.
	// All other spawns use fiberSpawnJoinableFn (prejoined=1) by default so that
	// stored futures can be awaited later without racing against ff_reclaim and
	// pid reuse.
	spawnFireForget    bool
	fiberCompleteFn    *ir.Func
	fiberJoinFn        *ir.Func   // _tin_fiber_join(pid i64, hdl i8*): register waiter
	fiberGetResultFn   *ir.Func   // _tin_fiber_get_result(pid i64) -> i8*
	fiberGetPanicMsgFn *ir.Func   // _tin_fiber_get_panic_msg(pid i64) -> i8* (null = ok)
	fiberCheckPanicFn  *ir.Func   // _tin_fiber_check_panic() -> i8*: unhandled panic check at yield points
	panicFlagGlobal    *ir.Global // _has_unhandled_panics: fast-path flag for the two-level panic check
	coroTakeResultFn   *ir.Func   // _tin_coro_take_result() -> i8*: for chaining
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
func (cg *CodeGen) pushBreakTarget(afterBlock *ir.Block) {
	cg.breakStack = append(cg.breakStack, afterBlock)
	cg.breakUsedStack = append(cg.breakUsedStack, false)
	// Record the scope before the loop body so that break can release
	// all variables declared inside the loop up to (not including) this scope.
	var outerScope *scope
	if cg.curScope != nil {
		outerScope = cg.curScope.parent
	}

	cg.breakScopeStack = append(cg.breakScopeStack, outerScope)
}

// popBreakTarget removes the innermost break target after loop body generation.
// Returns true if any break statement was emitted to this target.
func (cg *CodeGen) popBreakTarget() bool {
	if len(cg.breakStack) == 0 {
		return false
	}

	used := cg.breakUsedStack[len(cg.breakUsedStack)-1]
	cg.breakStack = cg.breakStack[:len(cg.breakStack)-1]
	cg.breakUsedStack = cg.breakUsedStack[:len(cg.breakUsedStack)-1]
	cg.breakScopeStack = cg.breakScopeStack[:len(cg.breakScopeStack)-1]

	return used
}

// currentBreakTarget returns the innermost loop's after-block, or nil if not in a loop.
func (cg *CodeGen) currentBreakTarget() *ir.Block {
	if len(cg.breakStack) == 0 {
		return nil
	}

	return cg.breakStack[len(cg.breakStack)-1]
}

// currentBreakScope returns the scope before the innermost loop body, or nil.
// On break, variables in scopes from cg.curScope up to (not including) this scope
// must be released before branching to the break target.
func (cg *CodeGen) currentBreakScope() *scope {
	if len(cg.breakScopeStack) == 0 {
		return nil
	}

	return cg.breakScopeStack[len(cg.breakScopeStack)-1]
}

// markBreakUsed records that a break was emitted to the current break target.
func (cg *CodeGen) markBreakUsed() {
	if len(cg.breakUsedStack) > 0 {
		cg.breakUsedStack[len(cg.breakUsedStack)-1] = true
	}
}

// pkgStructKey returns the canonical map key / LLVM IR name for a struct named
// "name" that is being compiled under the current package.  When currentPkg is
// set (i.e. we are inside loadPackageFromSource), the returned key is
// "pkgName__name" so that structs from different packages never collide even
// when they share the same short name.  For user-level structs (currentPkg="")
// the bare name is returned unchanged.
func (cg *CodeGen) pkgStructKey(name string) string {
	if cg.currentPkg != "" {
		key := cg.currentPkg + "__" + name
		displayPkg := cg.currentPkgPath

		if displayPkg == "" {
			displayPkg = cg.currentPkg
		}

		cg.recordDisplay(CanonKey(key), displayPkg+"::"+name)

		return key
	}

	return name
}

// newBlock creates a uniquely-named basic block in the current function.
// Sequential if/for/match statements in the same function reuse label base
// names (e.g. "if.merge") which produces duplicate labels in the IR and
// confuses LLVM's loop-deletion pass.  Always routing through this helper
// ensures every block in a function has a distinct name.
func (cg *CodeGen) newBlock(base string) *ir.Block {
	id := cg.labelCount
	cg.labelCount++

	return cg.curFn.NewBlock(fmt.Sprintf("%s.%d", base, id))
}

// SetTestMode enables test-mode compilation: test blocks are compiled into
// test functions and a test-runner main() is generated.
func (cg *CodeGen) SetTestMode(v bool)         { cg.testMode = v }
func (cg *CodeGen) SetVerboseMatchInfo(v bool) { cg.verboseMatchInfo = v }

// SetNoWarnAsyncMain is the -Wno-async-main hook (kept for back-compat with
// existing callers; new code should use SetWarnSuppress(DiagAsyncMain)).
func (cg *CodeGen) SetNoWarnAsyncMain(v bool) {
	if v {
		cg.SetWarnSuppress(DiagAsyncMain)
	}
}

// SetNoWarnUnusedMatchArms is the -Wno-unused-match-arms hook.
func (cg *CodeGen) SetNoWarnUnusedMatchArms(v bool) {
	if v {
		cg.SetWarnSuppress(DiagUnusedMatchArms)
	}
}

// SetNoWarnBoolAnalysis is the -Wno-bool-analysis hook.
func (cg *CodeGen) SetNoWarnBoolAnalysis(v bool) {
	if v {
		cg.SetWarnSuppress(DiagBoolAnalysis)
	}
}
func (cg *CodeGen) SetVerboseDemorgan(v bool)  { cg.verboseDemorgan = v }
func (cg *CodeGen) SetEmitHeaderPath(p string) { cg.emitHeaderPath = p }
func (cg *CodeGen) SetUseDoubleForF128(v bool) { cg.useDoubleForF128 = v }
func (cg *CodeGen) SetTargetTriple(triple string) {
	if triple != "" {
		cg.mod.TargetTriple = triple
	}
}
func (cg *CodeGen) SetVerboseHeuristics(v bool)                     { cg.verboseHeuristics = v }
func (cg *CodeGen) SetProgressFunc(fn func(string))                 { cg.progressFn = fn }
func (cg *CodeGen) SetTCOReportFunc(fn func(caller, callee string)) { cg.tcoReportFn = fn }

// SetPureFoldDisabled toggles compile-time evaluation of #pure calls.
// When true, both tier-1 (AST evaluator) and tier-2 (cached .so dispatch)
// are short-circuited, and every #pure call codegens as a regular runtime
// invocation. The user-visible behavior of #pure (purity contract,
// alwaysinline, readnone, no_recurse depth limit) is unchanged - only
// the constant-folding optimization is suppressed. Driven by the
// `--no-pure-fold` CLI flag.
func (cg *CodeGen) SetPureFoldDisabled(v bool) { cg.pureFoldDisabled = v }

// SetPureFoldBudget overrides the per-call evaluation budget cap used by
// the CTFE evaluator. Pass 0 to keep the default (defaultPureFoldBudget);
// any negative value is treated as 0. Each call to evalNode consumes one
// unit; the budget is reset at the top-level entry into the evaluator.
// On exhaustion the call falls back to runtime evaluation just as if
// the body weren't foldable.
func (cg *CodeGen) SetPureFoldBudget(n int) {
	if n < 0 {
		n = 0
	}

	cg.pureFoldBudget = n
}

// StacktraceUsed reports whether any reachable call site referenced the
// `stacktrace()` builtin. main.go consults this after Generate() returns
// to decide whether to link libunwind, emit unwind tables, and pass
// `-rdynamic` (the conditional-emission story in
// docs/plans/stacktrace-libunwind.md). Stable through the rest of the
// build; once set true it stays true.
func (cg *CodeGen) StacktraceUsed() bool { return cg.stacktraceUsed }

// PkgModules returns per-package LLVM modules (excluding cg.mod) in
// deterministic alphabetical name order. Empty when no `use` decls
// loaded any pkg, OR when mergeRoutedPkgMods has folded everything
// back into cg.mod (which happens at the end of Generate today).
//
// main.go uses this to drive per-pkg .o compilation in parallel; the
// linker then combines them with cg.mod into the final binary.
// Currently returns nil because the merge step still folds everything
// into cg.mod for the legacy single-module compile path; once main.go
// is wired to compile each module separately, the merge step gets
// removed and this returns the live per-pkg modules.
func (cg *CodeGen) PkgModules() []*ir.Module {
	if len(cg.pkgMods) == 0 {
		return nil
	}

	out := make([]*ir.Module, 0, len(cg.pkgMods))
	for _, name := range cg.pkgModNames() {
		if m := cg.pkgMods[name]; m != nil && hasPkgContent(m) {
			out = append(out, m)
		}
	}

	return out
}

// PkgModuleNames returns the names paired with PkgModules() in the
// same order. Used by the build driver to label per-pkg .ll / .o
// artifacts.
func (cg *CodeGen) PkgModuleNames() []string {
	if len(cg.pkgMods) == 0 {
		return nil
	}

	out := make([]string, 0, len(cg.pkgMods))
	for _, name := range cg.pkgModNames() {
		if m := cg.pkgMods[name]; m != nil && hasPkgContent(m) {
			out = append(out, name)
		}
	}

	return out
}

// hasPkgContent reports whether m carries any IR worth compiling. Pkg
// modules that only declared types (everything DCE'd away) skip the
// per-pkg .o emit so we don't waste a clang invocation on a no-op.
func hasPkgContent(m *ir.Module) bool {
	return len(m.Funcs) > 0 || len(m.Globals) > 0 || len(m.Aliases) > 0
}

// progress fires the optional progress callback with msg.  Callers use it to
// report named pass boundaries, per-function events, imports, CTFE, and macros.
func (cg *CodeGen) progress(msg string) {
	if cg.progressFn != nil {
		cg.progressFn(msg)
	}
}
func (cg *CodeGen) SetDebugMode(v bool) { cg.debugMode = v }

// HasTests reports whether the source contained at least one test block.
// Only meaningful after Generate has been called.
func (cg *CodeGen) HasTests() bool { return len(cg.testDecls) > 0 }

// targetIsAMD64 reports whether the module's target triple is an x86-64 target.
// Used in place of runtime.GOARCH so that cross-compilation works correctly:
// the decision is based on the compilation target, not the host.
func (cg *CodeGen) targetIsAMD64() bool {
	return strings.HasPrefix(cg.mod.TargetTriple, "x86_64")
}

// targetIsARM64 reports whether the module's target triple is an ARM64 target.
func (cg *CodeGen) targetIsARM64() bool {
	return strings.HasPrefix(cg.mod.TargetTriple, "arm64") ||
		strings.HasPrefix(cg.mod.TargetTriple, "aarch64")
}

// newModuleWithTriple creates a new LLVM IR module pre-populated with the
// target triple that clang will actually use, preventing the
// "overriding the module target triple" warning.
//
// clang -dumpmachine (and llc --version) return the darwin-style triple
// (e.g. arm64-apple-darwin25.1.0) but clang normalizes it to the macosx-style
// triple (e.g. arm64-apple-macosx26.0.0) when compiling LLVM IR.  Setting a
// darwin-style triple in the module therefore always triggers the override
// warning.  The only reliable way to get the exact string clang will use is to
// compile a trivial C snippet to LLVM IR and read the "target triple" line.
func newModuleWithTriple() *ir.Module {
	mod := ir.NewModule()
	mod.TargetTriple = detectTargetTriple()
	// Pin the source-filename to a stable string so opt + ld.lld
	// produce deterministic bitcode metadata regardless of the
	// random temp-file path tin writes the IR to. Without this, the
	// random-suffixed temp name leaks into the binary's symbol table
	// and breaks reproducible-build tests.
	mod.SourceFilename = "tin"

	return mod
}

// SetTargetTriple lets the caller (typically main, after consulting
// the disk-cached host-info record) hand us a precomputed triple so
// codegen doesn't itself spawn clang. The setter is one-shot per
// process; subsequent calls are no-ops, matching the sync.Once
// semantics of the original auto-probe path.
func SetTargetTriple(t string) {
	targetTripleOnce.Do(func() { targetTripleCache = t })
}

// detectTargetTriple returns the LLVM target triple this build should
// emit. Prefers the value supplied via SetTargetTriple (the disk-
// cached host-info path). Falls back to TIN_TARGET_TRIPLE, then to a
// live `clang -x c -` probe, then to a hardcoded GOOS/GOARCH map.
func detectTargetTriple() string {
	targetTripleOnce.Do(func() {
		// TIN_TARGET_TRIPLE env var overrides the triple (for cross-target tests).
		if override := os.Getenv("TIN_TARGET_TRIPLE"); override != "" {
			targetTripleCache = override

			return
		}
		// Compile an empty C TU to LLVM IR and extract the triple that
		// clang actually emits. This is the only way to get the
		// normalized macosx-style triple (rather than the darwin-style
		// one from -dumpmachine).
		if out, err := exec.Command("clang", "-x", "c", "-", "-S", "-emit-llvm", "-o", "-").
			Output(); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				if strings.HasPrefix(line, `target triple = "`) {
					triple := strings.TrimPrefix(line, `target triple = "`)

					triple = strings.TrimSuffix(triple, `"`)
					if triple != "" {
						targetTripleCache = triple

						return
					}
				}
			}
		}
		// Fallback by GOOS/GOARCH when clang is unavailable.
		switch runtime.GOOS + "/" + runtime.GOARCH {
		case "linux/amd64":
			targetTripleCache = "x86_64-pc-linux-gnu"
		case "linux/arm64":
			targetTripleCache = "aarch64-unknown-linux-gnu"
		case "darwin/amd64":
			targetTripleCache = "x86_64-apple-macosx11.0.0"
		case "darwin/arm64":
			targetTripleCache = "arm64-apple-macosx11.0.0"
		default:
			targetTripleCache = "x86_64-pc-linux-gnu"
		}
	})

	return targetTripleCache
}

var (
	targetTripleCache string
	targetTripleOnce  sync.Once
)

// New creates a new CodeGen instance.
func New(filename string) *CodeGen {
	cg := &CodeGen{
		filename:                 filename,
		mod:                      newModuleWithTriple(),
		structFields:             make(map[string][]string),
		structFieldTags:          make(map[string]map[string]string),
		structFieldTinTypes:      make(map[string][]ast.TypeExpr),
		genericStructsByArity:    make(map[string]map[int]*ast.StructDecl),
		genericStructTmplFiles:   make(map[string]string),
		traitMethodOrder:         make(map[string][]string),
		traitVtableGlobals:       make(map[string]*ir.Global),
		traitBorrowVtableGlobals: make(map[string]*ir.Global),
		traitDataReleaseThunks:   make(map[string]*ir.Func),
		wildcardMonos:            make(map[string]*ir.Func),
		traitInstKeys:            make(map[string]string),
		traitAsyncMethodNames:    make(map[string][]string),
		traitBareToQualInstKey:   make(map[string]string),
		implicitConvFns:          make(map[string][]implicitConvEntry),
		coerceConvFns:            make(map[string][]coerceConvEntry),
		structVtableOrder:        make(map[string][]string),
		enumValues:               make(map[string]int64),
		genericTypeAliases:       make(map[string]*ast.TypeDecl),
		opTraitImpls:             make(map[string][]opTraitImplEntry),
		exports:                  make(map[string]string),
		importedPkgs:             make(map[string]bool),
		loadedSrcPaths:           make(map[string]bool),
		constrainedFuncs:         make(map[string]*ast.FuncDecl),
		genericFuncs:             make(map[string]*ast.FuncDecl),
		genericFuncOverloads:     make(map[string][]*ast.FuncDecl),
		genericFuncHomeScopes:    make(map[string]*scope),
		constrainedFuncInstances: make(map[string]*ir.Func),
		genericMethodTemplates:   make(map[string]*ast.FuncDecl),
		macros:                   make(map[string]*ast.MacroDecl),
		funcDecls:                make(map[string]*ast.FuncDecl),
		ctfeCache:                make(map[string]ctfeMemoEntry),
		topLevelVarPos:           make(map[string]ast.Pos),
		externTLSVars:            make(map[string]*ir.Global),
		structTypeIDs:            make(map[string]int32),
		fnTypeIDs:                make(map[string]int32),
		nextTypeID:               6, // 0-5 reserved for anyTag* primitives (fn=5)
		structImpls:              make(map[string][]string),
		deadStrippedMethods:      make(map[string]map[string][]string),
		structFieldLLVMTypes:     make(map[string][]irtypes.Type),
		traitChainedInits:        make(map[string][]*ir.Func),
		traitChainedDeinits:      make(map[string][]*ir.Func),
		atomCodes:                make(map[string]int32),
		atomCodeToName:           make(map[int32]string),
		unionTypeMembers:         make(map[string][]ast.TypeExpr),
		nativeUnionDecls:         make(map[string]*ast.UnionDecl),
		unionTypeIDs:             make(map[string]int32),
		dataTypeIDs:              make(map[string]int32),
		dataVariants:             make(map[string]map[string]*dataVariantInfo),
		dataVariantLookup:        make(map[string][]string),
		dataValueReleaseFns:      make(map[string]*ir.Func),
		dataValueRetainFns:       make(map[string]*ir.Func),
		coroCallable:             make(map[string]bool),
		coloredCallable:          make(map[string]bool),
		boxedFns:                 make(map[string]bool),
		fnParkingClass:           make(map[string]bool),
		knownParkingExterns: map[string]bool{
			// Channel ops: park on empty / full + closed.
			"tin_channel_recv_blocking":  true,
			"tin_channel_send_blocking":  true,
			"tin_channel_recv_park":      true,
			"_tin_channel_recv_blocking": true,
			"_tin_channel_send_blocking": true,
			"_tin_channel_recv_park":     true,
			// Timer / sleep: park until elapsed.
			"tin_sleep_ms_c": true,
			"_tin_sleep_ms":  true,
			// Async I/O: park on EAGAIN.
			"tin_async_read_c":  true,
			"tin_async_write_c": true,
			"_tin_async_read":   true,
			"_tin_async_write":  true,
			// Mutex / cond / fiber join: explicit park.
			"_tin_fmutex2_lock":      true,
			"_tin_fcond2_add_waiter": true,
			"_tin_fiber_sync_await":  true,
			"_tin_fiber_join":        true,
		},
		callGraph:                   make(map[string][]string),
		funcHeuristics:              make(map[string]*FuncHeuristicInfo),
		overloadedNames:             make(map[string]bool),
		overloads:                   make(map[string][]*overloadEntry),
		genericMethodsSetUp:         make(map[string]bool),
		funcReturnUnsigned:          make(map[string]bool),
		heapPromotingFns:            make(map[string]bool),
		fnReturnsHeapPromotedFields: make(map[string][]int),
		structWeakFields:            make(map[string]map[string]bool),
		structOwningRawPtrFields:    make(map[string]map[string]bool),
		fnReturnsOwningIface:        make(map[string]bool),
		structConstFields:           make(map[string]map[string]bool),
		cLayoutStructs:              make(map[string]bool),
		nativeStructTypes:           make(map[string]*irtypes.StructType),
		packedStructs:               make(map[string]bool),
		noCopyStructs:               make(map[string]bool),
		closedStructs:               make(map[string]bool),
		structDeclsByName:           make(map[string]*ast.StructDecl),
		structDeclFiles:             make(map[string]string),
		elemReleaseHelpers:          make(map[string]*ir.Func),
		elemRetainHelpers:           make(map[string]*ir.Func),
		heapBlockReleaseFns:         make(map[string]*ir.Func),
		structPtrReleaseFns:         make(map[string]*ir.Func),
		chainReleaseFns:             make(map[string]*ir.Func),
		diFiles:                     make(map[string]*metadata.DIFile),
		diTypeCache:                 make(map[string]metadata.Field),
	}
	atomType := irtypes.NewStruct(irtypes.I32)
	atomType.SetName("__atom")
	cg.atomType = atomType
	cg.mod.TypeDefs = append(cg.mod.TypeDefs, atomType)
	cg.initBuiltinTupleTemplates()

	// Default libs root: <execDir>/libs next to the tin binary.
	if ex, err := os.Executable(); err == nil {
		cg.libsRoots = []string{filepath.Join(filepath.Dir(ex), "libs")}
	}

	// Seed builtin type aliases that exist outside of any source package:
	// rune is a Unicode codepoint represented as i32; `for r rune in s`
	// triggers UTF-8 decoding in the for-in loop.  Setting it via
	// recordAliasType lands it in the registry so cg.aliasTypeFor finds
	// it from any call site.
	cg.recordAliasType(CanonKey("rune"), &ast.SimpleType{Name: "i32"})

	return cg
}

// SetStdlibOverride overrides the default stdlib/ search path.
// Used by the --stdlib CLI flag.
func (cg *CodeGen) SetStdlibOverride(path string) { cg.stdlibOverride = path }

// AddLibsRoot prepends an additional libs root directory to the search path.
// Used by the --lib-root CLI flag.
func (cg *CodeGen) AddLibsRoot(path string) {
	cg.libsRoots = append([]string{path}, cg.libsRoots...)
}

// stdlibBase returns the effective stdlib directory.
func (cg *CodeGen) stdlibBase() string {
	if cg.stdlibOverride != "" {
		return cg.stdlibOverride
	}

	if ex, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(ex), "stdlib")
	}

	return "stdlib"
}

// runtimeBuiltinBase returns the path to runtime/builtin/, the
// directory holding language-defined traits (tryable, awaitable,
// etc.) that are auto-loaded into every Tin program. Sibling of
// stdlib so a custom-stdlib build still gets the built-in traits.
func (cg *CodeGen) runtimeBuiltinBase() string {
	if ex, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(ex), "runtime", "builtin")
	}

	return filepath.Join("runtime", "builtin")
}

// initBuiltinTupleTemplates pre-populates the Tuple generic struct templates
// for arities 2-10. Fields are named alphabetically: a, b, c, ...
func (cg *CodeGen) initBuiltinTupleTemplates() {
	letters := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}

	cg.genericStructsByArity["Tuple"] = make(map[int]*ast.StructDecl)
	for arity := 2; arity <= 10; arity++ {
		typeParams := make([]string, arity)
		copy(typeParams, letters[:arity])

		fields := make([]ast.StructField, arity)
		for i, name := range typeParams {
			fields[i] = ast.StructField{Name: name, Type: &ast.SimpleType{Name: name}}
		}

		cg.genericStructsByArity["Tuple"][arity] = &ast.StructDecl{
			Name:       "Tuple",
			TypeParams: typeParams,
			Fields:     fields,
		}
	}
}

// registerBuiltinTraits pre-populates cg.traits with synthetic declarations for
// built-in special traits (iter[t]) so structs can implement them without an
// explicit "trait iter[t] = ..." declaration in the source file.
func (cg *CodeGen) registerBuiltinTraits() {
	if cg.traitFor(CanonKey("iter")) != nil {
		return // already declared by user
	}
	// iter[t]: fn len(this iter[t]) i64 = virtual
	//          fn get(this iter[t], i i64) t = virtual
	selfType := &ast.GenericType{Name: "iter", TypeParams: []ast.TypeExpr{&ast.SimpleType{Name: "t"}}}
	lenMethod := &ast.FuncDecl{
		Name:      "len",
		IsVirtual: true,
		Params:    []ast.Param{{Name: "this", Type: selfType}},
		RetType:   &ast.SimpleType{Name: "i64"},
	}
	getMethod := &ast.FuncDecl{
		Name:      "get",
		IsVirtual: true,
		Params: []ast.Param{
			{Name: "this", Type: selfType},
			{Name: "i", Type: &ast.SimpleType{Name: "i64"}},
		},
		RetType: &ast.SimpleType{Name: "t"},
	}
	iterTrait := &ast.TraitDecl{
		Name:       "iter",
		TypeParams: []string{"t"},
		Methods:    []*ast.FuncDecl{lenMethod, getMethod},
	}
	cg.recordTrait(CanonKey("iter"), iterTrait)
}

// registerBuiltinOpTraits pre-populates cg.traits with synthetic alias-form
// declarations for the built-in operator traits. Each is a single-method
// `as fn` trait whose method name equals the trait name.
//
// User code implements an operator trait with `fn ::<trait>(this T, ...) ret`.
// genBinExpr / genUnaryExpr then dispatch operator expressions through the
// usual trait-impl lookup (traitImplKey).
func (cg *CodeGen) registerBuiltinOpTraits() {
	mk := func(name string, typeParams []string, params []ast.TypeExpr, ret ast.TypeExpr) {
		if cg.traitFor(CanonKey(name)) != nil {
			return // user (or earlier pass) already declared
		}

		td := &ast.TraitDecl{
			Name:       name,
			TypeParams: typeParams,
			IsAlias:    true,
			AliasType: &ast.FuncType{
				Params:  params,
				RetType: ret,
			},
		}
		cg.recordTrait(CanonKey(name), td)
	}

	t := func(n string) ast.TypeExpr { return &ast.SimpleType{Name: n} }

	// Binary arithmetic: trait <op>[rhs, ret] as fn(rhs) ret
	for _, n := range []string{"add", "sub", "mul", "div", "mod", "concat"} {
		mk(n, []string{"rhs", "ret"}, []ast.TypeExpr{t("rhs")}, t("ret"))
	}

	// Unary: trait <op>[ret] as fn() ret
	for _, n := range []string{"neg", "pos", "not"} {
		mk(n, []string{"ret"}, nil, t("ret"))
	}

	// Comparison: comp[rhs] as fn(rhs) bool
	mk("comp", []string{"rhs"}, []ast.TypeExpr{t("rhs")}, t("bool"))

	// Ordering: ord[rhs] as fn(rhs) i64  (negative/zero/positive)
	mk("ord", []string{"rhs"}, []ast.TypeExpr{t("rhs")}, t("i64"))

	// Index: index[key, ret] as fn(key) ret
	mk("index", []string{"key", "ret"}, []ast.TypeExpr{t("key")}, t("ret"))

	// Index-assign: index_set[key, val] as fn(key, val)
	mk("index_set", []string{"key", "val"}, []ast.TypeExpr{t("key"), t("val")}, nil)
}

// LinkLibs returns the list of libraries that source-level directives
// requested to link against (e.g. from `use extern` lib entries).
// The caller should pass these as -l<lib> flags to the linker.
func (cg *CodeGen) LinkLibs() []string { return cg.linkLibs }

// PackageSrcPaths returns the paths of all .tin source files that were loaded
// as packages during codegen. The caller can scan these for //! directives
// (e.g. //!+file.c) to collect C sources that need to be compiled alongside.
func (cg *CodeGen) PackageSrcPaths() []string { return cg.pkgSrcPaths }

// ReplGlobal records a top-level variable promoted from a `let` binding in
// REPL mode. The session uses this to declare the variable as a `var` in
// subsequent cells so the type checker can resolve cross-cell references.
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

// Generate translates the AST program into an LLVM IR module.
func (cg *CodeGen) Generate(prog *ast.Program) (*ir.Module, error) {
	// Initialize global scope.
	cg.curScope = newScope(nil)
	cg.moduleScope = cg.curScope

	// Register built-in special traits so structs can implement them without
	// an explicit trait declaration in source.
	cg.registerBuiltinTraits()
	cg.registerBuiltinOpTraits()

	// Duplicate-declaration pass: reject same-scope re-declarations before
	// any IR is emitted.  Shadowing (same name in a nested scope) is allowed.
	cg.progress("check declarations")

	if err := checkDuplicateDecls(prog.Stmts); err != nil {
		return nil, fmt.Errorf("%s:%w", cg.filename, err)
	}

	// Reject `pkg::Name` references when the file only imported `pkg`
	// selectively (`use { Name } from pkg`).  Selective imports bring
	// the named symbols into scope but do NOT register `pkg` as a
	// package alias; reaching for the qualified form should be a hard
	// error so users can't accidentally rely on a namespace they did
	// not opt into.
	if err := checkSelectiveImportQualifiers(prog.Stmts); err != nil {
		return nil, fmt.Errorf("%s:%w", cg.filename, err)
	}

	// Stacktrace usage detection: scan for `stacktrace(` calls before any
	// fn is emitted so funcs.go knows whether to keep Tin user fns at
	// internal linkage (default) or promote them to external (so dladdr
	// can resolve them after `-rdynamic` exports them to the dynsym).
	// Empirical check: STB_LOCAL symbols never reach the dynamic symbol
	// table regardless of `-rdynamic`, so promotion is mandatory when
	// stacktrace is reachable. Doing the scan here, after dup-decl checks
	// but before any other pass, keeps every later codegen path observing
	// a single consistent value of cg.stacktraceUsed.
	//
	// Must precede initDebugInfo: when stacktrace is reachable we flip
	// debugMode on so DWARF line tables get emitted into the IR (clang's
	// `-gline-tables-only` flag then preserves only `.debug_line`, which
	// libdwfl reads at runtime to map IPs to "file:line:col"). Without
	// this flip, the runtime resolver would always fall through to the
	// dladdr "<symbol>+<offset>" form for Tin user code.
	cg.detectStacktraceUsage(prog.Stmts)

	// pclntabUsed mirrors stacktraceUsed for now. It controls the
	// per-instruction line/col side-map (cg.instLineCol) that the
	// pclntab post-pass reads to build per-fn PC tables. Unlike
	// debugMode, enabling this does NOT pull in DWARF emission -
	// release builds get pclntab WITHOUT bloating the binary with
	// .debug_info / .debug_line / .debug_str sections that nothing
	// reads. -g (debugMode) still emits full DWARF for lldb / gdb.
	cg.pclntabUsed = cg.stacktraceUsed

	// Initialize DWARF debug metadata only when -g is active. pclntab
	// captures source positions through cg.instLineCol instead, so the
	// runtime resolver works without a DICompileUnit graph in the IR.
	if cg.debugMode {
		cg.initDebugInfo()
	}

	// Zero pass: collect exports and constrained generic function templates
	// before compiling anything.
	cg.progress("collect exports")

	for _, node := range prog.Stmts {
		if exp, ok := node.(*ast.ExportDecl); ok && exp.AsName != "" {
			for _, name := range exp.Names {
				cg.exports[name] = exp.AsName
			}
		}

		if fd, ok := node.(*ast.FuncDecl); ok && len(fd.Constraints) > 0 {
			cg.constrainedFuncs[fd.Name] = fd
		}

		if fd, ok := node.(*ast.FuncDecl); ok && len(fd.TypeParams) > 0 {
			cg.genericFuncs[fd.Name] = fd
			cg.genericFuncOverloads[fd.Name] = appendGenericFuncOverload(cg.genericFuncOverloads[fd.Name], fd)
		}
	}

	// First pass: register struct / enum / type declarations so forward refs work.
	// Collect concrete struct decls for the cycle check below.
	cg.progress("register types")

	var concreteStructDecls []*ast.StructDecl

	for _, node := range prog.Stmts {
		if err := cg.preregister(node); err != nil {
			return nil, err
		}

		if sd, ok := node.(*ast.StructDecl); ok && len(sd.TypeParams) == 0 {
			concreteStructDecls = append(concreteStructDecls, sd)
		}
	}

	// Validate struct reference cycles: every cycle must have at least one
	// weak edge and at least one strong edge.
	cg.progress("check struct cycles")

	if err := cg.checkStructCycles(concreteStructDecls); err != nil {
		return nil, err
	}

	// Validate complex (block-body) macros: side-effect check.
	// Recursive macros are allowed - the 5-second timeout handles runaway recursion.
	cg.progress("validate macros")

	for _, m := range cg.macros {
		if isMacroComplex(m) {
			if err := checkMacroSideEffects(m); err != nil {
				return nil, err
			}
		}
	}

	// Pre-pass 1.5: collect C extern symbol names BEFORE predeclaring Tin user
	// functions. This allows predeclareFuncAs to detect collisions and mangle
	// Tin wrapper names (e.g. `fn printf(...)` -> IR `@_tin__printf`) to avoid
	// redefinition conflicts with C externs declared in the same source file.
	if cg.externIRNames == nil {
		cg.externIRNames = map[string]bool{}
	}

	for _, node := range prog.Stmts {
		if fd, ok := node.(*ast.FuncDecl); ok && fd.IsExtern != "" {
			cg.externIRNames[fd.IsExtern] = true
		}
	}

	// Pre-pass 1.9: load all imported packages BEFORE registering top-level vars
	// and predeclaring function signatures. This ensures struct types (e.g.
	// AtomicI64 from "use sync") are registered before preregisterTopLevelVar
	// tries to resolve types like sync::AtomicI64, and before predeclareFunc
	// tries to resolve parameter types like sync::Channel[i64].
	cg.progress("load packages")

	for _, node := range prog.Stmts {
		if ud, ok := node.(*ast.UseDecl); ok && !ud.IsExtern {
			if err := cg.genUseDecl(ud); err != nil {
				return nil, err
			}
		}
	}

	// Pre-pass 1.8: detect overloaded function/method base names so that
	// predeclareFunc and predeclareMethod can mangle their IR names.
	for name, flag := range scanOverloadedNames(prog.Stmts) {
		cg.overloadedNames[name] = flag
	}

	// Pre-pass 1.85: emit struct layouts for the entry program BEFORE
	// predeclareFunc starts walking signatures.  A `fn make() Result[Foo, ...]`
	// signature triggers Result monomorphization via tinTypeToLLVM; if
	// Foo's struct hasn't had its Fields populated yet, the ADT's
	// payload buffer is sized to the smaller variant only and writes
	// of the larger struct payload spill over the buffer.  This
	// hoist mirrors the pkg-import path in loadPackageFromSource
	// (Pass 1.5a runs before its Pass 2 predeclares).
	for _, node := range prog.Stmts {
		if sd, ok := node.(*ast.StructDecl); ok && len(sd.TypeParams) == 0 {
			if err := cg.genStructLayout(sd); err != nil {
				return nil, err
			}
		}
	}

	// Pre-pass 1.9: synthesize `fn main() Result[Unit, errors::Err]`
	// when top-level imperative statements use `try` but no explicit
	// `fn main` exists.  Without this, `let v = try foo()` at script
	// scope tries to propagate Err through the implicit main's i32
	// return and fails to typecheck.  Wrapping the script in a Result-
	// returning main lets try desugar against a real Result-shaped
	// return frame; the C-main wrapper unpacks the Result into the
	// process exit code via tryEmitResultMainReturn.
	prog.Stmts = cg.synthesizeImplicitMainForTry(prog.Stmts)

	// Second pass: pre-declare all functions (signatures only) so forward calls work.
	cg.progress("predeclare functions")

	for _, node := range prog.Stmts {
		if fd, ok := node.(*ast.FuncDecl); ok {
			if err := cg.predeclareFunc(fd); err != nil {
				return nil, err
			}
		}

		if sd, ok := node.(*ast.StructDecl); ok {
			// Skip method predeclaration for generic struct templates - methods
			// will be compiled on demand when the concrete type is instantiated.
			if len(sd.TypeParams) > 0 {
				continue
			}
			// Propagate struct-level scoped tags (#pure@fn, etc.) onto methods
			// BEFORE they are registered in funcDecls. The later #pure /
			// #no_recurse check iterates funcDecls and must see the expanded
			// tag set.
			if err := cg.propagateStructScopedTags(sd); err != nil {
				return nil, err
			}

			aug := cg.augmentStructFromTraits(sd)
			for _, m := range aug.Methods {
				if err := cg.predeclareMethod(aug.Name, m); err != nil {
					return nil, err
				}
			}
		}
	}

	// Validate trait-impl completeness: every struct that declares (T1, T2, ...)
	// must provide qualified impls for each virtual method of each listed trait.
	// Default-bodied methods (e.g. labeled.label) remain optional. Reports all
	// missing impls per struct in one error so users can fix them in one pass.
	if err := cg.checkAllTraitImplsComplete(prog.Stmts); err != nil {
		return nil, err
	}

	// Collect entry-program top-level var bare names (pkg-imported vars are
	// already registered via Pre-pass 1.9). The pure-check below uses this set
	// to reject reads/writes of mutable globals from #pure bodies.
	if cg.topLevelVarBareNames == nil {
		cg.topLevelVarBareNames = map[string]bool{}
	}

	if cg.topLevelConstNames == nil {
		cg.topLevelConstNames = map[string]bool{}
	}

	for _, node := range prog.Stmts {
		if tv, ok := node.(*ast.TopLevelVar); ok {
			cg.topLevelVarBareNames[tv.Name] = true
			if tv.IsConst {
				cg.topLevelConstNames[tv.Name] = true
			}
		}
	}

	// Validate #pure functions: transitive side-effect check.
	// Validate #no_recurse functions: transitive call-graph cycle check.
	// Both run after predeclaration so all function signatures and tags are known.
	if err := cg.checkAllPureFuncs(); err != nil {
		return nil, err
	}

	if err := cg.checkAllNoRecurseFuncs(); err != nil {
		return nil, err
	}

	if err := cg.checkAllInteropFuncs(prog.Stmts); err != nil {
		return nil, err
	}

	cg.checkAllUnused(prog)

	// Run Andersen FIRST so cg.andersenPts is populated before the
	// dataflow pass, which consults the points-to map for the
	// -Wunchecked-returned-nil pedantic check.  Andersen has no
	// dataflow dependency, so the swap is a pure ordering change.
	cg.runAndersen(prog)

	cg.runDataflow(prog)

	cg.runAstChecks(prog)

	// Build call graph and run color propagation for the #async / coro system.
	cg.progress("build call graph")

	for _, node := range prog.Stmts {
		if fd, ok := node.(*ast.FuncDecl); ok && fd.Body != nil {
			cg.buildCallGraphEntry(fd.Name, fd.Body)
		}

		if sd, ok := node.(*ast.StructDecl); ok {
			for _, m := range sd.Methods {
				key := methodScopeName(sd.Name, m)
				cg.buildCallGraphEntry(key, m.Body)
			}
		}
	}

	cg.collectBoxedFns(prog)
	cg.colorCallGraph()
	cg.checkSyncWaitInCoroCallable(prog)
	cg.computeAutoYieldHeuristics(prog)

	// Pre-declare $coro and $colored variants for all colored functions
	// so that mutual references across coro/colored bodies resolve
	// correctly during body emission.
	for _, node := range prog.Stmts {
		if fd, ok := node.(*ast.FuncDecl); ok {
			// fn main() is renamed to _tin_user_main at IR level; predeclare
			// the $coro stub under that IR name so genFuncDeclAs can find it.
			coroKey := fd.Name
			if fd.Name == "main" && !fd.IsStatic {
				coroKey = "_tin_user_main"
			}

			if cg.coroCallable[coroKey] {
				if err := cg.predeclareCoroVariant(fd, coroKey, false); err != nil {
					return nil, err
				}
			}

			if cg.coloredCallable[coroKey] {
				if err := cg.predeclareColoredVariant(fd, coroKey, false); err != nil {
					return nil, err
				}
			}
		}
	}
	// Predeclare $colored variants for every fn in coloredCallable that
	// wasn't already covered by the prog.Stmts loop above.  In
	// particular: struct methods (registered into cg.funcDecls under
	// their mangled scope name during the struct-decl pass) and
	// package-loaded fns.  Without this pass, a coro caller emitted
	// BEFORE its colored callee would not see `<callee>$colored` in
	// scope and resolveColoredCallee would silently fall back to the
	// plain sync callee -- correct semantics but cooperation is lost
	// on every forward-ref'd colored call site.
	//
	// Iterate in sorted name order: Go map iteration is intentionally
	// non-deterministic, and each iteration calls NewFunc on cg.mod, so
	// the declarations land in whatever order the map happened to walk
	// this run.  That broke TestIRDeterminism (which diffs two runs of
	// `tin ir` and demands byte-identical output).
	colNames := make([]string, 0, len(cg.funcDecls))
	for name := range cg.funcDecls {
		colNames = append(colNames, name)
	}

	sort.Strings(colNames)

	for _, name := range colNames {
		decl := cg.funcDecls[name]
		if !cg.coloredCallable[name] {
			continue
		}

		if decl == nil || decl.IsExtern != "" || decl.Body == nil {
			continue
		}

		if hasTag(decl.Tags, "no_autoyield") {
			continue
		}

		if err := cg.predeclareColoredVariant(decl, name, false); err != nil {
			return nil, err
		}
	}

	// Pre-pass 2.5: scan extern declarations for *StructName pointer types.
	// Structs used as *S in extern signatures must use C-compatible layout
	// (no type_id prefix) so raw pointers can round-trip to/from C.
	cg.scanExternPtrStructs(prog.Stmts)

	// Pre-pass 2.8: register extern functions that use only built-in types.
	// Struct method bodies may call module-level externs before those externs
	// are processed by the Third pass (e.g. AtomicI64.make -> _tin_atomic_new_i64).
	// predeclareFuncAs skips externs, so without this pass they are undefined
	// when Pre-pass 3 compiles method bodies.
	// Only externs with all-primitive types are processed here; externs that
	// reference struct/enum types are skipped (struct types aren't registered
	// until Pre-pass 3, so processing them now would panic).
	for _, node := range prog.Stmts {
		if fd, ok := node.(*ast.FuncDecl); ok && fd.IsExtern != "" && externHasPrimitiveTypes(fd) {
			if err := cg.genFuncDecl(fd); err != nil {
				return nil, err
			}
		}
	}

	// Pre-pass 3: generate struct/enum/type/union declarations before anything
	// else so that structFieldLLVMTypes is fully populated.  This is needed
	// because use-extern declarations reference struct types for C ABI conversion
	// and may appear before the struct definition in source order.
	cg.progress("generate type declarations")

	// Phase A: struct field layouts (no methods yet); plus enum/type/union
	// declarations whose field types may reference structs.
	for _, node := range prog.Stmts {
		switch n := node.(type) {
		case *ast.StructDecl:
			if err := cg.genStructLayout(n); err != nil {
				return nil, err
			}
		case *ast.EnumDecl:
			if err := cg.genEnumDecl(n); err != nil {
				return nil, err
			}
		case *ast.TypeDecl:
			if err := cg.genTypeDecl(n); err != nil {
				return nil, err
			}
		case *ast.UnionDecl:
			if err := cg.genUnionDecl(n); err != nil {
				return nil, err
			}
		}
	}

	// Phase B: ADT layouts now that every struct field type is known, so
	// generic ADTs like Result[LocalStruct, Err] get the correct payload
	// size rather than a placeholder [1 x i8].
	for _, node := range prog.Stmts {
		if n, ok := node.(*ast.DataDecl); ok {
			if err := cg.genDataDecl(n); err != nil {
				return nil, err
			}
		}
	}

	// Pass 2.5: register top-level var declarations as LLVM globals. Runs
	// AFTER pass 2 (function predeclaration) so initializer fold can call
	// pure functions via funcDecls (e.g. `var x i64 = pure_fn(7) + 1`), and
	// BEFORE struct method bodies are generated so methods can reference
	// module-scoped vars by bare name.
	cg.progress("register globals")

	for _, node := range prog.Stmts {
		if tv, ok := node.(*ast.TopLevelVar); ok {
			if err := cg.preregisterTopLevelVar(tv); err != nil {
				return nil, err
			}
			// Record the declaration position so `sourcepos(my_top_var)`
			// can resolve back to the originating let/var/const line.
			cg.topLevelVarPos[tv.Name] = tv.Pos()
		}
	}

	// Phase C: struct method bodies, trait chain shims, and vtables.
	for _, node := range prog.Stmts {
		if n, ok := node.(*ast.StructDecl); ok {
			if err := cg.genStructMethods(n); err != nil {
				return nil, err
			}
		}
	}

	// Third pass: generate full function bodies and other declarations.
	cg.progress("generate code")

	var topStmts []ast.Node

	for _, node := range prog.Stmts {
		switch n := node.(type) {
		case *ast.FuncDecl:
			if err := cg.genFuncDecl(n); err != nil {
				return nil, err
			}
		case *ast.StructDecl:
			// Already processed in pre-pass 3.
		case *ast.EnumDecl:
			// Already processed in pre-pass 3.
		case *ast.TypeDecl:
			// Already processed in pre-pass 3.
		case *ast.UseDecl:
			if err := cg.genUseDecl(n); err != nil {
				return nil, err
			}
		case *ast.ExportDecl:
			// Already handled in zero pass; ExportDecl itself emits no IR.
		case *ast.TraitDecl:
			// Registered in preregister; no IR to emit.
		case *ast.MacroDecl:
			// Registered in preregister; no IR to emit.
		case *ast.UnionDecl:
			// Already processed in pre-pass 3.
		case *ast.DataDecl:
			// Already processed in pre-pass 3.
		case *ast.TestDecl:
			if cg.testMode {
				cg.testDecls = append(cg.testDecls, n)
			}
			// In normal mode, test blocks are silently ignored.
		case *ast.TopLevelVar:
			// Already registered as an LLVM global in pre-pass 1.7.
			// If the initializer is a runtime expression, it was appended to
			// cg.topLevelVarInits and will be emitted at the top of main().
		default:
			topStmts = append(topStmts, node)
		}
	}

	// Emit C-callable wrappers for #interop functions. Done after the
	// third pass so all internal entry points exist as IR functions
	// the wrapper can reference.
	if err := cg.emitInteropWrappers(prog.Stmts); err != nil {
		return nil, err
	}

	// Emit a parallel #interop-style shim for every wrappable #pure function
	// so the per-fn .so cache (Phase C2) has a single uniform dispatch
	// surface for cgo. The shim shares emitInteropWrapperFor's marshal
	// logic - string/slice/bool widening all go through the same helpers
	// the user-tagged #interop pipeline uses. Shim symbol is
	// `__tin_pure_shim_<fn_name>` so it never collides with the function
	// itself; in the main binary the shim has internal linkage and clang
	// DCEs it; the cache slicer promotes it to external for dlsym.
	if err := cg.emitPureFnCtfeShims(); err != nil {
		return nil, err
	}

	if cg.emitHeaderPath != "" {
		if err := cg.writeInteropHeader(prog.Stmts); err != nil {
			return nil, err
		}
	}

	// In test mode, generate test functions and a test-runner main.
	// Top-level statements that would form the implicit main are intentionally
	// not executed - only test blocks run.
	if cg.testMode && len(cg.testDecls) > 0 {
		if err := cg.genTestRunner(); err != nil {
			return nil, err
		}

		cg.emitAtomTable()
		cg.applyStacktracePostPass()
		cg.finalizeImplSection()
		// extractMonoModules MUST run before applyPclntabPostPass:
		// pclntab emits blockaddress(@fn, %bb) constants and lld
		// rejects them when @fn is only a declare. Moving mono fns
		// first lets pclntab route the pcs entry into the same mono
		// module where the fn definition now lives.
		cg.extractMonoModules()
		cg.applyPclntabPostPass()
		cg.emitLlvmUsedRoots()
		cg.finalizePerPkgModules()

		return cg.mod, nil
	}

	// In REPL mode the cell function is the only entry point; skip main().
	// Skip mono extraction: REPL compiles cg.mod alone (no separate mono
	// .o cache), so monomorphized fn bodies must stay in cg.mod or
	// dlopen will see only `declare`s for them.  For the same reason we
	// also fold per-pkg modules back into cg.mod here -- the non-REPL
	// build compiles each pkg to its own .o and links them together,
	// but the REPL has no separate compile/link pass; everything has
	// to live in the single cg.mod that becomes the cell .so.
	if cg.replMode {
		cg.emitAtomTable()
		cg.applyStacktracePostPass()
		cg.finalizeImplSection()
		cg.applyPclntabPostPass()
		// Merge BEFORE emitLlvmUsedRoots: emitting first would create a
		// per-pkg @llvm.used global that the merge then drops by name
		// dedup, silently losing every impl-section pin entry.  Merge
		// rekeys cg.llvmUsedRoots/Funcs from pkgMod -> cg.mod so the
		// subsequent emit produces a single combined @llvm.used.
		cg.mergePkgModsIntoMain()
		cg.emitLlvmUsedRoots()
		cg.finalizePerPkgModules()

		return cg.mod, nil
	}

	// If there are top-level statements, wrap them in main().
	if len(topStmts) > 0 {
		// Top-level imperative statements form an implicit main. If the
		// user also wrote `fn main()`, both would emit an `i32 @main`
		// wrapper -- the implicit one wins and the user main is never
		// called. Error out so the user picks one model.
		// (Top-level `const` and `var` are TU-level decls and don't
		// reach topStmts, so this only fires for actual statements.)
		if cg.userMainDecl != nil {
			return nil, cg.nodeErr(topStmts[0],
				"top-level statements cannot coexist with an explicit fn main(); "+
					"move the statement inside main, or remove fn main() to use the implicit-main form")
		}

		// Check if main is already defined.
		hasmain := false

		for _, f := range cg.allFuncs() {
			if f.Name() == "_tin_c_main" {
				hasmain = true

				break
			}
		}

		if !hasmain {
			if err := cg.genImplicitMain(topStmts); err != nil {
				return nil, err
			}
		}
	}

	// If the user declared a void `fn main()`, it was compiled as
	// `_tin_user_main`.  Generate a proper `i32 @main()` wrapper that
	// calls it and returns 0 so the process exits cleanly.
	var userMainFn *ir.Func

	for _, f := range cg.allFuncs() {
		if f.Name() == "_tin_user_main" {
			userMainFn = f

			break
		}
	}

	if userMainFn != nil {
		// Only add the wrapper if there is no `i32 @main` already.
		hasMain := false

		for _, f := range cg.allFuncs() {
			if f.Name() == "_tin_c_main" {
				hasMain = true

				break
			}
		}

		if !hasMain {
			// Check whether the user wrote fn{#async} main() - if so we have a
			// $coro ramp and main should run as the first fiber.
			var userMainCoroFn *ir.Func

			for _, f := range cg.allFuncs() {
				if f.Name() == "_tin_user_main$coro" {
					userMainCoroFn = f

					break
				}
			}

			// If the user's main takes a [string] parameter, expose argc/argv.
			wantsArgs := mainTakesStringArgs(cg.userMainDecl)

			wf := cg.newCMainWrapper(wantsArgs)

			wb := wf.NewBlock("entry")

			// Save context so emitTopLevelVarInits can generate expressions.
			prevFn := cg.curFn
			prevScope := cg.curScope
			cg.curFn = wf
			cg.curScope = newScope(cg.curScope)

			// Attach a DISubprogram so `br set -n main` in lldb/gdb lands on
			// the wrapper and shows source. Use line 1 of the primary source
			// file as the scope line; the real user main (compiled as
			// _tin_user_main) carries its own DISubprogram with the exact line.
			prevDbgScope := cg.diCurrentScope
			cg.emitDbgSubprogramForSynthetic(wf, "main", 1)

			defer func() { cg.diCurrentScope = prevDbgScope }()

			// Emit fiber init + io init when the program uses fiber features.
			wb = cg.emitFiberMainWrap(wb)

			// Register the deinit dispatcher with libc atexit BEFORE
			// running user code. atexit guarantees the deinits fire on
			// every clean exit path (return-from-main, libc exit(N),
			// any fn call to std::os::exit) - not only the
			// fall-through-from-main path the inline emit covers.
			wb = cg.emitDeinitAllAtexit(wb)

			// Register per-type-id any-release helpers so that any-boxed
			// structs run their deinit on scope exit instead of just
			// freeing the heap block.
			wb = cg.emitAnyDispatchRegistrations(wb)

			// Emit runtime initializers for top-level var declarations before
			// any fiber runs so that globals are valid from the start.
			var err error

			wb, err = cg.emitTopLevelVarInits(wb)
			if err != nil {
				return nil, err
			}

			cg.emitPkgInitFns(wb)

			// Build the [string] args value from argc/argv if needed.
			var argsSliceVal value.Value

			if wantsArgs {
				strArrType := irtypes.NewStruct(irtypes.NewPointer(stringFatPtrType()), irtypes.I64)
				argvFn := cg.ensureExternDecl("_tin_argv_to_slice", strArrType, []*ir.Param{
					ir.NewParam("argc", irtypes.I32),
					ir.NewParam("argv", irtypes.NewPointer(irtypes.I8Ptr)),
				}, false)
				argsSliceVal = wb.NewCall(argvFn, wf.Params[0], wf.Params[1])
			}

			if userMainCoroFn != nil {
				// fn{#async} main(): spawn as the first fiber and block the OS
				// main thread until it completes, then drain remaining fibers.
				// _tin_fiber_run() sends a shutdown signal immediately - if we
				// called it before main's fiber finished, workers would exit too
				// early.  _tin_fiber_sync_await blocks without touching the run
				// queue, so workers continue normally until main is done.
				cg.ensureFiberRuntime()
				syncAwaitFn := cg.ensureExternDecl("_tin_fiber_sync_await", irtypes.Void,
					[]*ir.Param{ir.NewParam("pid", irtypes.I64)}, false)

				var coroArgs []value.Value

				for i, p := range userMainCoroFn.Params {
					if wantsArgs && i == 0 {
						coroArgs = append(coroArgs, argsSliceVal)
					} else {
						coroArgs = append(coroArgs, constant.NewZeroInitializer(p.Type()))
					}
				}

				coroHdl := wb.NewCall(userMainCoroFn, coroArgs...)
				mainPid := wb.NewCall(cg.fiberSpawnJoinableFn, coroHdl)
				wb.NewCall(syncAwaitFn, mainPid)
				cg.emitFiberMainEnd(wb)
				// Deinits run via atexit(_tin_deinit_all); no inline
				// emit needed here.
				wb.NewRet(constant.NewInt(irtypes.I32, 0))
			} else {
				// fn main(): call synchronously (existing behavior).
				var callArgs []value.Value

				for i, p := range userMainFn.Params {
					if wantsArgs && i == 0 {
						callArgs = append(callArgs, argsSliceVal)
					} else {
						callArgs = append(callArgs, constant.NewZeroInitializer(p.Type()))
					}
				}

				retIsVoid := userMainFn.Sig.RetType.Equal(irtypes.Void)
				if retIsVoid {
					wb.NewCall(userMainFn, callArgs...)
					// Deinits run via atexit(_tin_deinit_all).
					cg.emitFiberMainEnd(wb)
					wb.NewRet(constant.NewInt(irtypes.I32, 0))
				} else {
					ret := wb.NewCall(userMainFn, callArgs...)
					// Deinits run via atexit(_tin_deinit_all).
					cg.emitFiberMainEnd(wb)
					// fn main() Result[Unit, errors::Err] / Result[i64, errors::Err]:
					// unpack the Result.  Ok -> exit 0 (or the inner i64,
					// truncated to i32); Err -> print the message and exit 1.
					if cg.tryEmitResultMainReturn(wb, ret) {
						// terminator already emitted by the helper
					} else {
						// Coerce return value to i32 if needed.
						var retVal value.Value = ret
						if !ret.Type().Equal(irtypes.I32) {
							if ret.Type().Equal(irtypes.I64) {
								retVal = wb.NewTrunc(ret, irtypes.I32)
							} else {
								retVal = constant.NewInt(irtypes.I32, 0)
							}
						}

						wb.NewRet(retVal)
					}
				}
			}

			cg.ensureAllCallsHaveDbg(wf)

			cg.curFn = prevFn
			cg.curScope = prevScope
		}
	}

	// Emit the compile-time atom table and fill in atom helper function bodies.
	cg.emitAtomTable()

	// If no main function was generated (e.g. export-only module), emit a
	// trivial no-op main so the binary links successfully.
	hasMain := false

	for _, f := range cg.allFuncs() {
		if f.Name() == "_tin_c_main" {
			hasMain = true

			break
		}
	}

	if !hasMain && !programHasInteropFunc(prog.Stmts) {
		// No user main and no #interop functions: emit an empty main so
		// the linker has an entry point. When #interop functions exist
		// the program is being built as a library; skip the synthetic
		// main so the C consumer can provide its own.
		wf := cg.newCMainWrapper(false)
		wb := wf.NewBlock("entry")
		wb.NewRet(constant.NewInt(irtypes.I32, 0))
	}

	cg.debugDumpUnterminated()
	cg.applyStacktracePostPass()
	cg.finalizeImplSection()
	cg.applyPclntabPostPass()
	cg.emitLlvmUsedRoots()
	cg.finalizePerPkgModules()

	// -Wunwrapped-c-resource: walks every struct (incl. stdlib) for raw
	// C resource fields whose value crosses an extern boundary. Runs at
	// the very end so all struct decls are registered (genStructDecl
	// populates structDeclsByName lazily) and the call graph is built
	// (computeFnsTouchingExtern needs it for transitive propagation).
	cg.checkAllUnwrappedCResources(prog)

	// -Wunclosed-closeable: warns when a let-bound value whose type
	// implements io::Closeable leaves scope without a .close() call (or
	// transfer). Runs after structDeclsByName is populated so the trait
	// list per struct is complete.
	cg.checkAllUnclosedCloseables(prog)

	// Static analysis: warn on writes that reach a top-level `const`
	// through a pointer alias. Top-level consts live in read-only
	// storage so the write is undefined behavior.
	cg.checkAllWritesToTopLevelConst(prog)

	return cg.mod, nil
}

// synthesizeImplicitMainForTry walks stmts and, if any top-level
// imperative statement uses `try`, replaces those statements with a
// synthesized `fn main() Result[Unit, errors::Err]` whose body is the
// original statements plus a closing `return Ok(Unit{_unit: 0})`.
// The synthesis only fires when there is no explicit `fn main`
// already in stmts; with an explicit main, top-level try is a user
// error that the existing diagnostic handles.
func (cg *CodeGen) synthesizeImplicitMainForTry(stmts []ast.Node) []ast.Node {
	hasExplicitMain := false

	for _, s := range stmts {
		if fd, ok := s.(*ast.FuncDecl); ok && fd.Name == "main" && !fd.IsStatic {
			hasExplicitMain = true

			break
		}
	}

	if hasExplicitMain {
		return stmts
	}

	bodyStmts := make([]ast.Node, 0, len(stmts))

	otherStmts := make([]ast.Node, 0, len(stmts))

	hasTry := false

	for _, s := range stmts {
		if isTopLevelDecl(s) {
			otherStmts = append(otherStmts, s)

			continue
		}

		bodyStmts = append(bodyStmts, s)

		if nodeContainsTry(s) {
			hasTry = true
		}
	}

	if !hasTry {
		return stmts
	}

	// `return Ok(0)` as the final body statement.  Using i64 instead of
	// sync::Unit avoids forcing the user's script to `use sync` just to
	// satisfy the synthetic Result wrapper.  The C-main wrapper accepts
	// Result[i64, errors::Err] equally well and exits with the inner i64
	// truncated to i32.
	okCall := &ast.CallExpr{
		Func: &ast.Identifier{Name: "Ok"},
		Args: []ast.Node{
			&ast.IntLit{Value: 0},
		},
	}

	bodyStmts = append(bodyStmts, &ast.ReturnStmt{Value: okCall})

	resultType := &ast.GenericType{
		Name: "Result",
		TypeParams: []ast.TypeExpr{
			&ast.SimpleType{Name: "i64"},
			&ast.SimpleType{Name: "errors::Err"},
		},
	}

	mainDecl := &ast.FuncDecl{
		Name:    "main",
		RetType: resultType,
		Body:    &ast.Block{Stmts: bodyStmts},
	}

	return append(otherStmts, mainDecl)
}

// isTopLevelDecl returns true for AST nodes that belong at the top
// level and MUST stay outside the synthesized main body (function /
// type / use / etc. declarations).  Everything else is imperative
// script-level code and gets absorbed into the synthesized main.
func isTopLevelDecl(n ast.Node) bool {
	switch n.(type) {
	case *ast.FuncDecl, *ast.StructDecl, *ast.EnumDecl, *ast.TypeDecl,
		*ast.UseDecl, *ast.ExportDecl, *ast.TraitDecl, *ast.MacroDecl,
		*ast.UnionDecl, *ast.DataDecl, *ast.TestDecl, *ast.TopLevelVar:
		return true
	}

	return false
}

// nodeContainsTry returns true when n or any of its descendants is a
// TryExpr.  Used by the implicit-main synthesizer to decide whether
// the script needs a Result-typed wrapper.
func nodeContainsTry(n ast.Node) bool {
	if n == nil {
		return false
	}

	if _, ok := n.(*ast.TryExpr); ok {
		return true
	}

	found := false

	walkAST(n, func(v ast.Node) {
		if _, ok := v.(*ast.TryExpr); ok {
			found = true
		}
	})

	return found
}

// tryEmitResultMainReturn unpacks ret as a Result[Unit | i64, errors::Err]
// when the user's `fn main()` is typed that way.  Ok exits 0 (or the
// inner i64 truncated to i32); Err calls the iface's `errors::Err::message`
// method, forwards the string to the `_tin_main_err_exit` C helper which
// prints "error: <msg>\n" to stderr and exits 1.  Returns true when the
// helper emitted the full unpack-and-return chain (caller must skip its
// own terminator).
func (cg *CodeGen) tryEmitResultMainReturn(block *ir.Block, ret value.Value) bool {
	retSt, ok := ret.Type().(*irtypes.StructType)
	if !ok {
		return false
	}

	name := retSt.Name()
	if !strings.HasPrefix(name, "Result__") {
		return false
	}

	variants, ok := cg.dataVariants[name]
	if !ok {
		return false
	}

	okVI, ok := variants["Ok"]
	if !ok {
		return false
	}

	errVI, ok := variants["Err"]
	if !ok {
		return false
	}
	// Ok payload must be Unit or i64 - any other shape means the caller
	// can't sensibly express the process exit code.  An Err payload that
	// looks like errors::Err but an Ok shape we don't handle (e.g.
	// `fn main() Result[i32, errors::Err]`) would silently swallow the
	// Err if we fell through to the generic non-Result path; refuse the
	// signature instead so the user picks i64 or Unit.
	okInnerTy, okShape := resultMainOkInnerType(okVI)

	errIsErrIface := resultMainErrIsErrIface(errVI)
	if !okShape && errIsErrIface {
		panic(fmt.Sprintf("fn main() return type %s is not supported as a process exit shape; use Result[Unit, errors::Err] or Result[i64, errors::Err]", name))
	}

	if !okShape {
		return false
	}
	// Err payload must be an errors::Err fat-ptr iface so we know how to
	// call .message() on it.
	if !errIsErrIface {
		return false
	}

	// Stash the returned Result so we can GEP into it.
	alloca := block.NewAlloca(retSt)
	block.NewStore(ret, alloca)

	tagGEP := block.NewGetElementPtr(retSt, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 1))
	tag := block.NewLoad(irtypes.I64, tagGEP)
	isOk := block.NewICmp(enum.IPredEQ, tag, constant.NewInt(irtypes.I64, okVI.Tag))

	parent := block.Parent
	okBlk := parent.NewBlock("main.result.ok")
	errBlk := parent.NewBlock("main.result.err")
	block.NewCondBr(isOk, okBlk, errBlk)

	// Ok branch.
	payloadGEP := okBlk.NewGetElementPtr(retSt, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))

	switch okInnerTy {
	case "Unit":
		okBlk.NewRet(constant.NewInt(irtypes.I32, 0))
	case "i64":
		payloadPtr := okBlk.NewBitCast(payloadGEP, irtypes.NewPointer(irtypes.I64))
		v := okBlk.NewLoad(irtypes.I64, payloadPtr)
		okBlk.NewRet(okBlk.NewTrunc(v, irtypes.I32))
	}

	// Err branch: extract iface fat-ptr, call vtable message slot, forward
	// the resulting TinString to _tin_main_err_exit.
	errPayloadGEP := errBlk.NewGetElementPtr(retSt, alloca,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, 2))

	// The Err variant's payload struct stores the errors::Err iface as
	// its field 0; reuse that struct type directly so we don't depend
	// on whether buildIfaceShape happened to register the type under
	// `cg.traitFatPtrTypes` yet.
	ifaceTy, ok := errVI.PayloadType.Fields[0].(*irtypes.StructType)
	if !ok {
		errBlk.NewRet(constant.NewInt(irtypes.I32, 1))

		return true
	}

	ifaceP := errBlk.NewBitCast(errPayloadGEP, irtypes.NewPointer(ifaceTy))
	ifaceVal := errBlk.NewLoad(ifaceTy, ifaceP)
	dataPtr := errBlk.NewExtractValue(ifaceVal, 0)
	vtPtr := errBlk.NewExtractValue(ifaceVal, 1)

	msgFn, ok := cg.findErrMessageSlot(vtPtr.Type())
	if !ok {
		errBlk.NewRet(constant.NewInt(irtypes.I32, 1))

		return true
	}
	// Load the message slot from the vtable.
	msgSlotPtr := errBlk.NewGetElementPtr(msgFn.vtType, vtPtr,
		constant.NewInt(irtypes.I32, 0), constant.NewInt(irtypes.I32, int64(msgFn.slotIdx)))
	msgFnVal := errBlk.NewLoad(msgFn.fnPtrType, msgSlotPtr)
	msgStr := errBlk.NewCall(msgFnVal, dataPtr)

	strPtr := errBlk.NewExtractValue(msgStr, 0)
	strLen := errBlk.NewExtractValue(msgStr, 1)
	errExitFn := cg.ensureExternDecl("_tin_main_err_exit", irtypes.Void, []*ir.Param{
		ir.NewParam("msg", irtypes.I8Ptr),
		ir.NewParam("len", irtypes.I64),
	}, false)
	errBlk.NewCall(errExitFn, strPtr, strLen)
	// _tin_main_err_exit calls exit(); marking this block unreachable
	// lets LLVM drop any code after it.
	errBlk.NewUnreachable()

	return true
}

// resultMainOkInnerType classifies the Ok payload of `Result[T, errors::Err]`
// when used as main's return type.  Returns ("Unit", true), ("i64", true),
// or ("", false) for any other shape (which falls back to the generic
// non-Result main return handling).
func resultMainOkInnerType(vi *dataVariantInfo) (string, bool) {
	if vi == nil || vi.PayloadType == nil || len(vi.PayloadType.Fields) != 1 {
		return "", false
	}

	ft := vi.PayloadType.Fields[0]
	if it, ok := ft.(*irtypes.IntType); ok && it.BitSize == 64 {
		return "i64", true
	}

	if st, ok := ft.(*irtypes.StructType); ok {
		name := st.Name()
		if name == "sync__Unit" || name == "Unit" {
			return "Unit", true
		}
	}

	return "", false
}

// resultMainErrIsErrIface returns true when the Err variant's payload
// is the `errors::Err` trait-object struct (fat ptr layout: {i8* data, vt* vtable}).
func resultMainErrIsErrIface(vi *dataVariantInfo) bool {
	if vi == nil || vi.PayloadType == nil || len(vi.PayloadType.Fields) != 1 {
		return false
	}

	st, ok := vi.PayloadType.Fields[0].(*irtypes.StructType)
	if !ok {
		return false
	}

	return st.Name() == "errors__Err_iface"
}

// errMessageSlot resolves the layout of `errors::Err::message` for the
// active errors::Err vtable type so the wrapper can call the right
// vtable slot at IR-emit time.
type errMessageSlot struct {
	vtType    *irtypes.StructType
	fnPtrType *irtypes.PointerType
	slotIdx   int
}

// findErrMessageSlot returns the vtable struct's `message` slot
// metadata so tryEmitResultMainReturn can GEP into the right offset
// and load the function pointer to call.  Walks cg.traitVtableFields
// to find the index of the `message` method.
func (cg *CodeGen) findErrMessageSlot(vtPtrTy irtypes.Type) (*errMessageSlot, bool) {
	pt, ok := vtPtrTy.(*irtypes.PointerType)
	if !ok {
		return nil, false
	}

	vt, ok := pt.ElemType.(*irtypes.StructType)
	if !ok {
		return nil, false
	}

	for i, f := range vt.Fields {
		fpt, ok := f.(*irtypes.PointerType)
		if !ok {
			continue
		}

		ft, ok := fpt.ElemType.(*irtypes.FuncType)
		if !ok {
			continue
		}
		// The `message` method has signature `(*Err data) TinString`.
		// TinString is the {i8*, i64} struct; match by return type.
		retSt, ok := ft.RetType.(*irtypes.StructType)
		if !ok {
			continue
		}

		if len(retSt.Fields) != 2 {
			continue
		}

		if _, ok := retSt.Fields[0].(*irtypes.PointerType); !ok {
			continue
		}

		if it, ok := retSt.Fields[1].(*irtypes.IntType); !ok || it.BitSize != 64 {
			continue
		}

		if len(ft.Params) != 1 {
			continue
		}

		return &errMessageSlot{vtType: vt, fnPtrType: fpt, slotIdx: i}, true
	}

	return nil, false
}

// newCMainWrapper creates the C-side entry-point function under the IR
// name `_tin_c_main`, plus an `@main` alias so libc / `__libc_start_main`
// still finds the conventional entry symbol. The rename keeps stacktrace
// frames inside the wrapper distinct from the user's `fn main` (compiled
// as `_tin_user_main` and displayed as `main`); without the rename, the
// trace would show two consecutive `main`-named frames and confuse
// readers about which is which.
//
// `withArgs` controls whether the wrapper takes the libc (argc, argv)
// signature; the caller decides based on the user main's parameter list.
//
// LLVM aliases are handled by both ld.lld and GNU ld; on Mach-O the
// convention is the same alias syntax via `--defsym` equivalent.
// Returns the wrapper *ir.Func - the alias is internal bookkeeping.
func (cg *CodeGen) newCMainWrapper(withArgs bool) *ir.Func {
	var wf *ir.Func
	if withArgs {
		wf = cg.mod.NewFunc("_tin_c_main", irtypes.I32,
			ir.NewParam("argc", irtypes.I32),
			ir.NewParam("argv", irtypes.NewPointer(irtypes.I8Ptr)),
		)
	} else {
		wf = cg.mod.NewFunc("_tin_c_main", irtypes.I32)
	}

	cg.mod.Aliases = append(cg.mod.Aliases, ir.NewAlias("main", wf))

	return wf
}

// applyStacktracePostPass walks every emitted function and tags it with
// `frame-pointer="all"` when the program references stacktrace(). Required
// for the runtime's frame-pointer walker (runtime/stacktrace.c, fp_walk)
// to step through every Tin frame: LLVM at -O2 otherwise elides %rbp
// setup on leaf / short functions and the FP walk skips them.
//
// Must be the LAST step in Generate so it covers everything that
// cg.mod.NewFunc has produced - user fns, atom helpers, ADT release/retain
// helpers, coro splits, lambda thunks, test runners, REPL cells. The
// helper is shared across the three Generate exit branches (test runner,
// REPL, normal main) so none of them slip past the tagging.
//
// clang's `-fno-omit-frame-pointer` cmd-line flag does NOT propagate into
// IR-compiled functions; it only sets the default for code clang
// generates from C source. Function attributes embedded in the IR are
// the only mechanism that survives the IR -> object pipeline.
func (cg *CodeGen) applyStacktracePostPass() {
	if !cg.stacktraceUsed {
		return
	}

	for _, f := range cg.allFuncs() {
		if f.Blocks == nil {
			continue // declarations don't carry codegen attributes
		}

		f.FuncAttrs = append(f.FuncAttrs,
			ir.AttrPair{Key: "frame-pointer", Value: "all"})
	}
}

// mainTakesStringArgs reports whether the user's explicit fn main has a first
// parameter of type [string] (dynamic string array).
func mainTakesStringArgs(n *ast.FuncDecl) bool {
	if n == nil || len(n.Params) == 0 {
		return false
	}

	at, ok := n.Params[0].Type.(*ast.ArrayType)
	if !ok || at.Size >= 0 {
		return false
	}

	st, ok2 := at.Elem.(*ast.SimpleType)

	return ok2 && st.Name == "string"
}

// externHasPrimitiveTypes reports whether all parameter and return types of an
// extern function declaration are built-in (non-struct) types.  Used by
// Pre-pass 2.8 to determine which externs can be safely registered before
// struct types are populated in Pre-pass 3.
func externHasPrimitiveTypes(fd *ast.FuncDecl) bool {
	for _, p := range fd.Params {
		if !typeExprIsPrimitive(p.Type) {
			return false
		}
	}

	return typeExprIsPrimitive(fd.RetType)
}

func typeExprIsPrimitive(te ast.TypeExpr) bool {
	if te == nil {
		return true
	}

	switch t := te.(type) {
	case *ast.SimpleType:
		switch t.Name {
		case "i8", "i16", "i32", "i64", "i128",
			"u8", "u16", "u32", "u64", "u128",
			"f32", "f64", "f128",
			"byte", "char", "bool", "string", "void":
			return true
		}

		return false
	case *ast.PointerType:
		return typeExprIsPrimitive(t.Elem)
	case *ast.ArrayType:
		return typeExprIsPrimitive(t.Elem)
	default:
		return false
	}
}
