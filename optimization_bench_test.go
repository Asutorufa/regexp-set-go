package regexpset

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// Unique hostnames exercise shard selection instead of repeatedly matching
// the same tiny set of hot strings. Domain regexes are all fully anchored.
func BenchmarkCandidateShardsHighCardinality(b *testing.B) {
	const rules = 4096
	patterns := make([]string, rules)
	for i := range patterns {
		patterns[i] = fmt.Sprintf(`^host%05d\.example\.com$`, i)
	}
	fast, err := Compile(patterns)
	if err != nil {
		b.Fatal(err)
	}
	single, err := CompileWithOptions(patterns, Options{DisableCandidateShards: true})
	if err != nil {
		b.Fatal(err)
	}
	unsharded, err := CompileWithOptions(patterns, Options{DisablePrefilter: true})
	if err != nil {
		b.Fatal(err)
	}
	inputs := make([]string, 2048)
	for i := range inputs {
		if i%2 == 0 {
			inputs[i] = fmt.Sprintf("host%05d.example.com", (i*17)%rules)
		} else {
			inputs[i] = fmt.Sprintf("missing%05d.example.org", i)
		}
	}
	for _, s := range []*Set{fast, single, unsharded} {
		for _, input := range inputs {
			s.MatchAny(input)
		}
	}
	b.Run("candidate-shards", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchmarkBool = fast.MatchAny(inputs[i%len(inputs)])
		}
	})
	b.Run("single-prefiltered-machine", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchmarkBool = single.MatchAny(inputs[i%len(inputs)])
		}
	})
	b.Run("unfiltered", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchmarkBool = unsharded.MatchAny(inputs[i%len(inputs)])
		}
	})
}

func BenchmarkRuleSetPriorityStreaming(b *testing.B) {
	rules := make([]Rule, 1024)
	rules[0] = Rule{ID: 1, Operator: RuleAND, Patterns: []string{`first`, `done`}}
	for i := 1; i < len(rules); i++ {
		rules[i] = Rule{ID: uint32(i + 1), Operator: RuleOR, Patterns: []string{fmt.Sprintf(`rule%04d`, i)}}
	}
	set, err := CompileRules(rules)
	if err != nil {
		b.Fatal(err)
	}
	for _, input := range []string{"first---done", "rule0999", "nothing"} {
		b.Run(input, func(b *testing.B) {
			set.MatchFirstRule(input)
			b.Run("first", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					_, _ = set.MatchFirstRule(input)
				}
			})
			b.Run("any", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					benchmarkBool = set.MatchAny(input)
				}
			})
			b.Run("all", func(b *testing.B) {
				b.ReportAllocs()
				var dst []uint32
				for i := 0; i < b.N; i++ {
					dst = set.MatchRuleIDsInto(input, dst)
				}
				benchmarkRuleIDs = dst
			})
		})
	}
}

var benchmarkRuleIDs []uint32

func BenchmarkLiteralTrieOracles(b *testing.B) {
	patterns := make([]string, 1024)
	for i := range patterns {
		patterns[i] = fmt.Sprintf(`^common-prefix-%04d$`, i)
	}
	s, err := CompileWithOptions(patterns, Options{DisablePrefilter: true})
	if err != nil {
		b.Fatal(err)
	}
	compiled := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		compiled[i] = regexp.MustCompile(p)
	}
	input := "common-prefix-0400"
	s.MatchAny(input)
	b.Run("shared-trie-dfa", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchmarkBool = s.MatchAny(input)
		}
	})
	b.Run("regexp-loop", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			v := false
			for _, re := range compiled {
				if re.MatchString(input) {
					v = true
					break
				}
			}
			benchmarkBool = v
		}
	})
}

