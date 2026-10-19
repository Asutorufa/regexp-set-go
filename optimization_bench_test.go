package regexpset

import (
	"fmt"
	"regexp"
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
	for _, s := range []*Set{fast, unsharded} {
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
