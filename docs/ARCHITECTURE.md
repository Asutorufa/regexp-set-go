# RegexSet architecture (optimized version)

This describes the **local optimization candidate** built on top of the
public `regexp-set-go` engine; it retains Go `regexp.Regexp.MatchString`
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
   entry and inbound edges under a generation-wide eviction lock; readers
   holding a temporary pointer can finish safely without resurrecting the
   evicted state. Intern shards, per-state locks and atomic budgets are
   synchronized.
8. Rule priorities use array order. Lower-priority early matches cannot
   defeat unresolved higher-priority AND rules. AC **per-pattern** necessary
   literals can prove a higher-priority rule impossible, allowing safe early
   returns without declaring a false regex match.

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
observed state, deletes its inbound cached transitions, and permits admission
of a replacement. This is a potentially expensive O(number of retained states)
scan per eviction, **not** a constant-time LRU. Whole-generation reset is a
separate, opt-in alternative. Neither policy enforces Go heap/RSS limits.

Remaining limits: exact byte-level Go heap allocation control, O(1) eviction,
generic arbitrary-prefix automaton factoring and complete Unicode
casefolding-aware classes. Per-pattern verification duplicates some compiled
regular-expression memory outside the DFA budget. See `OPTIMIZATIONS.md`.