func BenchmarkGeneralPrefixFactoring(b *testing.B) {
	const count = 1000
	patterns := make([]string, count)
	for i := range patterns {
		patterns[i] = fmt.Sprintf(`^sharedprefix%04d\.(?:com|net)$`, i)
	}
	set, err := CompileWithOptions(patterns, Options{DisablePrefilter: true})
	if err != nil {
		b.Fatal(err)
	}
	compiled := make([]*regexp.Regexp, count)
	for i, p := range patterns {
		compiled[i] = regexp.MustCompile(p)
	}
	input := "sharedprefix0500.com"
	set.MatchAny(input)
	b.Run("shared-NFA", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchmarkBool = set.MatchAny(input)
		}
	})
	b.Run("separate-regexp", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			ok := false
			for _, p := range compiled {
				if p.MatchString(input) {
					ok = true
					break
				}
			}
			benchmarkBool = ok
		}
	})
}

func BenchmarkRuleSetCandidatePriorityPruning(b *testing.B) {
	rules := []Rule{
		{ID: 1, Operator: RuleAND, Patterns: []string{`highestOnlyA111`, `highestOnlyB222`}},
		{ID: 2, Patterns: []string{`matchLowerPriority`}},
	}
	for i := 0; i < 1024; i++ {
		rules = append(rules, Rule{ID: uint32(i + 3), Patterns: []string{fmt.Sprintf(`excludedRule%04d`, i)}})
	}
	input := "matchLowerPriority" + strings.Repeat("unrelated", 100)
	set, err := CompileRules(rules)
	if err != nil {
		b.Fatal(err)
	}
	set.MatchFirstRule(input)
	b.Run("first-eligible-priority", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, _ = set.MatchFirstRule(input)
		}
	})
	b.Run("all-rules", func(b *testing.B) {
		b.ReportAllocs()
		dst := make([]uint32, 0, 4)
		for i := 0; i < b.N; i++ {
			dst = set.MatchRuleIDsInto(input, dst)
		}
		benchmarkRuleIDs = dst
	})
}

// BenchmarkRuleSetSatisfiedAND measures realistic priority selection when an
// AND group can actually succeed. Unlike BenchmarkRuleSet's anchored distinct
// hostname patterns, these predicates deliberately overlap on the same input.
// The sequential reference is precompiled and stops at the first satisfied
// logical rule, so this compares the same application-visible result.
func BenchmarkRuleSetSatisfiedAND(b *testing.B) {
	rules := make([]Rule, 0, 512)
	for i := 0; i < 512; i++ {
		domain := fmt.Sprintf("tenant%04d.example.com", i)
		rules = append(rules, Rule{ID: uint32(i + 1), Operator: RuleAND, Patterns: []string{
			regexp.QuoteMeta(domain), `(?i)example\.com$`, `(?i)tenant[0-9]{4}`,
		}})
	}
	set, err := CompileRules(rules)
	if err != nil {
		b.Fatal(err)
	}
	compiled := make([][]*regexp.Regexp, len(rules))
	for i, rule := range rules {
		for _, p := range rule.Patterns {
			compiled[i] = append(compiled[i], regexp.MustCompile(p))
		}
	}
	samples := []string{"tenant0000.example.com", "tenant0128.example.com", "tenant0511.example.com", "nothing.invalid"}
	for _, s := range samples {
		set.MatchFirstRule(s)
	}
	for _, tc := range []struct{ name, input string }{{"early", samples[0]}, {"middle", samples[1]}, {"late", samples[2]}, {"miss", samples[3]}} {
		b.Run(tc.name+"/ruleset", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				id, ok := set.MatchFirstRule(tc.input)
				benchmarkBool = ok
				_ = id
			}
		})
		b.Run(tc.name+"/sequential-regexp", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				found := false
				for _, group := range compiled {
					satisfied := true
					for _, r := range group {
						if !r.MatchString(tc.input) {
							satisfied = false
							break
						}
					}
					if satisfied {
						found = true
						break
					}
				}
				benchmarkBool = found
			}
		})
	}
}
