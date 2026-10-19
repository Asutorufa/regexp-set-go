# Benchmark methodology

Re-run with:

```bash
go test -run '^$' -bench '^BenchmarkMatch$' -benchmem -benchtime=1s -count=5 ./...
go test -run '^$' -bench '^BenchmarkPrefilter$' -benchmem -benchtime=1s -count=5 ./...
go test -run '^$' -bench '^BenchmarkCacheBudget$' -benchmem ./...
go test -run '^$' -bench '^BenchmarkParallel$' -benchmem -cpu=1,2,4,8 ./...
go test -run '^$' -bench '^BenchmarkMatchIDsOracle$' -benchmem ./...
go test -run '^$' -bench '^BenchmarkCompile$' -benchmem ./...
```

The supplied microbenchmarks use 10, 100, 1,000 or 10,000 synthetic anchored
host patterns. The matching corpus has 128 repeated strings, split between
no-hit and 50% hit workloads. Match benchmarks precompile patterns **before**
timing, then warm the caches. Compilation has its own benchmark.

Preliminary Linux/amd64 results, Go 1.23.2 on a shared AMD EPYC 9V74 machine,
with `-benchtime=100ms` on October 19, 2026:

| Workload | ns/op (illustrative) |
| --- | ---: |
| 1,000 rules, miss, prefilter disabled | 652 |
| 1,000 rules, miss, prefilter enabled | 489 |
| 1,000 rules, 50% hit, prefilter disabled | 712 |
| 1,000 rules, 50% hit, prefilter enabled | 841 |
| 10,000 rules, miss, prefilter disabled | 659 |
| 10,000 rules, miss, prefilter enabled | 429 |
| 1,000 rules, mixed, normal cache | 790 |
| 1,000 rules, mixed, tiny fallback cache | 527,798 |
| 1,000 rules, mixed, tiny reset cache | 691,716 |

These numbers are sensitive to machine load, input skew, cache warmup and
rule composition. They do **not** establish superiority over Rust RegexSet,
Hyperscan, or a domain trie. A sequential loop over Go `regexp`
is a useful baseline but is not a competitive multi-regex matcher.

Benchmark both **existence** (stop after the first hit) and **all rule IDs**
(each rule checked) against identical inputs. Report ns/op, B/op and allocs/op.
Test cold start, hot cache, heavy cache churn, different rule shapes, Unicode,
realistic malformed input, concurrent goroutines and application memory/RSS.

Particularly important: with tiny caches, lazy-state recomputation can be
*hundreds of times* slower than the warmed cache. Do not blindly enable cache
resets or assume a state-count budget constrains memory usage.
