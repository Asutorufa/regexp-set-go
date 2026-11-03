package regexpset

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestBoundedCandidatesPreserveGroups(t *testing.T) {
	f := newPrefilterPatterns(
		[]string{"foo", "foobar", "bar", "baz", "qux"},
		[]int{0, 1, 2, 3, 4}, []int{0, 1, 2, 3, 4}, 5,
	)
	// Overflow happens before the last groups appear. Those groups must still
	// be visited by the shared DFA even though exact verification is abandoned.
	for range 3 {
		bounded := f.candidatePatternsLimit("foobar baz qux", 1)
		if bounded.complete || len(bounded.ids) != 1 || bounded.groups != 31 {
			t.Fatalf("bounded scan: complete=%v ids=%v groups=%b", bounded.complete, bounded.ids, bounded.groups)
		}
		f.releaseCandidates(bounded)
		full := f.candidatePatterns("foobar baz qux")
		ids := slices.Clone(full.ids)
		slices.Sort(ids)
		if !full.complete || full.groups != 31 || !slices.Equal(ids, []int{0, 1, 2, 3, 4}) {
			t.Fatalf("full scan after reuse: complete=%v ids=%v groups=%b", full.complete, ids, full.groups)
		}
		f.releaseCandidates(full)
		repeated := f.candidatePatternsLimit("foo foo foo", 1)
		if !repeated.complete || !slices.Equal(repeated.ids, []int{0}) {
			t.Fatalf("repeated candidate overflowed: complete=%v ids=%v", repeated.complete, repeated.ids)
		}
		f.releaseCandidates(repeated)
	}
}

func TestCandidateThresholdsAgainstRegexp(t *testing.T) {
	for _, count := range []int{1, 7, 8, 16, 128, 256} {
		t.Run(fmt.Sprintf("patterns=%d", count), func(t *testing.T) {
			patterns := make([]string, count)
			for i := range patterns {
				patterns[i] = fmt.Sprintf(`token%03d`, i)
			}
			// Unfiltered patterns must run alongside sparse or dense candidates.
			patterns = append(patterns, `^$`, `(?i)ELSE`, `[0-9]+`)
			compiled := make([]*regexp.Regexp, len(patterns))
			for i, p := range patterns {
				compiled[i] = regexp.MustCompile(p)
			}
			s, err := Compile(patterns)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				for _, hits := range []int{0, 1, 2, 16, 17, 32} {
					input := strings.Join(patterns[:min(hits, count)], " ")
					for _, suffix := range []string{"", " ELSE", " missing"} {
						text := input + suffix
						var want []int
						for id, re := range compiled {
							if re.MatchString(text) {
								want = append(want, id)
							}
						}
						if got := s.MatchIDs(text); !slices.Equal(got, want) {
							t.Fatalf("MatchIDs(%q)=%v, want %v", text, got, want)
						}
						if got := s.MatchAny(text); got != (len(want) != 0) {
							t.Fatalf("MatchAny(%q)=%v, want %v", text, got, len(want) != 0)
						}
					}
				}
			}
		})
	}
}

func TestDuplicateVerifiersRetainPatternAndRuleIDs(t *testing.T) {
	patterns := []string{`shared`, `shared`, `shared`}
	for i := range 128 {
		patterns = append(patterns, fmt.Sprintf("other%03d", i))
	}
	s, err := Compile(patterns)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.MatchIDs("shared"); !slices.Equal(got, []int{0, 1, 2}) {
		t.Fatalf("duplicate pattern IDs=%v", got)
	}
	rules := []Rule{
		{ID: 99, Operator: RuleAND, Patterns: []string{"shared", "shared"}},
		{ID: 1, Patterns: []string{"shared"}},
		{ID: 50, Patterns: patterns[3:]},
	}
	r, err := CompileRules(rules)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if got := r.MatchRuleIDs("shared"); !slices.Equal(got, []uint32{99, 1}) {
			t.Fatalf("duplicate AND results=%v", got)
		}
		if id, ok := r.MatchFirstRule("shared"); !ok || id != 99 {
			t.Fatalf("first duplicate AND rule=%d,%v", id, ok)
		}
		if r.MatchAny("missing") {
			t.Fatal("pooled match retained duplicate hits")
		}
	}
}

func TestSharedFieldPredicatesPreserveMissingAndPriority(t *testing.T) {
	host := Expr{Field: "host", Pattern: `^$`}
	path := Expr{Field: "path", Pattern: `^$`}
	rules := []FieldRule{
		{ID: 9, When: Expr{Op: ExprAnd, Children: []Expr{host, host, path}}},
		{ID: 3, When: Expr{Op: ExprNot, Children: []Expr{host}}},
		{ID: 1, When: Expr{Op: ExprOr, Children: []Expr{host, path}}},
	}
	s, err := CompileFieldRules(rules)
	if err != nil {
		t.Fatal(err)
	}
	if s.PatternCount() != 6 {
		t.Fatalf("original leaf count=%d, want 6", s.PatternCount())
	}
	for range 3 {
		for _, tc := range []struct {
			fields map[string]string
			want   []uint32
		}{
			{nil, []uint32{3}},
			{map[string]string{"host": "", "path": ""}, []uint32{9, 1}},
			{map[string]string{"host": ""}, []uint32{1}},
			{map[string]string{"path": ""}, []uint32{3, 1}},
			{map[string]string{"host": "nonempty", "path": "nonempty"}, []uint32{3}},
		} {
			if got := s.MatchRuleIDs(tc.fields); !slices.Equal(got, tc.want) {
				t.Fatalf("fields=%v got=%v, want %v", tc.fields, got, tc.want)
			}
			if id, ok := s.MatchFirstRule(tc.fields); !ok || id != tc.want[0] {
				t.Fatalf("first fields=%v got=%d,%v, want %d", tc.fields, id, ok, tc.want[0])
			}
		}
	}
}
