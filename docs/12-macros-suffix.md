# Suffix macros

Literal-suffix macros let a Tin literal bind to a macro that expands at parse
time, producing typed code in place. `4_GiB`, `10_kg`, `3.75_percent`,
`"<div>"_html`, and `2_i` (complex imaginary) all run through this
mechanism. The macro receives the literal as its first argument; the
expanded code goes in place of the whole `<literal>_<suffix>` source range.

## Motivation

Bytes, durations, angles, currencies, units, and one-shot string transforms
all want a typed-literal shape. `4_GiB` is more honest about intent than
`4 * 1073741824` and survives refactors without copying the magic number.
The macro is the right tool: parse-time expansion means the IR is identical
to the hand-written form, no runtime cost.

## Surface syntax

A macro becomes a literal suffix by adding the `#suffix@<kind>` tag.
The macro's first parameter binds the literal value; additional
parameters bind to ordinary call args (when `#no_parens` is absent).

Tin macros default to requiring `!` at the call site (`foo!(...)`).
The `#no_excl` tag opts out so the macro is called as `foo(...)` (or
`<lit>_foo` for the suffix form). Suffix macros follow the same rule:
`4_GiB` requires `#no_excl` on `GiB`; `4_GiB!` requires a macro named
`GiB!` (no `#no_excl`).

