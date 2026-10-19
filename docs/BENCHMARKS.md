# Benchmarks and methodology

This repository includes benchmarks for more than the flattering warm-cache,
no-hit case. They measure CPU time (ns/op), allocations (B/op), compile cost,
cache limits and shared-cache concurrency.

## Commands

```bash
go test -run '^$' -bench '^BenchmarkMatch$' -benchmem -benchtime=1s -count=5 ./...
go test -run '^$' -bench '^BenchmarkPrefilter$' -benchmem -benchtime=1s -count=5 ./...
go test -run '^$' -bench '^BenchmarkCacheBudget$' -benchmem -benchtime=1s ./...
go test -run '^$' -bench '^BenchmarkParallel$' -benchmem -cpu=1,2,4,8 ./...
go test -run '^$' -bench '^BenchmarkMatchIDsOracle$' -benchmem ./...
go test -run '^$' -bench '^BenchmarkCompile$' -benchmem ./...
```

## Reproducible preliminary run

Environment: Linux/amd64, Go 1.23.2, AMD EPYC 9V74 (shared runner),
`-benchtime=100ms`, run on 2026-10-19. These are **illustrative**, not a
cross-machine comparison or proof of superiority against Hyperscan/Rust RegexSet.

| Workload | Result | Allocations |
|---|---:|---:|
| 1,000 rules, miss, prefilter disabled | 634 ns/op | 0 allocs/op |
| 1,000 rules, miss, prefilter enabled | 466 ns/op | 0 allocs/op |
| 1,000 rules, mixed, prefilter disabled | 730 ns/op | 0 allocs/op |
| 1,000 rules, mixed, prefilter enabled | 828 ns/op | 0 allocs/op |
| 10,000 rules, miss, prefilter disabled | 672 ns/op | 0 allocs/op |
| 10,000 rules, miss, prefilter enabled | 409 ns/op | 0 allocs/op |
| 10,000 rules, mixed, prefilter enabled | 903 ns/op | 0 allocs/op |
| 1,000 rules, mixed, 4-state fallback cache | 458,000 ns/op | 28 allocs/op |
| 1,000 rules, mixed, 4-state reset cache | 768,000 ns/op | 90 allocs/op |

These measurements use **synthetic host patterns** with long shared suffixes,
128 repeated inputs and a warmed cache. They are not a representative
production workload. Reported B/op may be nonzero while rounded allocs/op is
zero. The `regexp-loop` baseline stops at the first match when measuring
`MatchAny`; a separate benchmark traverses *all* rules for IDs. The baseline
is **not** another multi-regex engine and is not a fair stand-in for Hyperscan.

The large slowdown with a tiny cache is a critical warning: state count limits
can drastically change CPU requirements. `ResetCacheOnFull` is not an automatic
performance improvement.

## Real-corpus evaluation checklist

- **Rule shapes:** short/long literals, case-insensitive rules, Unicode,
  alternations, Unicode character classes, nested repetitions, anchors,
  expressions with no extractable required literal.
- **Inputs:** 0/1/50/100% hit rates, short domain strings, long URLs, varying
  Unicode content, invalid UTF-8, random bytes, real application traffic.
- **Cache:** first request after compile (cold), warmed steady state, budgets
  of 1/64/4,096 states, repeated distribution changes and cache resets.
- **Throughput:** sequential, 1/2/4/8+ goroutines, realistic key skew, tail
  latency (p95/p99) and contention profiles.
- **Memory:** compiled NFA and AC size, retained cache, in-flight scratch
  allocations per goroutine and peak RSS. `CacheStats` only counts entries.
- **Compilation:** 10/100/1,000/10,000+ rules, including invalid patterns.
- **Semantics:** oracle-match all IDs using compiled `regexp.Regexp`, including
  zero-width boundaries, case folding, Unicode and malformed input.

Always benchmark the alternative algorithms on **the same rule corpus**. For
simple exact/suffix-domain routing, a hash table or domain trie can beat a
regex engine entirely.

## Optimization branch exploratory numbers (2026-10-19)

Same Linux/amd64 shared EPYC Go 1.23.2 environment. These are **short
100–200 ms samples**, not stabilized `-count=5` results. Callers should always
repeat on their own rules and traffic; non-matches and positive cardinality
produce very different cache behavior.

| Fixture | Variant | ns/op | B/op | allocs/op |
| --- | --- | ---: | ---: | ---: |
| 4,096 anchored hosts / 2,048 inputs / 50% hits | Candidate AC shards | 2,349 | 664 | 10 |
| Same | Single prefiltered machine | 2,344 | 584 | 9 |
| Same | Unfiltered shared machine | 1,690 | 581 | 9 |
| 1,000 deterministic literal-prefix + regex-suffix | Shared NFA warm hit | 487 | 0 | 0 |
| Same | Sequential `regexp` early-exit | 17,144 | 0 | 0 |
| 1,024 rule groups, higher-priority group provably impossible | First eligible priority | 20,518 | 3 | 0 |
| Same | Return all satisfied rules | 46,096 | 7 | 0 |

The candidate-shard counterexample is intentional: adding an AC scan and
partitioning the DFA cache do not necessarily beat one combined DFA on
high-cardinality positive input. The first-match and all-match operations
perform different amounts of work, so their ratio is **not** a general
regex-engine speedup.

```sh
go test -run '^$' -bench '^BenchmarkCandidateShardsHighCardinality$' -benchtime=1s -count=5 -benchmem .
go test -run '^$' -bench '^BenchmarkGeneralPrefixFactoring$' -benchtime=1s -count=5 -benchmem .
go test -run '^$' -bench '^BenchmarkRuleSetCandidatePriorityPruning$' -benchtime=1s -count=5 -benchmem .
```

## RuleSet with satisfied AND rules

The optimization suite includes a fair, precompiled sequential-regexp baseline
which stops at the **first satisfied logical rule**, matching
`RuleSet.MatchFirstRule` behavior. Run:

```sh
go test -run '^$' -bench '^BenchmarkRuleSetSatisfiedAND$' -benchmem -benchtime=1s -count=5 .
```

In the 100ms smoke run on Linux/amd64 Go 1.23.2, the shared RuleSet was slower
for an early successful AND rule (about 10.9 µs vs 0.58 µs), but faster for a
late rule (10.0 µs vs 17.6 µs) and a miss (0.71 µs vs 2.2 µs). These are
synthetic samples rather than a universal result; see [OPTIMIZATIONS.md](OPTIMIZATIONS.md).
