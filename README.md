# regexp-set-go

[![CI](https://github.com/Asutorufa/regexp-set-go/actions/workflows/ci.yml/badge.svg)](https://github.com/Asutorufa/regexp-set-go/actions/workflows/ci.yml)

**Pure Go multi-pattern regular-expression matcher**, without CGO or third-party
dependencies. Go 1.23+. Patterns use Go/RE2 syntax via `regexp/syntax`.
Matches a complete input **with substring semantics** (like
`regexp.Regexp.MatchString`). Captures, spans, streaming across chunks,
lookarounds and backreferences are not supported.

The project exposes two levels:

| API | Purpose | Result |
| --- | --- | --- |
| `Compile` / `Set` | Thousands of independent regex patterns | Original 0-based pattern indexes, `[]int` |
| `CompileRules` / `RuleSet` | Logical priority-ordered AND/OR groups | Caller-assigned `uint32` rule IDs, in priority order |

## Quick start: logical rules and priority

```go
package main

import (
    "fmt"
    regexpset "github.com/Asutorufa/regexp-set-go"
)

func main() {
    rules := []regexpset.Rule{
        {ID: 10, Operator: regexpset.RuleAND, Patterns: []string{`google`, `\.com$`}},
        {ID: 20, Operator: regexpset.RuleOR,  Patterns: []string{`github`, `google`}},
        {ID: 30, Patterns: []string{`doubleclick`}}, // OR by default
    }
    set, err := regexpset.CompileRules(rules)
    if err != nil { panic(err) }

    id, ok := set.MatchFirstRule("www.google.com")
    fmt.Println(id, ok) // 10 true
    fmt.Println(set.MatchRuleIDs("www.google.com")) // [10 20]

    dst := make([]uint32, 0, 8)
    dst = set.MatchRuleIDsInto("github.doubleclick.net", dst)
    fmt.Println(dst) // [20 30]
}
```

- **Priority**: rules appear in descending priority; `rules[0]` wins first.
  The numeric `ID` is an opaque identifier, **not** a priority value.
- **OR**: any expression matches anywhere in the same input.
- **AND**: every expression matches somewhere in the same input. Their
  matched spans need not overlap or be adjacent.
- Both operators can coexist in a `RuleSet`; duplicated pattern strings retain
  separate identities, and logical rule IDs must be unique. Nested Boolean
  groups and different-field predicates (Host AND Path) are not provided.
- `MatchFirstRule` may stop before reading the whole input **only when it can
  prove no higher-priority rule remains possible**. An early lower-priority
  substring is *not* sufficient for an early return.
- `MatchAny` stops once any logical rule becomes satisfied. `MatchRuleIDs`
  always returns every satisfied rule in priority order.

For individual pattern indexes rather than grouped rules:

```go
set, err := regexpset.Compile([]string{`foo`, `bar`, `^baz$`})
if err != nil { panic(err) }
fmt.Println(set.MatchAny("xxbar")) // true
fmt.Println(set.MatchIDs("foobar")) // [0 1]
var ids []int
ids = set.MatchInto("foobar", ids) // reuse caller-owned buffer
```

## Under the hood

1. Parse and simplify each pattern using `regexp/syntax` and emit a Thompson
   NFA. Identical programs share instructions; simple literal-only and
   **deterministic literal-prefix + regular-expression-suffix** forms share
   leading trie instructions. Complex expressions fall back to normal NFA
   compilation. Pattern IDs are preserved, including duplicates.
2. Extract a *provably required* ASCII substring where possible. A sparse
   Aho-Corasick automaton emits both a bitmask of candidate NFA/DFA groups
   and **individual candidate Pattern IDs**. Patterns without a required
   literal remain unfiltered. When at most 16 individual candidates survive
   and are at most 1/8 of all eligible patterns, compiled Go `regexp` verifies
   only those candidates. Dense hits fall back to the shared lazy DFA. AC
   only proves necessary conditions; it never declares a regex match.
3. Evaluate the selected Thompson NFAs using lazy subset-construction DFA
   states. Each state contains consuming instruction PCs and accepting pattern
   IDs; transitions are cached by `(character equivalence class, zero-width
   context)`. NFA start epsilon-closures are memoized by boundary context.
4. ASCII equivalence classes and conservative Unicode interval classes reduce
   repeated cached edges. State interning is sharded (32 locks); an adaptive
   sparse/bitset frontier representation reduces sorting overhead; uncached
   DFA states reuse match-local scratch storage.
5. `RuleSet` streams distinct accepting pattern IDs into per-rule counters,
   computes AND/OR, and chooses the earliest satisfied rule by declared
   priority. Exact AC candidate bits also eliminate impossible earlier rules
   without skipping viable AND requirements. It never creates a distinct DFA
   per logical rule.

These are **conservative transformations**. Regex behavior is checked against
Go's `regexp` in randomized, corpus, fuzz and concurrent tests. More details:
[architecture](docs/ARCHITECTURE.md), [optimizations](docs/OPTIMIZATIONS.md),
[RuleSet](docs/RULESET.md).

## Cache configuration

```go
set, err := regexpset.CompileRulesWithOptions(rules, regexpset.Options{
    MaxStates:              8192,       // default 4096
    MaxTransitions:         131072,     // default 65536
    MaxCacheBytes:          16 << 20,   // optional estimated reservation budget
    DisablePrefilter:       false,      // enable conservative AC filtering
    DisableCandidateShards: false,      // optional grouping; benchmark hit rate
    DisableExactCandidates: false,     // default: use exact regex verification when sparse
    EvictLRU:              false,      // opt-in per-state recency-based eviction
    ResetCacheOnFull:       false,      // opt-in whole-generation recycling
})
if err != nil { panic(err) }
fmt.Printf("cache: %+v\n", set.CacheStats())
```

`MaxCacheBytes` and `CacheStats.Bytes` count **estimated retained state/edge
reservations**, not process RSS or exact heap allocations. The NFA, AC nodes,
start closures, goroutine-local scratch, map overhead and old generations
awaiting GC, **and separately compiled `regexp` candidate verifiers** are
outside that accounting. This is **not a hard heap/RSS memory sandbox**.
A zero state/transition option selects the default; negative limits fail.
By default a full cache falls back to correct uncached NFA-derived states.
With `EvictLRU`, old individual states and their incoming cached transitions
are removed under synchronization (scanning cached states on an eviction),
allowing subsequent states to be admitted. Recency tracking is approximate
under concurrent matches. Eviction can significantly increase contention and
allocations; it is not a performance guarantee. `ResetCacheOnFull` is a distinct
whole-generation policy and should normally not be combined with `EvictLRU`.
`CacheStats.Evictions` reports evictions in currently retained generations.
`EvictLRU` and `ResetCacheOnFull` are mutually exclusive to avoid surprising
cache-policy transitions.

## Benchmarks

All benchmark functions live in normal Go `*_test.go` files and are runnable
with `go test -run '^$' -bench ... -benchmem`:

| Benchmark | What it measures |
| --- | --- |
| `BenchmarkMatch`, `BenchmarkMatchIDsOracle` | Pattern-level first hit vs all matching IDs, with distinct baseline work |
| `BenchmarkRuleSet` | 100/1k/5k AND/OR logical rules, highest priority and all matches |
| `BenchmarkRuleSetConcurrent`, `BenchmarkParallel` | Shared engine with multiple goroutines |
| `BenchmarkPrefilter`, `BenchmarkCandidateShardsHighCardinality` | AC on/off, candidate grouping, many distinct positive inputs |
| `BenchmarkLiteralTrieOracles`, `BenchmarkGeneralPrefixFactoring` | Literal and generic deterministic-prefix sharing vs separate `regexp` |
| `BenchmarkRuleSetPriorityStreaming`, `BenchmarkRuleSetCandidatePriorityPruning` | Rule-level short-circuiting and priority proof |
| `BenchmarkCacheBudget`, `BenchmarkRuleSetTinyCache` | Cold/underfunded cache and generation recycling |
| `BenchmarkExactCandidates` | Sparse candidate verification vs full shared DFA, miss and hit |
| `BenchmarkEvictLRU` | Opt-in per-state LRU versus correct uncached fallback |
| `BenchmarkCompile` | Parsing and automaton construction time |

```sh
go test -run '^$' -bench '^BenchmarkMatch$' -benchmem -benchtime=1s ./...
go test -run '^$' -bench '^BenchmarkRuleSet' -benchmem -benchtime=1s ./...
go test -run '^$' -bench 'BenchmarkCandidateShardsHighCardinality|BenchmarkGeneralPrefixFactoring|BenchmarkRuleSetCandidatePriorityPruning' -benchmem ./...
go test -run '^$' -bench '^BenchmarkCacheBudget$' -benchmem ./...
go test -run '^$' -bench 'BenchmarkExactCandidates|BenchmarkEvictLRU' -benchmem ./...
```

### Illustrative measured results

Linux/amd64, Go 1.23.2, AMD EPYC 9V74 shared machine, **100–200 ms per
sample**, synthetic rules, caches warmed. These are not production performance
claims or comparisons to Hyperscan/Rust RegexSet:

| Workload | Engine | Measured latency |
| --- | --- | ---: |
| 1,000 anchored regexes, prefilter enabled, negative input | RegexSet | ~432 ns/op |
| 1,000 anchored regexes, 50% hits, prefilter enabled | RegexSet | ~882 ns/op |
| 1,000 anchored literal-prefix + regex-suffix rules, fixed hit | Shared NFA | ~487 ns/op |
| Same prefix/suffix fixture, loop over precompiled `regexp` | Go regexp loop | ~17,144 ns/op |
| 1,024 rules, high-priority impossible via AC, lower priority matches | `MatchFirstRule` | ~20,518 ns/op |
| Same rules/input, return all satisfied rules | `MatchRuleIDsInto` | ~46,096 ns/op |
| 4,096 varying host rules, 50% hit | AC candidate groups | ~2,349 ns/op |
| Same workload with prefilter disabled | Single NFA/DFA | ~1,690 ns/op |

**Important counterexample:** AC candidate sharding can be slower on
high-cardinality positive traffic, and very small caches can be orders of
magnitude slower than warmed cache. Benchmark *your* rules and inputs with
`DisablePrefilter` and `DisableCandidateShards` toggles. These figures are
short-run exploratory numbers; they should not be used as cross-machine ratios.
[Benchmark methodology](docs/BENCHMARKS.md) lists reproducibility caveats.

## Validation and limitations

```sh
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
go test -fuzz '^FuzzMatchesRegexp$' -fuzztime=30s .
go test -fuzz '^FuzzRuleSetAgainstRegexp$' -fuzztime=30s .
```

Not yet implemented: exact process-heap/RSS caps, O(1) LRU eviction, SIMD,
full Unicode folding-aware equivalence classes, fully general shared regex
prefixes, incremental streaming, and nested/field-aware logical expressions.
Do not interpret the presence of a feature as proof it is faster for all
workloads. For exact/suffix hostnames, a hash table or reversed-domain trie
can be simpler and faster than a general regex engine.
