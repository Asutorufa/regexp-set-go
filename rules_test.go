package regexpset

import (
	"math/rand"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
)

func TestRuleSetPriorityAndBooleanOperators(t *testing.T) {
	rules := []Rule{
		{ID: 900, Operator: RuleAND, Patterns: []string{`alpha`, `beta`}},
		{ID: 100, Operator: RuleOR, Patterns: []string{`beta`, `gamma`}},
		{ID: 7, Operator: RuleAND, Patterns: []string{`beta`, `delta`}},
		{ID: 0, Patterns: []string{`alpha`}},
	}
	set, err := CompileRules(rules)
	if err != nil {
		t.Fatal(err)
	}
	if set.Len() != 4 || set.PatternCount() != 7 {
		t.Fatalf("lengths: rules=%d patterns=%d", set.Len(), set.PatternCount())
	}
	cases := []struct {
		input string
		want  []uint32
	}{
		{"", nil},
		{"no match", nil},
		{"alpha", []uint32{0}},
		{"beta", []uint32{100}},
		{"gamma", []uint32{100}},
		{"alpha beta", []uint32{900, 100, 0}},
		{"beta delta alpha", []uint32{900, 100, 7, 0}},
		{"delta beta", []uint32{100, 7}},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := set.MatchRuleIDs(tc.input)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("all: got %v want %v", got, tc.want)
			}
			id, ok := set.MatchFirstRule(tc.input)
			if ok != (len(tc.want) != 0) {
				t.Fatalf("first match existence %v want %v", ok, len(tc.want) != 0)
			}
			if ok && id != tc.want[0] {
				t.Fatalf("first=%d want %d", id, tc.want[0])
			}
			if set.MatchAny(tc.input) != ok {
				t.Fatal("MatchAny disagrees with first match")
			}
		})
	}
}

func TestRuleSetDuplicatesAnchorsAndInvalidUTF8(t *testing.T) {
	rules := []Rule{
		{ID: 5, Operator: RuleAND, Patterns: []string{`foo`, `foo`, `^foo`}},
		{ID: 3, Operator: RuleOR, Patterns: []string{`foo`, `foo`}},
		{ID: 20, Operator: RuleAND, Patterns: []string{`(?m)^x`, `(?m)x$`}},
		{ID: 9, Patterns: []string{`\x{FFFD}`}},
	}
	set, err := CompileRulesWithOptions(rules, Options{MaxStates: 1, MaxTransitions: 1})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		input string
		want  []uint32
	}{
		{"foo", []uint32{5, 3}},
		{"barfoo", []uint32{3}},
		{"x\nfoo", []uint32{3, 20}},
		{"\xff", []uint32{9}},
	}
	for _, tc := range cases {
		if got := set.MatchRuleIDs(tc.input); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%q got %v want %v", tc.input, got, tc.want)
		}
	}
}

