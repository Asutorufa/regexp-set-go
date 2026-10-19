package regexpset

import (
	"fmt"
	"regexp"
	"sync/atomic"
	"testing"
)

// BenchmarkPrefilter compares mandatory-literal AC rejection against direct
// lazy-DFA matching with identical rules and a warmed input corpus.
func BenchmarkPrefilter(b *testing.B) {
	for _, count := range []int{1000, 10000} {
		rules := benchmarkRules(count)
		for _, enabled := range []bool{false, true} {
			s, err := CompileWithOptions(rules, Options{DisablePrefilter: !enabled})
			if err != nil {
				b.Fatal(err)
			}
			variant := "disabled"
			if enabled {
				variant = "enabled"
			}
			for _, hits := range []bool{false, true} {
				input := benchmarkInputs(hits, count)
				label := "miss"
				if hits {
					label = "mixed"
				}
				for _, item := range input {
					s.MatchAny(item)
				}
				b.Run(fmt.Sprintf("rules=%d/%s/%s", count, label, variant), func(b *testing.B) {
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						benchmarkBool = s.MatchAny(input[i%len(input)])
					}
				})
			}
		}
	}
}

// BenchmarkCacheBudget exposes how small caches can make misses very costly.
func BenchmarkCacheBudget(b *testing.B) {
	rules := benchmarkRules(1000)
	inputs := benchmarkInputs(true, 1000)
	cases := []struct {
		label string
		opts  Options
	}{
		{"default", Options{}},
		{"tiny-fallback", Options{MaxStates: 4, MaxTransitions: 8}},
		{"tiny-reset", Options{MaxStates: 4, MaxTransitions: 8, ResetCacheOnFull: true}},
	}
	for _, tc := range cases {
		s, err := CompileWithOptions(rules, tc.opts)
		if err != nil {
			b.Fatal(err)
		}
		for _, str := range inputs {
			s.MatchAny(str)
		}
		b.Run(tc.label, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				benchmarkBool = s.MatchAny(inputs[i%len(inputs)])
			}
		})
	}
}

// BenchmarkParallel measures shared-cache contention across goroutines.
func BenchmarkParallel(b *testing.B) {
	rules := benchmarkRules(1000)
	inputs := benchmarkInputs(true, 1000)
	s, err := Compile(rules)
	if err != nil {
		b.Fatal(err)
	}
	for _, str := range inputs {
		s.MatchAny(str)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		var result bool
		for pb.Next() {
			result = s.MatchAny(inputs[i%len(inputs)])
			i++
		}
		benchmarkBoolAtomic.Store(result)
	})
}

// BenchmarkMatchIDsOracle compares all matching IDs, not just first hit.
func BenchmarkMatchIDsOracle(b *testing.B) {
	patterns := benchmarkRules(1000)
	regexps := make([]*regexp.Regexp, len(patterns))
	for i, pattern := range patterns {
		regexps[i] = regexp.MustCompile(pattern)
	}
	set, err := Compile(patterns)
	if err != nil {
		b.Fatal(err)
	}
	inputs := benchmarkInputs(true, len(patterns))
	for _, str := range inputs {
		set.MatchIDs(str)
	}
	b.Run("set-ids", func(b *testing.B) {
		b.ReportAllocs()
		var dst []int
		for i := 0; i < b.N; i++ {
			dst = set.MatchInto(inputs[i%len(inputs)], dst)
		}
		benchmarkIDs = dst
	})
	b.Run("regexp-loop-all", func(b *testing.B) {
		b.ReportAllocs()
		ids := make([]int, 0, 1)
		for i := 0; i < b.N; i++ {
			ids = ids[:0]
			str := inputs[i%len(inputs)]
			for id, re := range regexps {
				if re.MatchString(str) {
					ids = append(ids, id)
				}
			}
		}
		benchmarkIDs = ids
	})
}

var benchmarkBoolAtomic atomic.Bool
