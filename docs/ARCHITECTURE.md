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
   pattern. Candidate masks select machines, not individual dynamic DFA
   frontiers. This avoids input-specific DFA cache identities.
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
   readers. Intern shards, per-state transition locks and atomic budget
   reservations are synchronized.
8. Rule priorities use array order. Lower-priority early matches cannot
   defeat unresolved higher-priority AND rules. AC candidate-group exclusions
   can prove that a higher-priority rule is impossible, allowing early return.

## Complexity and tradeoffs

AC lookup is amortized linear in input bytes; warm DFA transitions are close
to linear in runes plus output processing. On a cache miss, the engine must
recompute consuming NFA frontiers and epsilon closures. Shared prefix tries
and memoized root closures reduce but do not eliminate this cost.

DFA state space may still be exponential in NFA program complexity. A
state-count and an **estimated byte reservation** budget bound retained
accounted cache entries but **not** RSS or complete Go heap use. Compiled
programs, AC tries, scratch pools, old GC generations and map overhead are
outside the budget. Replacing the whole generation is optional, not LRU.

Partial optimizations intentionally left conservative: AC only narrows to
pattern groups; generic arbitrary-prefix automaton factoring, precise
memory accounting, per-state eviction and complete Unicode casefolding-aware
classes are substantial remaining work. See `OPTIMIZATIONS.md`.
