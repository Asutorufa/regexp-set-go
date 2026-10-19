# Engine optimizations: implementation and tradeoffs

This document describes the implementation in the October 2026 optimization
series. It complements [ARCHITECTURE](ARCHITECTURE.md), [BENCHMARKS](BENCHMARKS.md),
and [RULESET](RULESET.md). All claims distinguish semantics from measured
performance. The matcher remains pure Go and requires no external packages.

## How the three stages map to code

### Stage 1: avoid redundant NFA work

1. **Candidate-aware AC (prefilter.go, set.go).** Aho-Corasick failure nodes
   inherit a bitmask of candidate *machine groups*. The eligible regexes are
   partitioned by the hash of their safely extracted required literal, with
   up to 16 groups, approximately 512 patterns per group. An AC scan identifies
   which groups could match; the others are skipped. This is intentionally
   conservative: any match contains its required literal, and every pattern
   is assigned to exactly one candidate group. AC also collects individual pattern IDs. On sparse input (at most 16
   candidates and at most 1/8 of the eligible patterns), their independently
   compiled Go regexes verify exact matches. Dense input falls back to the
   shared DFA. This intentionally duplicates verifier memory outside the DFA
   budget. This avoids needing candidate-dependent
   DFA states or separate machine instances for every input bitmap. Set
   `DisableCandidateShards: true` to use a single eligible group instead.
2. **Memoized NFA start closures (nfa.go).** The epsilon closure of each
   machine's pattern roots is constructed only once per observed empty-width
   context and memoized. Each input boundary still merges that frontier with
   the continuing match frontier, preserving arbitrary-substring matching,
   anchors, and word boundaries. This removes repeated traversal of start
   epsilon chains but does **not** remove the cost of merging a potentially
   large set of start-consuming instructions on every cold DFA transition.
3. **Literal-prefix factoring (trie.go, compile.go).** Identical compiled
   regexp/syntax trees reuse one NFA program and emit all original pattern IDs
   through aliases. Pure case-sensitive literals, optionally anchored at the start/end of text
   or line, are factored into shared rune-prefix tries. Case-sensitive
   deterministic leading literals followed by **arbitrary regexp suffixes**
   now share the leading trie while the suffix remains a normal compiled NFA.
   Anchor kinds are kept separate. Case-folded and ambiguous/alternating
   leading prefixes are not generically factored. The trie is compiled into ordinary NFA
   instructions, not into a separate runtime matcher.

### Stage 2: reduce DFA state and cache overhead

4. **Sharded state interning (dfa.go).** State hashes distribute across 32
   independently locked intern tables. Entries are compared by their entire
   sorted NFA frontier and accepted IDs; hash collisions do not change the
   match result. State and transition counters use atomic reservations, not
   the old single global mutex. Uncached states reuse a scratch-owned
   `dfaState`, while interning creates an immutable deep copy. Per-state RW locks protect transition maps.
5. **Safe generation recycling (dfa.go).** Cache generations are published
   with an atomic pointer. Reset swaps the pointer and leaves previous
   generations intact, allowing in-flight readers to finish. New edges are
   attached only within one generation. Reset remains opt-in because it can
   cause heavy churn for small budgets.
6. **Estimated byte budgeting (set.go, dfa.go).** `MaxCacheBytes` bounds
   *tracked state and edge reservations* (object header, frontiers, IDs and
   a map-entry allowance) across eligible and unfiltered machines. `Bytes`
   exposes the tracked count. This is **not an exact heap/RSS limit**: map
   bucket capacity, allocator overhead, the compiled NFA, AC nodes, shared
   start cache, goroutine-local pools, and old generations awaiting GC are
   excluded. Protect against untrusted regexes with process limits as well.
7. **Unicode equivalence ranges (unicode.go).** ASCII characters use the
   existing exact consumption-equivalence classes. Non-ASCII intervals are
   split at consuming-instruction rune-range boundaries, and adjacent
   intervals are merged only when every consuming instruction agrees.
   The zero-width context remains in the DFA edge key. To preserve semantics
   the optimizer declines Unicode partitioning when any folded rune
   instruction is present or the compile budget is exceeded; it then caches
   non-ASCII transitions by their code point.
