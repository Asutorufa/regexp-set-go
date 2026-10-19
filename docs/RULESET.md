# Priority-ordered AND/OR RuleSet

`RuleSet` groups many regular expressions into logical rules while using the
existing shared Thompson NFA / lazy DFA matching engine. It does **not** create
a RegexSet or automaton per logical rule.

## Example

```go
rules := []regexpset.Rule{
    {
        ID: 100,
        Operator: regexpset.RuleAND,
        Patterns: []string{`(?i)google`, `\.com$`},
    },
    {
        ID: 42,
        Operator: regexpset.RuleOR,
        Patterns: []string{`github\.com$`, `gitlab\.com$`},
    },
    {
        ID: 7, // RuleOR is the default when Operator is omitted.
        Patterns: []string{`\.doubleclick\.net$`},
    },
}
set, err := regexpset.CompileRules(rules)
if err != nil { panic(err) }

first, ok := set.MatchFirstRule("https://github.com") // 42, true
all := set.MatchRuleIDs("https://google.com")      // []uint32{100}
var reused []uint32
reused = set.MatchRuleIDsInto("github.com", reused) // []uint32{42}
```

## Semantics

- **Priority is rule slice order**: `rules[0]` is highest, regardless of numeric
  ID or the time an expression is discovered in the input. The first matching
  rule wins in `MatchFirstRule`. `MatchRuleIDs` returns all satisfied rule IDs
  in priority order.
- **OR**: at least one expression matches somewhere in the input. This is the
  default operator, represented by `RuleOR` (zero).
- **AND**: every expression independently matches somewhere in the **same
  input**. They may match different substrings. This is not a concatenated
  regex, and does not require adjacent or simultaneous match spans.
- Different rules can use different operators in the **same** `RuleSet`.
- All regexes use Go/RE2-compatible `regexp/syntax.Perl` syntax and the same
  search semantics as `regexp.Regexp.MatchString`.
- Duplicate **patterns** are allowed and count as separate AND requirements;
  duplicate **rule IDs** are rejected. IDs may be zero. Empty rules and unknown
  operators are rejected. An empty *pattern string* is valid and matches every
  input, consistent with Go regexp.
- Match results are immutable copies owned by the caller. The compiled set
  supports concurrent matching and shares the original cache budgets. Rule
  actions, conditions on different input fields, and network routing remain
  application responsibilities.

### Intentional limitations

A `Rule` uses one flat Boolean operator, either AND or OR. Arbitrary nesting
such as `(A OR B) AND (C OR D)` and predicates across different fields (host,
URL path, process) are **not** supported natively. An application can compose
several rules or extend the rule AST explicitly; do not assume that matching
multiple fields against a concatenated string reproduces field-specific rules.

`MatchFirstRule` normally scans enough input to preserve priority: an early
low-priority match cannot beat a later matching higher-priority AND rule.
When the conservative AC candidate mask proves that every higher-priority
rule is impossible, matching **can stop once the earliest still-possible rule
is satisfied**. This is not arbitrary input-dependent priority reordering.

## Performance and safety

The underlying engine still uses conservative Aho-Corasick mandatory-literal
rejection filtering, two shared NFA / lazy-DFA machines when required, and
bounded retained state and transition counts. No per-rule DFA is constructed.

The logical-rule aggregator uses pooled scratch: each matched pattern indexes
its owning rule and increments that rule's hit count. Only touched rules are
examined; `MatchRuleIDs` sorts their indices to return priority order. Clearing
counts also touches only participating rules. Duplicate pattern strings retain
separate pattern identities so AND counting remains correct.

Use `go test -run '^$' -bench '^BenchmarkRuleSet' -benchmem` to compare 100,
1,000, or 5,000 rule fixtures against standalone Go regexp and to measure
concurrency / tiny cache degradation. Fixtures are synthetic; always profile
real rule sets, especially where few mandatory literals can be extracted,
cache budgets are small, or many patterns match each input.
