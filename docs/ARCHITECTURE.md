# RegexSet architecture (optimized version)

This describes the `regexp-set-go` engine; it retains Go `regexp.Regexp.MatchString`
existence semantics and the original pattern ID for duplicate expressions.

## Compile pipeline

```text
regexp/syntax.Parse + Simplify
  |
  +--> mandatory ASCII substring analysis
  |       |
  |       +--> no necessary literal -> unconditional NFA group
  |       +--> necessary literal -> AC output identifies candidate *groups*
  |
  +--> Thompson NFA emission with duplicate aliases
  +--> pure literal trie and deterministic literal-prefix / regex-tail trie
  +--> memoized start epsilon closures, per boundary context
  +--> ASCII / safe Unicode equivalence classes
  +--> sharded lazy DFA state interner and bounded transition caches

RuleSet receives streamed pattern IDs -> OR / AND counters -> declared
priority. It does not create one DFA per logical rule.
```

## Matching invariants

1. A prefilter is a **necessary** test only; it cannot exclude a matching
   pattern. AC outputs candidate machine masks and individual pattern IDs.
   Sparse candidate sets are checked using precompiled Go regexes; dense
   candidates use the shared DFA. Candidate sets never become DFA identities,
   avoiding input-dependent state explosion.
   Ordinary Set and any/all RuleSet queries combine group-mask and exact-ID
   discovery in one AC scan. Exact-ID collection stops once the sparse
   verification threshold is exceeded, while the group mask still covers
   the whole input. Priority proofs use an unbounded candidate snapshot.
2. Each rune boundary implicitly starts new unanchored matches; start
   epsilon closures are cached by the exact `syntax.EmptyOp` boundary context.
   Anchored-prefix trie roots check their own beginning assertion.
3. Prefix factoring is applied only to deterministic, case-sensitive
   literals in compatible AST positions. Regex suffixes retain their original
   compiled NFA instructions, including suffix assertion semantics.
4. A DFA state identity includes sorted consuming PCs and accepted pattern
   IDs. Collision-safe hashes are checked with full equality.
5. Unicode interval equivalence is disabled for problematic folding or
   compiler work bounds; zero-width assertions are independently included
   in edge keys. UTF-8 malformed sequences follow Go's RuneError decoding.
6. Uncached DFA state objects borrow a per-call scratch object. Cache inserts
   publish separately allocated deep copies with no aliases into scratch.
7. A cache generation switch never mutates old states held by concurrent
   readers. With optional per-state LRU, eviction removes a victim's intern
   entry, slot and outgoing edges under a generation-wide eviction lock; readers
   holding a temporary pointer can finish safely without resurrecting the
   evicted state. Intern shards, per-state locks and atomic budgets are
   synchronized.
8. Rule priorities use array order. Lower-priority early matches cannot
   defeat unresolved higher-priority AND rules. AC **per-pattern** necessary
   literals can prove a higher-priority rule impossible, allowing safe early
   returns without declaring a false regex match.
9. FieldRuleSet shares identical `(field, pattern)` Boolean predicates across
   expression leaves. Original leaf counts and rule IDs remain unchanged;
   field evaluation marks membership directly without sorting an ID list.
   Standalone Set and flat RuleSet retain every original pattern identity,
   including repeated AND requirements.

## Complexity and tradeoffs

AC lookup is amortized linear in input bytes; warm DFA transitions are close
to linear in runes plus output processing. On a cache miss, the engine must
recompute consuming NFA frontiers and epsilon closures. Shared prefix tries
and memoized root closures reduce but do not eliminate this cost.

DFA state space may still be exponential in NFA program complexity. A
state-count and an **estimated byte reservation** budget bound retained
accounted cache entries but **not** RSS or complete Go heap use. Compiled
programs, AC tries, scratch pools, old GC generations and map overhead are
outside the budget. Optional `EvictLRU` chooses the oldest approximately
observed state and unlinks it in expected O(1) map/list work. Incoming edges
hold revocable slot IDs and become cache misses when their target is evicted;
they do not keep the victim alive. Stale edges still consume reservations
until replaced or their source is evicted. Whole-generation reset is a
separate, opt-in alternative. Neither policy enforces Go heap/RSS limits.

Remaining limits include exact byte-level Go heap allocation control and
generic arbitrary-language minimization. Exact Unicode equivalence classes
are computed eagerly for manageable programs and lazily for large programs.
Per-pattern verification duplicates some compiled regular-expression memory
outside the DFA budget. Identical verifier sources share one regexp object;
sets with fewer than eight eligible patterns omit the unreachable verifier
path. Compile-only duplicate-program lookup is released after NFA assembly.
See `OPTIMIZATIONS.md`.
