# regexp-set-go

[![CI](https://github.com/Asutorufa/regexp-set-go/actions/workflows/ci.yml/badge.svg)](https://github.com/Asutorufa/regexp-set-go/actions/workflows/ci.yml)

A **Pure Go multi-pattern regular-expression matcher** for Go/RE2 syntax. It uses `regexp/syntax`, shared Thompson NFAs, and lazily cached DFA states to find which patterns match a string. **No CGO and no third-party dependencies.** Requires Go 1.23+.

There are two public matching layers:

| API | Purpose | Result |
| --- | --- | --- |
| `Compile` / `Set` | Match many individual regular expressions | Zero-based **pattern indexes** (`[]int`) |
| `CompileRules` / `RuleSet` | Group expressions into priority-ordered logical rules | Application-defined **rule IDs** (`[]uint32`), with AND/OR |

Both layers use the same matching engine; `RuleSet` **does not compile one automaton per rule**.

## Key features

- Compile multiple Go/RE2-compatible regular expressions into shared Thompson NFA instruction arrays.
- Lazily create and reuse DFA states and transitions; merge equivalent ASCII character classes.
- Reject clear non-matches with a **conservative** Aho-Corasick required-literal prefilter. Expressions without a provably required literal are never excluded.
- Return **all matching pattern indexes**, or evaluate **logical rules with AND/OR and explicit priority**.
- Match anywhere in a complete input string; handle Unicode, zero-width assertions, and malformed UTF-8 consistently with Go's `regexp.Regexp.MatchString` semantics.
- Reuse caller-supplied result slices and internal `sync.Pool` scratch buffers; compiled matchers support concurrent reads/matches.
- Apply shared cached-state/transition **count** limits; fall back to uncached matching when full. An optional whole-generation reset is available but **disabled by default**.

## Installation

```sh
go get github.com/Asutorufa/regexp-set-go
```

## RuleSet: multiple regexes, multiple priority-ordered rules

Choose `RuleSet` when several patterns belong to the same routing, filtering, or classification rule, and different rules can overlap.

```go
package main

import (
    "fmt"

    regexpset "github.com/Asutorufa/regexp-set-go"
)

func main() {
    rules := []regexpset.Rule{
        {
            ID:       100,
            Operator: regexpset.RuleAND,
            Patterns: []string{`(?i)google`, `\.com$`},
        },
        {
            ID:       50,
            Operator: regexpset.RuleOR,
            Patterns: []string{`(?i)github`, `(?i)google`},
        },
        {
            ID:       300,
            // RuleOR is the default if Operator is omitted.
            Patterns: []string{`(?i)doubleclick`},
        },
    }

    set, err := regexpset.CompileRules(rules)
    if err != nil {
        panic(err)
    }

    id, ok := set.MatchFirstRule("www.google.com")
    fmt.Println(id, ok) // 100 true

    fmt.Println(set.MatchRuleIDs("www.google.com")) // [100 50]
    fmt.Println(set.MatchRuleIDs("github.com/doubleclick")) // [50 300]

    dst := make([]uint32, 0, 8)
    dst = set.MatchRuleIDsInto("www.google.com", dst)
    fmt.Println(dst) // [100 50]
}
```

### Rule semantics and priority

- **Priority is input order:** `rules[0]` has the highest priority. It is **not** the numeric `Rule.ID` or the order in which the DFA discovers a match.
- **`RuleOR` (default):** at least one regex in that rule must match.
- **`RuleAND`:** every regex in that rule must independently match **somewhere in the same input string**. They do not have to match the same substring or adjacent spans.
- Different rules can use different operators. `MatchRuleIDs` returns all satisfied IDs in **priority order**, each at most once. `MatchFirstRule` returns the highest-priority satisfied ID and a boolean (IDs may legitimately be zero).
- Pattern text may be repeated within or across rules; each occurrence counts as an independent pattern. **Rule IDs must be unique**. A rule must contain at least one pattern; unknown operators are rejected.
- `MatchFirstRule` determines the winning rule **after evaluating matched pattern identities**. It does not stop at the first substring match; doing so would break AND semantics and priority.
- Grouping is **flat**, one AND or OR operator per rule. Expressions such as `(A OR B) AND (C OR D)`, conditions on different fields (host versus path), actions, and routing are application-level concerns, not built-in `RuleSet` operations.

`RuleSet` also exposes `MatchAny`, `MatchRuleIDsInto`, `Len`, `PatternCount`, `CacheStats`, and `CompileRulesWithOptions`.

For validation details and additional examples, see [RuleSet documentation](docs/RULESET.md).

## RegexSet: original pattern-level API

Use `Set` when callers need zero-based regex indexes rather than logical rule IDs:

```go
patterns := []string{
    `(?i)ads?\.[a-z]+`,
    `^tracker\d+\.example\.com$`,
    `\bmalware\b`,
}
set, err := regexpset.Compile(patterns)
if err != nil {
    panic(err)
}
fmt.Println(set.MatchAny("tracker123.example.com")) // true
fmt.Println(set.MatchIDs("tracker123.example.com"))  // [1]

dst := make([]int, 0, 8)
dst = set.MatchInto("tracker123.example.com", dst)
fmt.Println(dst) // [1]
```

`Set.MatchIDs` returns sorted original pattern indexes, including distinct indexes for duplicate patterns. `MatchInto` lets callers reuse a result buffer. Neither layer extracts captures or match spans.

## Cache configuration

The same `Options` apply to `CompileWithOptions` and `CompileRulesWithOptions`:

```go
opts := regexpset.Options{
    MaxStates:        8192,
    MaxTransitions:   131072,
    DisablePrefilter: false,
    ResetCacheOnFull: false, // optional: profile carefully before enabling
}

set, err := regexpset.CompileRulesWithOptions(rules, opts)
if err != nil {
    panic(err)
}
fmt.Println(set.CacheStats())
```

| Setting | Default | Meaning |
| --- | ---: | --- |
| `MaxStates` | 4,096 | Maximum retained DFA states across the matching groups |
| `MaxTransitions` | 65,536 | Maximum retained cached transitions |
| `DisablePrefilter` | `false` | Turn off conservative mandatory-literal rejection |
| `ResetCacheOnFull` | `false` | Opt into whole-generation cache recycling instead of normal uncached fallback |

A zero limit selects its default; negative limits fail compilation. Cache limits count retained entries, **not bytes**, and do **not** cap compiled regex instructions, Aho-Corasick data, concurrent scratch, or transient allocations. Very small caches can cause major slowdowns; a generation reset is not an LRU cache.

## Benchmarks

This repository includes benchmarks for **both `Set` and `RuleSet`**. The results depend heavily on whether the input matches, the composition of patterns, DFA cache size, and whether all rule IDs must be returned.

### What the benchmarks measure

| Benchmark | Rules / workload | What it compares |
| --- | --- | --- |
| `BenchmarkMatch` | 10 / 100 / 1,000 regexes; no-hit and mixed inputs | `MatchAny`, reusable `MatchInto`, and early-exit loop over precompiled Go `regexp` |
| `BenchmarkMatchIDsOracle` | 1,000 regexes | Return **all** matching indexes versus checking every Go `regexp` |
| `BenchmarkRuleSet` | 100 / 1,000 / 5,000 logical rules, **3 regexes each**, alternating AND/OR | First matching rule, all matching rules, and separate-regexp **all-rule** baseline (baseline provided through 1,000 rules) |
| `BenchmarkRuleSetConcurrent` | 500 logical rules | Parallel `MatchFirstRule` with a shared compiled `RuleSet` |
| `BenchmarkRuleSetTinyCache` | 250 logical rules | RuleSet latency and allocations with only 2 cached states/transitions |
| `BenchmarkPrefilter` | 1,000 / 10,000 regexes | Mandatory-literal AC prefilter enabled vs disabled, no-hit vs mixed |
| `BenchmarkCacheBudget` | 1,000 regexes | Default cache, tiny uncached fallback, tiny generation reset |
| `BenchmarkParallel` | 1,000 regexes | Shared-cache throughput under concurrent `MatchAny` |
| `BenchmarkCompile` | 10 / 100 / 1,000 regexes | Compilation time and allocations, **separate from match timings** |

`BenchmarkRuleSet`'s synthetic positive sample primarily exercises an OR-rule hit; its AND groups consist of mutually exclusive anchored alternatives. Use a real corpus that has **satisfied AND groups**, overlapping rule matches, and mixed priorities before concluding anything about production routing performance. Compare equivalent work: the separate-regexp **all-rule** baseline is comparable to the all-results operation, **not** directly to `MatchFirstRule`.

### Run the benchmarks

```sh
# RuleSet: priority, AND/OR grouping, allocations, and conventional regexp baseline
go test -run '^$' -bench '^BenchmarkRuleSet$' -benchmem -benchtime=1s -count=5 ./...

# RuleSet concurrency and pathological tiny cache
go test -run '^$' -bench '^BenchmarkRuleSetConcurrent$' -benchmem -cpu=1,2,4,8 ./...
go test -run '^$' -bench '^BenchmarkRuleSetTinyCache$' -benchmem ./...

# Individual regex existence/all-IDs and compile-time costs
go test -run '^$' -bench '^(BenchmarkMatch|BenchmarkMatchIDsOracle|BenchmarkCompile)$' -benchmem ./...

# Prefilter and cache behavior
go test -run '^$' -bench '^(BenchmarkPrefilter|BenchmarkCacheBudget)$' -benchmem ./...
```

Report **ns/op**, **B/op**, and **allocs/op** together. Matching benchmarks compile expressions outside the timed section and typically **warm the DFA cache**. The input sets are synthetic and often reuse 128 short strings; distinguish throughput on these hot inputs from cold cache, real routing traffic, or large unique input sets. For reliable comparisons, use the same machine and Go version, vary `-cpu`, and capture memory/RSS separately.

### Existing measurement snapshot (illustrative, **not** a performance guarantee)

The figures below are recorded in [benchmark methodology](docs/BENCHMARKS.md) from **October 19, 2026**, on a shared Linux/amd64 AMD EPYC 9V74 machine with **Go 1.23.2**, using `-benchtime=100ms`, synthetic **warm-cache** workloads:

| Workload | Prefilter off | Prefilter on |
| --- | ---: | ---: |
| 1,000 regexes, **no match** | 652 ns/op | 489 ns/op |
| 1,000 regexes, **50% hits** | 712 ns/op | 841 ns/op |
| 10,000 regexes, **no match** | 659 ns/op | 429 ns/op |

In another 1,000-regex mixed-input run, a normal warm cache measured **790 ns/op**, versus **527,798 ns/op** with tiny uncached fallback and **691,716 ns/op** with repeated tiny-cache resets. Those cases use different cache policies; do not read them as comparable RuleSet numbers or general speedups.

**Interpretation:** the prefilter can help workloads with many non-matches but adds cost when it frequently passes through to the matcher. Cache starvation can erase the benefit of lazy determinization. These samples do **not** prove an advantage over Hyperscan, Rust RegexSet, optimized domain tries, or even another workload on Go's standard library. The RuleSet benchmarks are provided for **local reproduction**, not accompanied here by an unverified published RuleSet performance figure.

For fuller methodology and limitations, see [docs/BENCHMARKS.md](docs/BENCHMARKS.md) and [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Correctness, limitations, and development

The engine implements **existence matching for complete strings**, not capture extraction, match positions, or streaming across chunks. Go's supported regexp syntax does not include PCRE lookbehind, backreferences, or arbitrary lookaround. Untrusted expression sets still need application-level resource limits; a combined lazy DFA can have expensive cold-cache behavior.

For exact or suffix-only domain rules, a hash map or reversed domain trie can be simpler and faster than general regex matching.

```sh
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...

# Fuzz targets are run separately; Go allows one active -fuzz target per run.
go test -fuzz '^FuzzMatchesRegexp$' -fuzztime=30s .
go test -fuzz '^FuzzRuleSetAgainstRegexp$' -fuzztime=30s .
```

Tests cover Go `regexp` differential checks, invalid UTF-8, Unicode, word/line boundaries, duplicate patterns, AND/OR priority, conservative prefilter behavior, tiny-cache fallback, optional cache reset, and concurrent use. CI runs tests with race detection, vet, cross-compilation, and a benchmark smoke test; smoke tests are **not** stable performance measurements.

## Latest engine optimizations

See [OPTIMIZATIONS.md](docs/OPTIMIZATIONS.md) for a code-level explanation of all eight bottlenecks and the three development stages.

- AC prefilter outputs candidate **machine-group masks**; shard selection is conservative, not exact per-pattern filtering.
- Start-state epsilon closures are memoized; common prefixes of safe literal patterns share a compiled NFA trie.
- DFA interning uses 32 hash shards, atomic generation replacement, adaptive sparse/bitset frontiers and conservative Unicode range equivalence.
- RuleSet aggregates matching pattern IDs as they arrive. `MatchAny` stops when any rule is satisfied; `MatchFirstRule` can stop early only when the highest-priority rule is satisfied.
- `Options.MaxCacheBytes` limits estimated DFA state/edge reservations, **not** total heap/RSS.
- `Options.DisableCandidateShards` disables AC-driven splitting, which may help hit-heavy workloads with high-cardinality inputs.

### Optimization benchmarks

```sh
go test -run '^$' -bench 'BenchmarkCandidateShardsHighCardinality|BenchmarkRuleSetPriorityStreaming|BenchmarkLiteralTrieOracles' -benchmem ./...
```

**Synthetic measurement (Linux/amd64, Go 1.23.2):** 4,096 anchored hostname regexes and 2,048 varied inputs (50% hits) showed **~2,271 ns/op** with candidate sharding versus **~1,658 ns/op** unfiltered, a measured regression caused by cache partitioning. A separate 1,024-rule repeated anchored-literal test measured **~559 ns/op** for the shared-trie DFA versus **~12,991 ns/op** for an early-exit loop over compiled Go regexes. These different, short, warm-cache benchmarks are not universal performance claims.
