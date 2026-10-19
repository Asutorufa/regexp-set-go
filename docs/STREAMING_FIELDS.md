# Incremental streaming and nested field-aware rules

These are complementary, standalone APIs that preserve the original `Set` and
flat `RuleSet` API. They use the existing Go/RE2 matching semantics.

## Incremental input matching

```go
set, err := regexpset.Compile([]string{`^abc$`, `(?m)^event$`, `\bhello\b`})
if err != nil { panic(err) }
stream := set.NewStream()
// May be individual packets, byte slices, or UTF-8 fragments.
_, _ = stream.Write([]byte("ab"))
_, _ = stream.Write([]byte("c"))
ids := stream.Finish() // []int{0}
```

`RuleSet.NewStream()` supports the same `Write`/`Finish` interface, returning
satisfied **rule IDs** in priority order instead of pattern indexes. AND
requirements may match in different chunks. Unlike a naive per-chunk matcher,
this retains each machine's NFA/DFA frontier across writes, buffers only
an incomplete UTF-8 rune (at most 3 bytes), and defers processing the final
rune until its right-hand neighbor or end-of-text is known. Therefore `^`,
`$`, `\b`, multiline anchors, Unicode and invalid UTF-8 have their intended
**single continuous input** meanings. No chunk boundary creates an artificial
start/end or word boundary.

- `Finish()` is required to resolve end-of-input conditions. It is idempotent;
  returned result slices belong to the caller.
- `Close()` abandons unfinished matching and releases pooled scratch. Writing
  after either `Finish` or `Close` returns `ErrStreamClosed`.
- A `Stream` or `RuleStream` itself is not safe for concurrent `Write` calls;
  separate streams from the same compiled set can execute concurrently.
- Streaming processes every compiled machine rather than waiting for an entire
  message to decide AC prefilter candidates. Accordingly, streaming can be
  slower than `MatchIDs` on short, highly filterable inputs. No capture/span
  extraction or early guaranteed-negative decision is exposed.
- `Finish()` returns results rather than `Write()`: a match is permanent once
  achieved, but an `$` assertion can only be decided when the true end arrives.

## Field-aware AND/OR/NOT trees

```go
rules := []regexpset.FieldRule{
    {
        ID: 100,
        When: regexpset.Expr{Op: regexpset.ExprAnd, Children: []regexpset.Expr{
            {Field: "host", Pattern: `(?i)example\.org$`},
            {Op: regexpset.ExprOr, Children: []regexpset.Expr{
                {Field: "path", Pattern: `^/api/`},
                {Field: "method", Pattern: `^POST$`},
            }},
            {Op: regexpset.ExprNot, Children: []regexpset.Expr{
                {Field: "path", Pattern: `^/private/`},
            }},
        }},
    },
}
set, err := regexpset.CompileFieldRules(rules)
if err != nil { panic(err) }
fields := map[string]string{"host":"api.example.org","path":"/api/v1","method":"GET"}
id, ok := set.MatchFirstRule(fields) // 100, true
ids := set.MatchRuleIDs(fields)     // []uint32{100}
```

- `ExprPattern` is the zero-value operator, with `Field` and `Pattern`.
- `ExprAnd` and `ExprOr` require two or more children; `ExprNot` requires
  exactly one child. Nested expressions can be arbitrarily shaped. No implicit
  string concatenation is used between unrelated fields.
- Rule slice order defines priority, regardless of ID. Rule IDs must be unique
  and may be zero. `MatchRuleIDsInto` allows a reusable output slice.
- A **missing field never matches** a regex, even the empty pattern; a field
  present with an empty string can match zero-width expressions. `NOT` of a
  missing-field pattern is `true` (ordinary Boolean negation).
- Regexes for the same field are compiled into **one Set per field**. A field
  is evaluated lazily the first time a visited expression refers to it, and
  its regex matching results are reused by later expressions in that query.
  `MatchFirstRule` short-circuits in rule-priority and expression order.
- The engine's cache budgets (`Options`) apply **per field**, not across all
  fields in the `FieldRuleSet`. Large field sets require separate memory plans.
- `FieldRuleSet` is safe for concurrent matching. The caller must not mutate
  its input field map during a match. Logical rule actions are application
  responsibilities.

## Exact Unicode folding equivalence (bounded compile work)

When a small NFA includes `FoldCase` rune-consuming instructions, the compiler
now scans Unicode scalar values using the actual `syntax.Inst.MatchRune` behavior
and builds equivalence classes from each character's instruction-acceptance
signature. This handles nontrivial case folding (Kelvin sign, long s, sigma)
without assuming a simple ASCII upper/lower transform. To avoid enormous
compilation time and tables, it falls back to per-rune transitions if the NFA
contains more than 8 consuming instructions or produces more than 8,192
intervals. **This is not an unbounded, always-on full Unicode DFA table.**

## Validation and benchmarks

```sh
go test -run 'TestStream|TestRuleStream|TestFieldRuleSet|TestUnicodeFold' -count=1 ./...
go test -race -run 'TestStream|TestRuleStream|TestFieldRuleSet|TestUnicodeFold' ./...
go test -run '^$' -bench 'BenchmarkStreamChunks|BenchmarkFieldRuleSet' -benchmem ./...
```

The differential tests compare streaming with unchunked matching over random
split points (including invalid UTF-8) and field-tree results against Go's
standard `regexp` engine. Streaming benchmarks report per-chunk overhead,
not guaranteed throughput advantages. Always test real rule distributions.

## LRU cache maintenance (partial optimization)

With `EvictLRU: true`, states now occupy an intrusive-style ordering list:
`touch` moves a cached state to the front and `evictOne` selects the tail,
each **O(1)** with respect to state count. All linked-list operations are
synchronized, and the policy is opt-in since touching each hot state takes a
lock. **Complete eviction is not O(1)**: the engine still walks retained
states to delete cached incoming edges and adjusts tracked byte/edge budgets.
Getting truly constant-time complete eviction requires redesigning edge
ownership or introducing a safe lazy-invalidation scheme that does not retain
unbounded stale pointers. Existing cache budgets are accounting estimates,
not exact Go heap limits.
