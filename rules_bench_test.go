package regexpset

import (
	"fmt"
	"regexp"
	"strconv"
	"testing"
)

// ruleBenchmarkFixture creates groups containing overlapping expressions so
// both operator evaluation and priority selection are exercised.
func ruleBenchmarkFixture(size int) ([]Rule, [][]*regexp.Regexp) {
	rules := make([]Rule, size)
	baseline := make([][]*regexp.Regexp, size)
	for i := range rules {
		rules[i] = Rule{
			ID: uint32(size - i), Operator: RuleOperator(i % 2),
			Patterns: []string{
				`^svc` + strconv.Itoa(i) + `\.example\.net$`,
				`^api` + strconv.Itoa(i) + `\.example\.net$`,
				`^cdn` + strconv.Itoa(i) + `\.example\.net$`,
			},
		}
		for _, pattern := range rules[i].Patterns {
			baseline[i] = append(baseline[i], regexp.MustCompile(pattern))
		}
	}
	return rules, baseline
}

func baselineRuleIDs(compiled [][]*regexp.Regexp, rules []Rule, input string) []uint32 {
	var found []uint32
	for i, group := range compiled {
		matched := rules[i].Operator == RuleAND
		for _, re := range group {
			if rules[i].Operator == RuleAND {
				matched = re.MatchString(input) && matched
			} else {
				matched = re.MatchString(input) || matched
			}
		}
		if matched {
			found = append(found, rules[i].ID)
		}
	}
	return found
}

func BenchmarkRuleSet(b *testing.B) {
	for _, size := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("rules=%d", size), func(b *testing.B) {
			rules, baseline := ruleBenchmarkFixture(size)
			set, err := CompileRules(rules)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = baseline })
			for _, sample := range []struct{ name, input string }{
				{"negative", "unrelated.domain.invalid"},
				{"positive", "svc42.example.net"},
			} {
				b.Run(sample.name, func(b *testing.B) {
					set.MatchFirstRule(sample.input) // warm DFA cache
					b.Run("first", func(b *testing.B) {
						b.ReportAllocs()
						for i := 0; i < b.N; i++ {
							_, _ = set.MatchFirstRule(sample.input)
						}
					})
					b.Run("all", func(b *testing.B) {
						b.ReportAllocs()
						dst := make([]uint32, 0, 10)
						for i := 0; i < b.N; i++ {
							dst = set.MatchRuleIDsInto(sample.input, dst)
						}
					})
					if size <= 1000 {
						b.Run("baseline-separate-regexp", func(b *testing.B) {
							b.ReportAllocs()
							for i := 0; i < b.N; i++ {
								_ = baselineRuleIDs(baseline, rules, sample.input)
							}
						})
					}
				})
			}
		})
	}
}

func BenchmarkRuleSetConcurrent(b *testing.B) {
	rules, _ := ruleBenchmarkFixture(500)
	set, err := CompileRules(rules)
	if err != nil {
		b.Fatal(err)
	}
	set.MatchFirstRule("svc42.example.net")
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = set.MatchFirstRule("svc42.example.net")
		}
	})
}

func BenchmarkRuleSetTinyCache(b *testing.B) {
	rules, _ := ruleBenchmarkFixture(250)
	set, err := CompileRulesWithOptions(rules, Options{MaxStates: 2, MaxTransitions: 2})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = set.MatchFirstRule("svc42.example.net")
	}
}
