# Engine architecture

## Correctness and scope

This is a multi-rule **existence** matcher for Go/RE2 regular expressions, with
the same `regexp.Regexp.MatchString` semantics for each rule.
It is not a streaming matcher, a submatch extractor, a priority router or a
replacement for extended PCRE features.

Invariants:

1. Every pattern retains its original zero-based ID, including duplicates.
2. The prefilter is conservative: no input can be rejected if a filtered
   pattern would match it. False positives only cost CPU.
3. New NFA starts are included at **every Unicode rune boundary**; assertions
   inspect the previous and next runes using `syntax.EmptyOpContext`.
4. Interned DFA states own their memory. Match-local uncached states can borrow
   scratch arrays only until the next transition consumes them.
5. State edge caches are synchronized. A generation reset does not mutate
   states already held by concurrent readers.
6. Cache limits span all machine groups, but count entries, **not bytes**.

## Compilation and dispatch

```text
patterns
    |
    +--- regexp/syntax.Parse + Simplify (preserve ID)
    |
    +--- conservative mandatory ASCII substring extraction
            |
            +--- no required literal --- Thompson NFA + lazy DFA
            |
            +--- safe required literal --- sparse AC presence filter
                                            |
                                            +--- on possible hit --- NFA + DFA
```

The matcher intentionally splits patterns into filterable and unfilterable
groups. Rules such as `.*`, `a?`, and case-folded literals
cannot suppress the rejection fast path of unrelated patterns.

The AC filter reports only whether **any** required literal occurs. It does
not identify which specific rule matches, so a positive lookup still runs
the entire filtered NFA/DFA. This favors negative-match workloads and may hurt
high-hit workloads.

## NFA and lazy DFA

Every parsed expression is compiled through `regexp/syntax.Compile`,
with instruction indices translated into one combined instruction array.
Accepting instructions carry original pattern IDs. Captures become epsilon
transitions because the API returns rule existence, not submatch spans.

The epsilon closure uses generation-stamped arrays to avoid repeated clearing,
collects consuming instruction pointers and accepted IDs, then sorts to obtain
a canonical deterministic frontier.

On a transition cache miss, all consuming instructions of the current frontier
are evaluated against the rune and the result is closed again. A 64-bit
hash indexes canonical states; equality is checked to handle collisions.

## Equivalence classes

ASCII code points are grouped into equivalence classes when they have exactly
the same acceptance vector over all consuming NFA instructions.
`(classID, emptyWidthContext)` is the transition cache key. This
keeps `\b`, `^`, case-folding and character classes correct
as the empty-width context is independent of the consuming code point.
Non-ASCII runes use their actual values, including U+FFFD for malformed UTF-8.

A future optimization is to construct Unicode equivalence ranges and denser
transition storage, instead of one hash-table entry per distinct rune.

## Cache concurrency and capacity

`Set` is safe for concurrent matches. Each call acquires a
`sync.Pool` scratch object. The machine mutex protects state
interning, initialization and budget counters. Per-state RW locks protect
cached outgoing edges.

Default limits are 4,096 states and 65,536 transitions across both machines.
Upon reaching a limit, **default behavior is correct uncached fallback**.
`ResetCacheOnFull` is opt-in whole-generation recycling.
It is not LRU: under tiny budgets, repeated resets can be much slower than
ordinary fallback, so benchmark it on the intended workload.

These limits do not constrain the compiled NFA size, AC nodes, worker-local
scratch, maps' memory overhead, or transient allocations. Untrusted patterns
require independent application-level resource controls.

## Complexity and remaining work

AC presence lookup is amortized linear in input bytes. Warm lazy-DFA matching
can approach linear time in input runes plus output handling, but uncached NFA
closures are proportional to explored instruction frontiers and the number
of pattern starts. DFA state space can be exponential in rule complexity.

Not yet implemented: prefix factoring at compile time, true byte-based memory
limits, adaptive LRU state eviction, byte-oriented ASCII scan, full Unicode
equivalence ranges, anchored fast paths, multi-chunk streaming, SIMD and
match-location/capture extraction. Those are substantial follow-up projects,
not features of this release.
