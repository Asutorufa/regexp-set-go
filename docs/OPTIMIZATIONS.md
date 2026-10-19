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
   is assigned to exactly one candidate group. Filters return *groups*, **not
   individual candidate pattern IDs**. This avoids needing candidate-dependent
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
   through aliases. Plain case-sensitive literals, optionally anchored at the
   start/end of text or line, are factored into shared rune-prefix tries.
   Anchor kinds are kept separate. General regexes, case-folded literals,
   non-literal prefixes, alternations and repetitions are **not** subject to
   generic prefix factoring. The trie is compiled into ordinary NFA
   instructions, not into a separate runtime matcher.

### Stage 2: reduce DFA state and cache overhead

4. **Sharded state interning (dfa.go).** State hashes distribute across 32
   independently locked intern tables. Entries are compared by their entire
   sorted NFA frontier and accepted IDs; hash collisions do not change the
   match result. State and transition counters use atomic reservations, not
   the old single global mutex. Per-state RW locks protect transition maps.
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
   `MatchFirstRule` can safely stop as soon as the **highest-priority rule**
   (index zero) is satisfied; otherwise it continues scanning, because a
   later substring can satisfy an earlier AND rule. `MatchRuleIDs` always
   returns all satisfied rules sorted by declaration order.

RuleSet does **not** store mutable AND counters in immutable DFA states.
Those counters depend on the entire input prefix and would make the state
space explode if included in the DFA identity. Pooled match-local scratch
stores counters instead. Dynamic "highest remaining possible priority"
proof and arbitrary nested AND/OR rule ASTs remain future work.

## Usage

```go
set, err := regexpset.CompileRulesWithOptions(rules, regexpset.Options{
    MaxStates:              8192,
    MaxTransitions:         131072,
    MaxCacheBytes:          16 << 20, // approximate retained state/edge budget
    DisableCandidateShards: false,
    ResetCacheOnFull:       false,
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

The performance-critical remaining work includes dynamic candidate-dependent
start-state caching without losing correctness, byte-exact cache allocation
management, adaptive eviction of individual DFA states (instead of
whole-generation reset), general regex-prefix factoring, and additional
workload-specific fast paths. These are not claimed by this implementation.

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