func TestRuleSetZeroWidthAndEmpty(t *testing.T) {
	set, err := CompileRules([]Rule{
		{ID: 2, Operator: RuleAND, Patterns: []string{`^`, `$`}},
		{ID: 1, Operator: RuleOR, Patterns: []string{`a+`, `\b`}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"", "hi", "a", "\n", "世界"} {
		if id, matched := set.MatchFirstRule(input); !matched || id != 2 {
			t.Fatalf("first(%q)=(%d,%v) want (2,true)", input, id, matched)
		}
	}
	empty, err := CompileRules(nil)
	if err != nil {
		t.Fatal(err)
	}
	if empty.MatchAny("anything") || empty.Len() != 0 || empty.PatternCount() != 0 {
		t.Fatal("empty rule set matched")
	}
	if got := empty.MatchRuleIDs("test"); len(got) != 0 {
		t.Fatalf("empty set returned %v", got)
	}
	if id, ok := empty.MatchFirstRule("test"); ok || id != 0 {
		t.Fatalf("empty first result=(%d,%v)", id, ok)
	}
}

func TestRuleSetInvalidRules(t *testing.T) {
	cases := []struct {
		name  string
		rules []Rule
		part  string
	}{
		{"duplicate ID", []Rule{{ID: 42, Patterns: []string{"foo"}}, {ID: 42, Patterns: []string{"bar"}}}, "duplicate ID"},
		{"empty group", []Rule{{ID: 1}}, "no patterns"},
		{"invalid operator", []Rule{{ID: 1, Operator: RuleOperator(255), Patterns: []string{"a"}}}, "invalid operator"},
		{"bad pattern", []Rule{{ID: 1, Patterns: []string{"ok"}}, {ID: 30, Patterns: []string{"good", "["}}}, "rule 1 (ID 30) pattern 1"},
		{"bad options", []Rule{{ID: 1, Patterns: []string{"ok"}}}, ""},
	}
	for _, tc := range cases {
		if tc.name == "bad options" {
			_, err := CompileRulesWithOptions(tc.rules, Options{MaxStates: -1})
			if err == nil {
				t.Fatal("negative limit accepted")
			}
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			_, err := CompileRules(tc.rules)
			if err == nil || !strings.Contains(err.Error(), tc.part) {
				t.Fatalf("got %v; want %q", err, tc.part)
			}
		})
	}
}

func TestRuleSetIntoReuseAndIndependence(t *testing.T) {
	set, err := CompileRules([]Rule{
		{ID: 80, Operator: RuleOR, Patterns: []string{"b"}},
		{ID: 10, Operator: RuleAND, Patterns: []string{"a", "b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	dst := make([]uint32, 3, 16)
	dst[0] = 12345
	got := set.MatchRuleIDsInto("ba", dst)
	if !reflect.DeepEqual(got, []uint32{80, 10}) {
		t.Fatalf("got %v", got)
	}
	if got[0] != 80 {
		t.Fatal("prior contents leaked")
	}
	again := set.MatchRuleIDs("b")
	if !reflect.DeepEqual(again, []uint32{80}) {
		t.Fatalf("pooled results leaked: %v", again)
	}
	if !reflect.DeepEqual(got, []uint32{80, 10}) {
		t.Fatal("previous result aliased pooled state")
	}
	if got := set.MatchRuleIDsInto("none", got); len(got) != 0 {
		t.Fatalf("no match result = %v", got)
	}
}

func TestRuleSetDifferentialAgainstRegexp(t *testing.T) {
	pool := []string{
		`^a`, `b$`, `foo`, `bar|baz`, `\w+`, `\bfoo\b`, `(?i)alpha`,
		`[a-z]{2,4}`, `.*\.example\.org$`, `^$`, `世界`, `\x{FFFD}`,
		`(?m)^x$`, `a?`, `[0-9]+`, `foo.*bar`, `\Ahi`, `z\z`, `ab|ab`,
	}
	rng := rand.New(rand.NewSource(20261019))
	rules := make([]Rule, 70)
	compiled := make([][]*regexp.Regexp, len(rules))
	for i := range rules {
		rules[i] = Rule{ID: uint32(5000 - 3*i), Operator: RuleOperator(i % 2)}
		for j := 0; j < 1+rng.Intn(4); j++ {
			p := pool[rng.Intn(len(pool))]
			rules[i].Patterns = append(rules[i].Patterns, p)
			compiled[i] = append(compiled[i], regexp.MustCompile(p))
		}
	}
	options := []Options{
		{},
		{DisablePrefilter: true},
		{MaxStates: 1, MaxTransitions: 1},
		{MaxStates: 2, MaxTransitions: 2, ResetCacheOnFull: true},
	}
	inputs := []string{"", "foo", "foobar", "foo bar", "hi", "x\nfoo", "世界", "alpha", "a", "ab", "bar.example.org", "\xff", "\n", "x", "zzz"}
	alphabet := []rune("abcfozALPHA0123. \n世界")
	for i := 0; i < 200; i++ {
		var b strings.Builder
		for j, n := 0, rng.Intn(25); j < n; j++ {
			b.WriteRune(alphabet[rng.Intn(len(alphabet))])
		}
		inputs = append(inputs, b.String())
	}
	for _, opt := range options {
		set, err := CompileRulesWithOptions(rules, opt)
		if err != nil {
			t.Fatal(err)
		}
		for _, input := range inputs {
			var expected []uint32
			for i, rule := range rules {
				matched := rule.Operator == RuleAND
				for _, re := range compiled[i] {
					if rule.Operator == RuleOR {
						matched = matched || re.MatchString(input)
					} else {
						matched = matched && re.MatchString(input)
					}
				}
				if matched {
					expected = append(expected, rule.ID)
				}
			}
			if actual := set.MatchRuleIDs(input); !reflect.DeepEqual(actual, expected) {
				t.Fatalf("opts=%+v input=%q got=%v want=%v", opt, input, actual, expected)
			}
			id, ok := set.MatchFirstRule(input)
			if ok != (len(expected) > 0) || (ok && id != expected[0]) {
				t.Fatalf("first opts=%+v input=%q got=(%d,%v), want=%v", opt, input, id, ok, expected)
			}
		}
	}
}

func TestRuleSetConcurrent(t *testing.T) {
	rules := make([]Rule, 500)
	for i := range rules {
		rules[i] = Rule{ID: uint32(500 - i), Operator: RuleOperator(i % 2), Patterns: []string{`^host[0-9]+\.example\.org$`, `example`, `(?:foo|bar)+`}}
	}
	set, err := CompileRulesWithOptions(rules, Options{MaxStates: 8, MaxTransitions: 16, ResetCacheOnFull: true})
	if err != nil {
		t.Fatal(err)
	}
	inputs := []string{"www.example.org", "host123.example.org", "foofoo", "no-match", "host500.example.org"}
	var wg sync.WaitGroup
	for worker := 0; worker < 12; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < 80; j++ {
				input := inputs[(worker+j)%len(inputs)]
				want := strings.Contains(input, "example") || strings.Contains(input, "foo")
				if got := set.MatchAny(input); got != want {
					t.Errorf("input=%q got=%v want=%v", input, got, want)
					return
				}
				got := set.MatchRuleIDs(input)
				if want && len(got) == 0 || !want && len(got) != 0 {
					t.Errorf("input=%q got=%v", input, got)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
}

func FuzzRuleSetAgainstRegexp(f *testing.F) {
	rules := []Rule{
		{ID: 7, Operator: RuleAND, Patterns: []string{`(?i)foo`, `\bbar\b`}},
		{ID: 5, Operator: RuleOR, Patterns: []string{`^a`, `\x{FFFD}`}},
		{ID: 99, Operator: RuleAND, Patterns: []string{`(?m)^x`, `(?m)y$`}},
	}
	compiled := make([][]*regexp.Regexp, len(rules))
	for i, rule := range rules {
		for _, pattern := range rule.Patterns {
			compiled[i] = append(compiled[i], regexp.MustCompile(pattern))
		}
	}
	set, err := CompileRules(rules)
	if err != nil {
		f.Fatal(err)
	}
	for _, seed := range []string{"", "foo bar", "a\xff", "x\ny", "世界", "foo"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		var want []uint32
		for i, rule := range rules {
			matched := rule.Operator == RuleAND
			for _, re := range compiled[i] {
				if rule.Operator == RuleOR {
					matched = matched || re.MatchString(input)
				} else {
					matched = matched && re.MatchString(input)
				}
			}
			if matched {
				want = append(want, rule.ID)
			}
		}
		if got := set.MatchRuleIDs(input); !reflect.DeepEqual(got, want) {
			t.Fatalf("input=%q got %v want %v", input, got, want)
		}
	})
}