8. **Adaptive frontier canonicalization (nfa.go).** Small NFA frontiers are
   sorted and deduplicated. At 128 entries, sufficiently dense frontiers use
   a reusable bitset and clear only touched words. Very sparse frontiers
   continue using sort to avoid allocating oversized bitmaps.

### Stage 3: rule-aware matching

9. **Streaming RuleSet aggregation (rules.go, dfa.go, set.go).** The underlying
   machine visits each distinct matched pattern identity and feeds it into
   the associated rule's hit count without first building and sorting a full
   pattern-ID list. `MatchAny` can stop as soon as *any* rule is satisfied.
   `MatchFirstRule` can safely stop as soon as the **earliest rule still possible**
   under the AC group mask is satisfied; otherwise it continues scanning,
   because a later substring can satisfy an earlier AND rule. `MatchRuleIDs` always
   returns all satisfied rules sorted by declaration order.

RuleSet does **not** store mutable AND counters in immutable DFA states.
Those counters depend on the entire input prefix and would make the state
space explode if included in the DFA identity. Pooled match-local scratch
stores counters instead. The proof is conservative and only excludes rules whose necessary AC
pattern group is absent. Per-pattern AC candidate presence now also proves a higher-priority AND
requirement impossible when its mandatory substring is absent. Nested AND/OR
ASTs remain future work.

## Usage

```go
set, err := regexpset.CompileRulesWithOptions(rules, regexpset.Options{
    MaxStates:              8192,
    MaxTransitions:         131072,
    MaxCacheBytes:          16 << 20, // approximate retained state/edge budget
    DisableCandidateShards: false,
    ResetCacheOnFull:       false,
    DisableExactCandidates: false,
    EvictLRU:              false, // opt-in; benchmark before enabling
})
if err != nil { panic(err) }

first, ok := set.MatchFirstRule("example.org")
_ = first
_ = ok
fmt.Printf("cache: %+v\n", set.CacheStats())
```

## Performance limitations and measured counterexamples

The following are **illustrative**, from Linux/amd64 Go 1.23.2 on a shared
AMD EPYC 9V74 host, short 100ms measurements with compiled patterns and
cache warmup. They are not stable cross-machine speedups.

| Workload | Variant | ns/op | B/op | allocs/op |
| --- | --- | ---: | ---: | ---: |
| 4,096 anchored host patterns, 2,048 varied inputs, 50% hits | candidate groups | ~2,271 | ~675 | ~10 |
| Same | no prefilter / one machine | ~1,658 | ~583 | ~9 |
| 1,024 anchored literal regexes, one warm matched input | shared literal-trie DFA | ~559 | 0 | 0 |
| Same | early-exit loop over precompiled Go regexp | ~12,991 | 0 | 0 |
| 1,024 logical rules, no-match input | `RuleSet.MatchAny` | ~142 | 0 | 0 |

A grouping optimization may **regress** workloads with many distinct
positive inputs, because the state budget is partitioned between machines.
The unfiltered baseline benefits from a larger shared cache. Use the
`DisableCandidateShards` and `DisablePrefilter` switches in local benchmarks.
Compare *matching the first result* against first-result baselines, and
*returning all matching IDs* against all-result baselines.

Opt-in per-state LRU is now available. It selects the least recently used
retained state by an atomic recency stamp and removes incoming edges before
reclaiming accounting reservations; eviction scans retained states and can
therefore cost O(states + edges). Additional work remains: byte-exact Go heap
allocation management (not possible with current maps/GC accounting), O(1)
recency eviction, dynamic candidate-dependent start-state caching without
state explosion, general regex-prefix factoring, and workload fast paths. These are not claimed by this implementation.

## Validation

```sh
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
go test -fuzz '^FuzzRuleSetAgainstRegexp$' -fuzztime=30s .
go test -run '^$' -bench 'BenchmarkCandidateShardsHighCardinality|BenchmarkRuleSetPriorityStreaming|BenchmarkLiteralTrieOracles' -benchmem ./...
```

The differential tests cover literal-prefix sharing, zero-width assertions,
Unicode classes, invalid UTF-8, candidate sharding, duplicate pattern IDs,
AND/OR priorities, tiny byte budgets and concurrent cache recycling.

