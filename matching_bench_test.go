package regexpset

import (
	"fmt"
	"testing"
)

// Identical predicates isolate repeated-output work: the first rule always
// wins, regardless of how many later rules reuse the same host and path.
func BenchmarkDuplicateFields(b *testing.B) {
	for _, count := range []int{1, 100, 500, 2000} {
		rules := make([]FieldRule, count)
		for i := range rules {
			rules[i] = FieldRule{ID: uint32(i), When: Expr{Op: ExprAnd, Children: []Expr{
				{Field: "host", Pattern: `^svc[0-9]+\.example\.com$`},
				{Field: "path", Pattern: `^/api`},
			}}}
		}
		set, err := CompileFieldRules(rules)
		if err != nil {
			b.Fatal(err)
		}
		fields := map[string]string{"host": "svc42.example.com", "path": "/api/v1"}
		set.MatchFirstRule(fields)
		b.Run(fmt.Sprintf("n=%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				_, _ = set.MatchFirstRule(fields)
			}
		})
	}
}

func BenchmarkCompileCandidateModes(b *testing.B) {
	for _, count := range []int{7, 1000, 10000} {
		patterns := exactCorpus(count)
		for _, tc := range []struct {
			name string
			opts Options
		}{
			{"default", Options{}},
			{"no-exact", Options{DisableExactCandidates: true}},
		} {
			b.Run(fmt.Sprintf("n=%d/%s", count, tc.name), func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					set, err := CompileWithOptions(patterns, tc.opts)
					if err != nil {
						b.Fatal(err)
					}
					benchmarkSet = set
				}
			})
		}
	}
}

// The same required literal deliberately produces dense candidates, which
// must fall back to the shared DFA without collecting every candidate ID.
func BenchmarkDenseCandidates(b *testing.B) {
	patterns := make([]string, 2048)
	for i := range patterns {
		patterns[i] = fmt.Sprintf(`^shared-prefix\.[0-9]{4}x%04d$`, i)
	}
	set, err := Compile(patterns)
	if err != nil {
		b.Fatal(err)
	}
	input := "shared-prefix.1234x1024"
	candidates := set.candidates.candidatePatterns(input)
	if len(candidates.ids) != len(patterns) {
		b.Fatal("fixture did not produce dense candidates")
	}
	set.candidates.releaseCandidates(candidates)
	set.MatchAny(input)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkBool = set.MatchAny(input)
	}
}

func BenchmarkShortPrefilter(b *testing.B) {
	set, err := Compile(exactCorpus(2048))
	if err != nil {
		b.Fatal(err)
	}
	for _, tc := range []struct{ name, input string }{
		{"root-absent", "unrelated.invalid"},
		{"root-present", "something.invalid"},
		{"hit", "www.svc00999.example.net"},
	} {
		set.MatchAny(tc.input)
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				benchmarkBool = set.MatchAny(tc.input)
			}
		})
	}
}
