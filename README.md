# regexp-set-go

[![CI](https://github.com/Asutorufa/regexp-set-go/actions/workflows/ci.yml/badge.svg)](https://github.com/Asutorufa/regexp-set-go/actions/workflows/ci.yml)

Pure Go, no CGO, no third-party dependencies: a multi-pattern
Go/RE2 regular-expression **existence** matcher returning original rule IDs.

## Features

- Compile multiple regexes into grouped Thompson NFAs using `regexp/syntax`.
- Lazily cache DFA states and transitions, with ASCII character equivalence classes.
- Use a sparse Aho-Corasick **conservative rejection filter** when a required
  ASCII literal can safely be extracted. Never drop rules that lack such a literal.
- Retain original zero-based rule IDs, including duplicated pattern strings.
- Match substrings, Unicode, zero-width assertions and malformed UTF-8 with
  Go `regexp` semantics.
- Thread-safe matching via reusable per-call scratch storage.
- Limit global retained state/transition **counts** and continue correctly
  when the cache is full. Optional generation reset, disabled by default.

## Usage

```go
package main

import (
    "fmt"
    regexpset "github.com/Asutorufa/regexp-set-go"
)

func main() {
    set, err := regexpset.Compile([]string{
        `(?i)ads?\.[a-z]+`,
        `^tracker\d+\.example\.com$`,
        `\bmalware\b`,
    })
    if err != nil {
        panic(err)
    }
    fmt.Println(set.MatchAny("tracker123.example.com")) // true
    fmt.Println(set.MatchIDs("tracker123.example.com")) // [1]

    dst := make([]int, 0, 8)
    dst = set.MatchInto("tracker123.example.com", dst)
    fmt.Println(dst) // [1]
}
```

Custom cache policy:

```go
set, err := regexpset.CompileWithOptions(patterns, regexpset.Options{
    MaxStates: 8192,
    MaxTransitions: 131072,
    DisablePrefilter: false,
    ResetCacheOnFull: false, // optional; profile before enabling
})
if err != nil {
    // Syntax errors are annotated with their pattern index.
}
_ = set.CacheStats()
```

The default combined budget is 4,096 cached states and 65,536 transitions.
Zero means default for public options; it does not mean disable caching.
`CacheStats` reports entries and ASCII class counts, **not actual memory bytes**.

## Limits and semantics

This library reports which expressions match *somewhere* in each complete
input string, matching `regexp.Regexp.MatchString` for each rule.
It does not report capture groups, span positions or priority order,
and does not perform incremental streaming matching. Lookbehind,
lookahead, backreferences and other unsupported PCRE syntax are not available.

The shared NFA currently concatenates per-regex programs; it does not factor
all common prefixes at compile time. A positive prefilter result still checks
the whole eligible group. High-hit input may be **slower** than the unfiltered
engine. Cache count limits do not bound total memory consumption.

### Performance caveats

This is a developing engine, **not a proven replacement for Hyperscan**.
On a synthetic warm-cache test with 1,000 anchored domain patterns,
a prefilter improved negative matches but slowed mixed-hit matches.
With only a handful of cached states, fallback or repeated resets can be
hundreds of times slower. See [benchmark methodology](docs/BENCHMARKS.md)
and [architecture](docs/ARCHITECTURE.md) before selecting a cache policy.

For ordinary exact/suffix-domain matching, a hash map or domain trie may
still be preferable to a general regex engine.

## Development

```bash
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
go test -run '^$' -bench 'BenchmarkMatch|BenchmarkPrefilter|BenchmarkCacheBudget' -benchmem ./...
go test -run '^$' -bench 'BenchmarkParallel|BenchmarkMatchIDsOracle' -benchmem ./...
go test -fuzz '^FuzzMatchesRegexp$' -fuzztime=30s .
```

Tests compare rule IDs against Go's standard regexp engine, including
generated pattern corpora, random inputs, Unicode, invalid UTF-8, AC overlap,
tiny-cache fallback, optional cache reset and concurrent matching.
