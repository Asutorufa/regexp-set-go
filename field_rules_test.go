package regexpset

import (
	"math/rand"
	"reflect"
	"regexp"
	"sync"
	"testing"
)

func TestFieldRuleSetNestedPriorityAndMissingFields(t *testing.T) {
	rules := []FieldRule{
		{ID: 90, When: Expr{Op: ExprAnd, Children: []Expr{
			{Field: "host", Pattern: `(?i)example\.org$`},
			{Op: ExprOr, Children: []Expr{{Field: "path", Pattern: `^/api`}, {Field: "method", Pattern: `^POST$`}}},
			{Op: ExprNot, Children: []Expr{{Field: "host", Pattern: `^blocked\.`}}},
		}}},
		{ID: 12, When: Expr{Op: ExprOr, Children: []Expr{{Field: "host", Pattern: `example\.org`}, {Field: "path", Pattern: `^/health$`}}}},
		{ID: 0, When: Expr{Field: "path", Pattern: `^$`}},
	}
	set, err := CompileFieldRules(rules)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		fields map[string]string
		want   []uint32
	}{
		{map[string]string{"host": "www.example.org", "path": "/api/foo"}, []uint32{90, 12}},
		{map[string]string{"host": "blocked.example.org", "path": "/api/foo"}, []uint32{12}},
		{map[string]string{"host": "www.example.org", "method": "POST"}, []uint32{90, 12}},
		{map[string]string{"path": "/health"}, []uint32{12}},
		{map[string]string{"path": ""}, []uint32{0}},
		{map[string]string{}, nil},
		{map[string]string{"host": "other"}, nil},
	}
	for _, tc := range cases {
		if got := set.MatchRuleIDs(tc.fields); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("fields=%v got=%v want=%v", tc.fields, got, tc.want)
		}
		id, ok := set.MatchFirstRule(tc.fields)
		if ok != (len(tc.want) > 0) || (ok && id != tc.want[0]) {
			t.Fatalf("first fields=%v got=%d,%v want=%v", tc.fields, id, ok, tc.want)
		}
	}
	if got := set.Fields(); !reflect.DeepEqual(got, []string{"host", "method", "path"}) {
		t.Fatal(got)
	}
}

func TestFieldRuleSetInvalid(t *testing.T) {
	cases := []FieldRule{
		{ID: 1, When: Expr{Op: ExprNot}},
		{ID: 1, When: Expr{Op: ExprAnd, Children: []Expr{{Field: "a", Pattern: "a"}}}},
		{ID: 1, When: Expr{Op: ExprPattern, Pattern: "abc"}},
		{ID: 1, When: Expr{Field: "a", Pattern: "["}},
		{ID: 1, When: Expr{Op: ExprOp(99)}},
	}
	for _, tc := range cases {
		if _, err := CompileFieldRules([]FieldRule{tc}); err == nil {
			t.Fatalf("accepted bad rule %+v", tc)
		}
	}
	if _, e := CompileFieldRules([]FieldRule{{ID: 0, When: Expr{Field: "x"}}, {ID: 0, When: Expr{Field: "y"}}}); e == nil {
		t.Fatal("duplicate ID")
	}
	empty, _ := CompileFieldRules(nil)
	if empty.MatchAny(nil) || len(empty.MatchRuleIDs(nil)) != 0 {
		t.Fatal("empty")
	}
}

func TestFieldRuleSetRandomAgainstRegexp(t *testing.T) {
	patterns := []string{`^a`, `b$`, `foo`, `(?i)bar`, `\bword\b`, `^$`, `.*`, `\d+`, `\pL+`, `x|z`, `\x{FFFD}`}
	rng := rand.New(rand.NewSource(99021))
	fields := []string{"host", "path", "method", "header"}
	var mkExpr func(int) Expr
	mkExpr = func(depth int) Expr {
		if depth == 0 || rng.Intn(4) == 0 {
			return Expr{Field: fields[rng.Intn(len(fields))], Pattern: patterns[rng.Intn(len(patterns))]}
		}
		op := []ExprOp{ExprAnd, ExprOr, ExprNot}[rng.Intn(3)]
		n := 2 + rng.Intn(2)
		if op == ExprNot {
			n = 1
		}
		out := Expr{Op: op}
		for i := 0; i < n; i++ {
			out.Children = append(out.Children, mkExpr(depth-1))
		}
		return out
	}
	var baseline func(Expr, map[string]string) bool
	baseline = func(e Expr, values map[string]string) bool {
		switch e.Op {
		case ExprPattern:
			v, ok := values[e.Field]
			return ok && regexp.MustCompile(e.Pattern).MatchString(v)
		case ExprNot:
			return !baseline(e.Children[0], values)
		case ExprAnd:
			for _, x := range e.Children {
				if !baseline(x, values) {
					return false
				}
			}
			return true
		case ExprOr:
			for _, x := range e.Children {
				if baseline(x, values) {
					return true
				}
			}
			return false
		}
		return false
	}
	rules := make([]FieldRule, 30)
	for i := range rules {
		rules[i] = FieldRule{ID: uint32(100 - i), When: mkExpr(3)}
	}
	for _, options := range []Options{{}, {MaxStates: 1, MaxTransitions: 1}, {EvictLRU: true, MaxStates: 4, MaxTransitions: 4}} {
		s, err := CompileFieldRulesWithOptions(rules, options)
		if err != nil {
			t.Fatal(err)
		}
		for trial := 0; trial < 200; trial++ {
			inputs := map[string]string{}
			for _, f := range fields {
				if rng.Intn(4) != 0 {
					inputs[f] = []string{"", "foo abc", "BAR", "word", "aaaz", "12"}[rng.Intn(6)]
				}
			}
			var want []uint32
			for _, rule := range rules {
				if baseline(rule.When, inputs) {
					want = append(want, rule.ID)
				}
			}
			if got := s.MatchRuleIDs(inputs); !reflect.DeepEqual(got, want) {
				t.Fatalf("opt=%+v fields=%v got=%v want=%v", options, inputs, got, want)
			}
		}
	}
}

func TestFieldRuleSetConcurrent(t *testing.T) {
	rules := []FieldRule{{ID: 2, When: Expr{Op: ExprAnd, Children: []Expr{{Field: "host", Pattern: "abc"}, {Op: ExprNot, Children: []Expr{{Field: "path", Pattern: "no"}}}}}}}
	s, err := CompileFieldRules(rules)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if !s.MatchAny(map[string]string{"host": "abc", "path": "yes"}) {
					t.Error("no match")
					return
				}
				if s.MatchAny(map[string]string{"host": "abc", "path": "no"}) {
					t.Error("bad match")
					return
				}
			}
		}()
	}
	wg.Wait()
}

func BenchmarkFieldRuleSet(b *testing.B) {
	rules := make([]FieldRule, 500)
	for i := range rules {
		rules[i] = FieldRule{ID: uint32(i), When: Expr{Op: ExprAnd, Children: []Expr{{Field: "host", Pattern: "^svc[0-9]+\\.example\\.com$"}, {Op: ExprOr, Children: []Expr{{Field: "path", Pattern: "^/api"}, {Field: "method", Pattern: "^POST$"}}}}}}
	}
	set, err := CompileFieldRules(rules)
	if err != nil {
		b.Fatal(err)
	}
	input := map[string]string{"host": "svc42.example.com", "path": "/api/v1", "method": "GET"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = set.MatchFirstRule(input)
	}
}
