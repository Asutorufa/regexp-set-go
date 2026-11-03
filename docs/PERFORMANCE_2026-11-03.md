# Candidate scans and shared field predicates

This change optimizes the library only. No application integration is included.
It preserves original Set pattern indexes, flat RuleSet AND multiplicity,
rule priority, and missing-versus-empty field semantics.

## Changes

- Combine AC group-mask and sparse candidate-ID discovery in one pass for Set
  and any/all RuleSet queries. Stop collecting candidate IDs after the sparse
  verification threshold is exceeded, while still computing the full group mask.
  Priority proofs continue to collect all required candidate IDs.
- Use the standard library's optimized byte search to reject short inputs that
  lack the common first byte of every mandatory literal. Negative scans avoid
  acquiring candidate scratch from the pool.
- Share identical `(field, pattern)` predicates across FieldRuleSet leaves.
  Mark Boolean membership directly instead of materializing and sorting IDs.
  PatternCount still counts original leaves, including duplicates.
- Share identical candidate regexp verifiers, omit the unreachable verifier
  path for fewer than eight eligible patterns, and release compile-only NFA
  duplicate-program lookup after construction.
- Expose aggregate FieldRuleSet.CacheStats, with unchanged per-field budgets.
- Make BenchmarkCacheBudget explicitly disable exact candidates in its DFA
  cases and assert that the fixture actually populated the DFA cache.

## Measurement

Baseline: `6ec6bfa566a06d7deaf820e24be614cfc5991a7c`. The new benchmark file
was copied unchanged into an archive of that commit so both versions ran the
same fixtures. Measurements ran sequentially on one Linux/amd64 Ryzen 5 5600G,
Go `go1.27.3-X:nodwarf5`, GOMAXPROCS=1, `-benchtime=300ms -count=5`.
Tables report sample medians, not confidence intervals. Caches were warmed.
The machine was not CPU-pinned or reserved exclusively for this benchmark.

These are synthetic corpora, not production traffic, end-to-end application
latency, or comparisons against other multi-regex engines.

| Benchmark | Before ns/op | After ns/op | Change |
|---|---:|---:|---:|
| 1,000 patterns, 50% hit, MatchAny | 867.3 | 471.9 | -45.6% |
| 1,000 patterns, negative input without common root byte | 356.5 | 12.43 | -96.5% |
| FieldRuleSet, 500 repeated nested rules | 11,413 | 1,197 | -89.5% |
| DuplicateFields, 500 repeated rules | 11,435 | 1,229 | -89.3% |
| DuplicateFields, 2,000 repeated rules | 40,553 | 1,217 | -97.0% |
| DenseCandidates, 2,048 necessary-literal candidates | 6,176 | 1,209 | -80.4% |
| ShortPrefilter, root byte absent | 243.6 | 12.93 | -94.7% |
| ShortPrefilter, root byte present but no match | 263.0 | 295.7 | +12.4% |
| ShortPrefilter, one positive match | 1,192 | 737.3 | -38.1% |

Matching in these samples rounds to zero allocations/op. The old FieldRuleSet
fixture reported 2 B/op from amortized cold work despite zero rounded allocs/op;
the other rows reported 0 B/op before and after.

The very cheap root-byte rejection applies only when every mandatory literal
starts with one common byte and that byte is absent from the input. It does
not apply to all negative traffic. The root-present counterexample is retained
deliberately: collecting candidates in the combined scanner adds work when no
candidate is ultimately found. RuleSet priority queries with common unfiltered
predicates also remain workload-dependent; this change does not promise fast
first-rule selection for every rule distribution.

## Compilation and memory

| Default compilation | Before | After |
|---|---:|---:|
| 7 eligible patterns | 175.3 us | 146.2 us |
| 7 patterns, allocation bytes/op | 137,680 | 102,616 |
| 7 patterns, allocations/op | 862 | 602 |
| 1,000 patterns | 32.65 ms | 33.89 ms |
| 1,000 patterns, allocation bytes/op | 22,973,120 | 23,081,880 |
| 10,000 patterns | 334.19 ms | 332.78 ms |
| 10,000 patterns, allocation bytes/op | 239,331,040 | 240,204,328 |

Small-set compilation benefits from removing unused verifiers. Large distinct
sets do not show a consistent compilation speedup; the verifier-sharing map
also adds some temporary allocation. At 10,000 patterns each sample contains
only one or two compilations, so small time differences are inconclusive.

A separate single-run GC/HeapAlloc probe measured retained compiled heap at
6,556,456 -> 6,377,944 bytes for 1,000 patterns and 65,772,272 -> 64,184,512 bytes
for 10,000 patterns. This is a modest reduction from releasing compile-only
lookup, not a total-memory bound. It is neither peak RSS nor allocated bytes/op.
The default sparse-positive probe retained zero DFA states while still holding
roughly 61 MiB of compiled objects at 10,000 patterns. CacheStats excludes
compiled NFA/AC/verifiers, scratch, map overhead and process-level memory.

## Reproduce

Validation completed with Go 1.23.12 tests and vet; Go 1.27.3 race tests,
purego tests and vet; 30 seconds of fuzzing each for Set and RuleSet; and
test-binary cross-compilation for Linux/arm64, Darwin/amd64, Darwin/arm64 and
Windows/amd64. Cache-pressure fixtures were also smoke-tested separately.

```sh
GOMAXPROCS=1 go test -run '^$' \
  -bench '^(BenchmarkMatch|BenchmarkExactCandidates|BenchmarkFieldRuleSet|BenchmarkDuplicateFields|BenchmarkDenseCandidates|BenchmarkCompileCandidateModes|BenchmarkShortPrefilter)$' \
  -benchmem -benchtime=300ms -count=5 .

# A separate directory keeps the original engine unchanged. Use the same
# benchmark file for the baseline and the optimized engine.
baseline_dir=$(mktemp -d)
git archive 6ec6bfa566a06d7deaf820e24be614cfc5991a7c | tar -x -C "$baseline_dir"
cp matching_bench_test.go "$baseline_dir/"
cd "$baseline_dir"
GOMAXPROCS=1 go test -run '^$' \
  -bench '^(BenchmarkMatch|BenchmarkExactCandidates|BenchmarkFieldRuleSet|BenchmarkDuplicateFields|BenchmarkDenseCandidates|BenchmarkCompileCandidateModes|BenchmarkShortPrefilter)$' \
  -benchmem -benchtime=300ms -count=5 .
```

Cache-pressure measurements should use the repaired BenchmarkCacheBudget,
not the old fixture whose exact-candidate path bypassed the DFA cache:

```sh
GOMAXPROCS=1 go test -run '^$' -bench '^BenchmarkCacheBudget$' -benchmem .
go test -run '^$' -bench '^BenchmarkParallel$' -benchmem -cpu=1,4,8 .
```
