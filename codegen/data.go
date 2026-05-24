package codegen

// sortedVariants returns the entries of variants sorted by tag so that
// codegen emission is deterministic across program runs (Go's map
// iteration is randomized per run, which would otherwise blow byte-for-
// byte determinism in the IR - see TestIRDeterminism).

// int8 cap silently truncated at the 129th variant.

// should be `false` (no retain). Callers that cannot distinguish may pass
// nil, which is equivalent to all-false (literal semantics).

//  5. Calls _tin_release on the outer block.
//
// Weak fields are skipped (they're non-owning by construction).

// recognizes the form; otherwise returns (nil, false, nil) so the caller can
// fall through to the existing union-is-check logic.
