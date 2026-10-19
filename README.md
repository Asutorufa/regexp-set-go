# regexp-set-go

A dependency-free **Pure Go multi-pattern regular-expression matcher**, built
on `regexp/syntax`. It returns the IDs of all expressions matching a string.

## Design

- Compiles Go/RE2-compatible expressions into one combined Thompson NFA.
- Lazily constructs and shares deterministic states, using an NFA-frontier
  cache with transition memoization; identical frontiers share a state.
- Handles Go's zero-width assertions (`^`, `$`, `\A`, `\z`, `\b`, `\B`),
  Unicode and invalid UTF-8 by evaluating assertions at rune boundaries.
- Uses a fixed-size **number of retained states and edges**. On cache
  exhaustion, computes uncached frontiers instead of changing match semantics.
- Thread-safe after compilation. No CGO and no third-party dependencies.

This library reports *whether* each expression matches somewhere, **not**
match locations, capture groups, leftmost-longest ordering, or streaming across
input chunks. It accepts the same regexp syntax as Go's `regexp` package
(including its restrictions, such as no lookarounds or backreferences).

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
    if err != nil { panic(err) }
    fmt.Println(set.MatchAny("tracker123.example.com")) // true
    fmt.Println(set.MatchIDs("tracker123.example.com")) // [1]
}
```

For latency-sensitive code, `MatchInto(text, dst)` reuses the caller's result
buffer. Matching and state caching can safely be shared between goroutines.

```go
set, err := regexpset.CompileWithOptions(patterns, regexpset.Options{
    MaxStates: 8192,
    MaxTransitions: 131072,
})
if err != nil { panic(err) }
_ = set.CacheStats()
```

`Options` zeros use defaults of 4,096 states and 65,536 transitions. These
limits bound retained *state/transition counts*, **not bytes**. Temporary
uncached states and runtime scratch buffers still allocate memory. Large
pattern sets, many simultaneous callers, or complex expressions can require
substantial additional memory. `Compile` does not enforce an input pattern
length or memory quota; validate untrusted patterns separately.

## Validation

```sh
go test ./...
go test -race ./...
go vet ./...
go test -run '^$' -bench 'BenchmarkMatch' -benchmem ./...
go test -run '^$' -bench 'BenchmarkCompile' -benchmem ./...
go test -fuzz '^FuzzMatchesRegexp$' -fuzztime 30s .
```

Tests use `regexp.Regexp.MatchString` as the differential oracle, including
Unicode, invalid UTF-8, anchors, boundaries, empty patterns, duplicates,
randomized inputs, bounded-cache fallback, concurrency and 2,000 patterns.
Benchmarks compare steady-state `MatchAny`, `MatchInto`, and a sequential loop
of precompiled `regexp.Regexp` objects for 10/100/1,000 rules. Results depend
on rule shape and input distribution; expect shared DFA caching to help most
when repeated inputs visit the same states. Cold caches, tiny caches, or many
unrelated rules may be slower. Run benchmarks on your real rule corpus before
choosing this matcher over Go's standard `regexp`.

## Limitations

The engine is a regex **set** with existence semantics. It does not select a
winning routing rule. When routing rules have priority, inspect `MatchIDs`
and apply your own precedence policy. Inputs are matched at Unicode rune
boundaries, like Go regexp, rather than at arbitrary byte offsets.