```
# Bare-name suffix: needs #no_excl.
# {expr} inside backticks is EVALUATED at macro expansion time, so
# the constant ends up folded in the emitted IR rather than left as a
# runtime multiply.  `4_GiB` expands to the literal 4294967296.
macro{#suffix@int #no_excl #no_parens} GiB(n):
  return `{n * 1073741824}`

macro{#suffix@float #no_excl #no_parens} percent(n):
  return `{n / 100.0}`

# Multi-kind: repeat the #suffix tag, one per accepted literal kind.
# The cast happens at runtime; the macro body emits one struct literal.
macro{#suffix@int #suffix@float #no_excl #no_parens} i(n):
  return `complex::Complex{re: 0.0, im: {n} as f64}`

# Extra params (no `#no_parens`).
# Action is a code arg; spliced verbatim into the loop body.
macro{#suffix@int #no_excl} times(n, action):
  return `for let _i i64 = 0; _i < {n}; _i = _i + 1: {action}`

# Bang form: name ends in `!`, no #no_excl, called as `<lit>_foo!`.
# Mixed: runtime assertion + macro-time fold.
macro{#suffix@float #no_parens} percent!(n):
  return `assert_pct({n}); {n / 100.0}`
```

Call sites:

```
let cache_size = 4_GiB                       # int macro, no parens
let tolerance  = 0.5_percent                 # float macro, no parens
let z          = 3 + 2_i                     # int->Complex via i macro
3_times(echo("foo"))                         # int macro with extra arg
let safe       = "select * from users"_sql   # string macro
let banner     = "  hello  "_trim            # string macro

# Package-qualified, no import needed.
let pkt = 1500_units::memory::B
```

## Tag reference

| Tag          | Effect                                                            |
| ------------ | ----------------------------------------------------------------- |
| `#suffix@T`  | Macro is callable as a literal suffix; `T` is the prefix kind (`int`, `float`, `string`, `bool`). Repeat for multi-kind macros. |
| `#no_excl`   | Reject the `!` form (`4_GiB!`) at parse time with a clear error. Without it, `4_GiB!` looks up a separate `GiB!` macro (see below). |
| `#no_parens` | Reject `(args)` after the suffix. Macro signature must be exactly `(n)`. |

`#suffix@T` is the only required tag. `#no_excl` and `#no_parens` are
opt-in restrictions that give better error messages and let the macro
author lock down the call shape.

### `#no_excl` semantics

The bare-name form (`e10`) and the bang form (`e10!`) are **separate
macros**. The bare name needs `#no_excl`; the bang name doesn't:

```
macro{#suffix@int #no_excl} e10(n):
  return `{n * 10000000000}`

macro{#suffix@int} e10!(n):
  return `{n * 10000000000} + epsilon()`

let a = 4_e10        # expands to literal 40000000000
let b = 4_e10!       # expands to 40000000000 + epsilon()
```

The two coexist freely. If `e10!` is not defined, `4_e10!` is a parse
error: "no suffix macro `e10!` in scope; did you mean `e10`?".  If
`e10` is not defined (only `e10!` is), `4_e10` is the same error in
the other direction.

The `!` form is also the **disambiguator** for hex / float-exponent
ambiguity (see Disambiguation below). A user who needs to force suffix
interpretation against an ambiguous numeric form types `!`:

```
let x = 0x10_10_e10!   # forces suffix `e10!`; without `!` the lexer
                       # would prefer the hex separator interpretation
```

## Lexer + parser model

Pure parse-time expansion. The lexer recognizes the `<literal>_<ident>`
pattern after any complete literal (`4`, `1_000_000`, `0xFF`, `0b1010`,
`1.5`, `1.5e3`, `"..."`, `true`, `false`). The parser looks the suffix
up in scope; if it resolves to a `#suffix@<kind>` macro whose kind
matches the literal, the macro expands. If the kind doesn't match, the
parser errors with `suffix \`X\` accepts <kinds> but got <literal-kind>`.

The expansion replaces the entire `<literal>_<suffix>` source range
with the macro's returned AST. From there, normal parsing continues.
The IR is identical to the hand-written expansion; there is no runtime
suffix-resolution.

## Disambiguation rules

Tin already uses `_` as a numeric digit separator (`1_000`). The
suffix-macro syntax shares that character. The rules:

1. **After a complete literal, `_<ident-start>` may start a suffix.**
   The lexer scans the ident (and any `::`-qualified scope chain) and
   asks the macro registry whether `<ident>` (or `<ident>!` when `!`
   trails) is registered as a `#suffix@<kind>` for the literal's kind.

2. **If the lookup hits, treat as suffix.** Otherwise, treat as
   numeric separator continuation. For `1_000_GiB`: `1_000` resolves
   to int `1000`, then `_GiB` lookup succeeds, expansion proceeds.

3. **Hex / binary / octal:** same rule. `0xFF_GiB` works because `G`
   isn't a hex digit. `0xFF_A` is ambiguous if `A` is a registered
   suffix AND `A` is a valid hex digit. The lexer's preference: SUFFIX
   wins, with a `-Whex-suffix-shadow` warning pointing at the
   ambiguity so the user can disambiguate explicitly.

4. **Float exponents:** `1e10` is a float literal. `1_e10` is `1`
   then suffix `e10` (the underscore breaks the float-literal scan).
   `1.5e3_GiB` is `1.5e3` float then suffix `GiB`.

5. **`!` as forced-suffix disambiguator:** trailing `!` cannot appear
   inside any numeric literal, so it forces the parser to take the
   suffix branch even when a numeric interpretation is also valid.
   `0x10_10_e10!` parses as `0x1010` then suffix `e10!`. Without `!`,
   the lexer prefers the hex separator and produces `0x101010e10`.

6. **Suffix names must start with a letter.** All-digit names are
   rejected at macro-decl time; this rules out the `1_000` ambiguity
   collapsing onto a `000` macro.

## Macro resolution

* Suffix lookup uses the same scope as regular macros. A `use` brings
  the name into scope; without it, the fully-qualified form
  (`4_units::memory::GiB`) is required.
* No auto-import. The compiler does not scan every available package
  for a matching `#suffix` macro at use sites.
* Same registry, same overload rules: `GiB` can be both a regular
  macro and a `#suffix@int` macro; the call form picks the entry.

## stdlib layout

```
stdlib/units/
  memory.tin    B, KiB, MiB, GiB, TiB, PiB,
                KB, MB, GB, TB, PB                        -> i64
  time.tin      ns, us, ms, s, min, h, d                  -> time::Duration
  angle.tin     deg, rad                                  -> f64
  si.tin        m, mm, cm, km, g, kg, mg, ug, A, mA, K,
                mol, cd                                   -> f64 (or unit-typed wrappers)
  chem.tin      Da, kDa, MDa, M (molar), mM, uM, nM, pM,
                eV, keV, MeV                              -> f64
```

`complex::i` (existing complex package) gains a `#suffix@int
#suffix@float` registration so `2_i` and `2.5_i` both produce a
`Complex` value. The package stays separate from `units::` since
imaginary isn't a unit.

Idiom: prefer explicit import for hot suffixes used in one file.

```
use { GiB, MiB } from units::memory
use { ms, s } from units::time
use { i } from complex

let buf       = 4_GiB
let timeout   = 30_s
let amplitude = 1 + 0.5_i
```

For cross-file consistency, the fully-qualified form is acceptable:

```
let buf = 4_units::memory::GiB
```

## Out of scope (deferred)

* **Generic suffix macros.** No generic macros in Tin today; suffix
  macros inherit that limitation.
* **Infix macros** (`5_meters_per_second`). Out of scope.
* **Unit checking** (rejecting `4_kg + 3_m`). Would require a
  units-typed numeric system, which is a separate feature.
* **Auto-import of obvious suffixes.** Considered and rejected;
  surprises don't pay for the saved keystrokes.

## Implementation phases

1. Lexer: recognize `_<ident-start>` and `!` trailing markers after
   any complete literal; emit a `LITERAL_SUFFIX` token paired with the
   literal.
2. Parser: at suffix-token sites, resolve the macro and expand inline.
   Surface clear errors for kind mismatch, unknown suffix, `#no_excl`
   violation.
3. Macro registry: register `#suffix@<kind>` macros under both the
   bare name and the `!`-suffixed variant slot; reject all-digit suffix
   names; emit `-Whex-suffix-shadow` when hex disambiguation triggers.
4. stdlib: write `units::memory`, `units::time`, `units::angle`,
   `units::si`, `units::chem`. Add `i` to `complex`.
5. Tests: fixtures for each kind, each tag combination, each
   disambiguation rule. Regressions for every "this used to error
   wrong" case from the spec.
6. Docs: usage examples in `docs/12-macros.md` cross-linking to this
   file.