### Satisfied-AND / first-priority comparison

`BenchmarkRuleSetSatisfiedAND` adds a missing case: 512 logical rules with
three overlapping AND expressions each, and inputs that fully satisfy early,
middle, or late rules. It compares the same `MatchFirstRule` result to a
precompiled sequential Go `regexp` loop that **also stops at the first
satisfied rule**. Illustrative 100ms Linux/amd64 Go 1.23.2 measurements:

| Input | RuleSet first (ns/op) | Early-exit regexp loop (ns/op) |
| --- | ---: | ---: |
| First rule satisfies AND | 10,936 | 584 |
| Middle rule satisfies AND | 10,474 | 4,736 |
| Last rule satisfies AND | 10,004 | 17,596 |
| No rule satisfies AND | 710 | 2,200 |

These results are **not** evidence that one matcher universally dominates:
RuleSet pays the overhead of scanning its combined machines even when the
first logical AND rule wins, whereas the sequential loop is exceptionally
cheap when the first rule succeeds. For routing work with a very hot first
rule, consider a specialized fast path or smaller ordered tiers before the
shared matcher. For large no-match corpora, prefiltering remains valuable.

This release deliberately does not promise byte-exact Go heap limits or
constant-time LRU: Go map bucket overhead, independently compiled Go regex
verifiers, concurrent snapshots and old generations awaiting collection mean
a precise retained-memory cap would require a different cache allocator. `MaxCacheBytes` bounds only the tracked
cache reservations described above. Prefer explicit semantics to a false
memory-safety guarantee.

## Further optimizations (October 2026)

- **Revocable DFA handles:** transitions in the optional per-state LRU store
  monotonically increasing slot IDs rather than owning pointers. Eviction
  removes the resident state from the index, slot table and LRU list without
  scanning incoming references. Expired edge handles are resolved as cache
  misses, and replaced in place. The core unlink uses expected O(1) Go
  map/list operations, not strict worst-case timing; incoming stale edge keys
  occupy the source's accounted edge budget until accessed or evicted.
- **Complete Unicode fold correctness:** for manageable NFAs, exact interval
  boundaries incorporate all Unicode SimpleFold cycle members in addition to
  regexp instruction interval boundaries. Large NFAs use a lazy per-rune
  signature map, comparing every consuming instruction on first encounter.
  This preserves class equivalence for *any* Go/RE2 Unicode fold rather than
  falling back to per-codepoint DFA symbols. The lazy map uses extra process
  memory outside the tracked DFA budget, so it is not a heap/RSS cap.
- **Extended guaranteed-prefix factoring:** handles AST concatenation, captures,
  alternatives with shared literal prefixes, and positive repetitions. It
  still does not claim expensive general regular-language minimization or
  equivalent-set merging for arbitrary character-class prefixes.
- **SIMD prefilter:** an amd64 SSE2 byte-test kernel is supplied and tested;
  the production common-first-byte prefilter chooses the faster Go standard
  library `strings.IndexByte` architecture-dispatched implementation. On a
  4 KiB benchmark that implementation outperformed the simple SSE2 version.
  Full automata SIMD/vectorization remains unimplemented.

### Symbolic prefix factoring

The compiler now recognizes guaranteed consuming prefixes that include not
only literal runes, but also case-folded literal runes, character classes,
any-rune and any-rune-except-newline atoms. Identical symbolic atoms share a
prefix trie, with their full regexp suffixes preserved as Thompson programs.
Alternatives and positive repetition are factored when structural equivalence
can be proven without changing the matched language. Tests compare thousands
of sample inputs with Go `regexp`, including anchors and invalid UTF-8.

This expands compile-time sharing considerably but **does not solve arbitrary
regular-language equivalence**. Unbounded minimization of arbitrary regular
languages can explode exponentially. The combined lazy DFA continues to
share reachable matching frontiers at runtime.

A common-first-byte AC precheck has a focused benchmark: on a 4 KiB no-hit
input consisting entirely of bytes outside the one-byte root, the optimized
precheck took ~56 ns/op versus ~76,521 ns/op for the full AC scan on one
Linux/amd64 shared VM (100ms samples). This does not apply when the root has
multiple outgoing first bytes or the input contains the candidate byte.
