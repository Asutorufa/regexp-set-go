package regexpset

import (
	"fmt"
	"math/rand"
	"reflect"
	"regexp"
	"regexp/syntax"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestCandidateShardsDifferential(t *testing.T) {
	patterns := make([]string, 640)
	for i := range patterns {
		if i%3 == 0 {
			patterns[i] = fmt.Sprintf(`^service%04d\.example\.org$`, i)
		} else {
			patterns[i] = fmt.Sprintf(`^api%04d\.example\.(?:net|com)$`, i)
		}
	}
	patterns = append(patterns, `foo|bar`, `(?i)hello`, `\b`, `(?m)^`)
	s, err := Compile(patterns)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.groups) < 3 || s.candidates == nil {
		t.Fatalf("expected candidate shards, got %d groups", len(s.groups))
	}
	for i := 0; i < 640; i++ {
		var input string
		if i%3 == 0 {
			input = fmt.Sprintf("service%04d.example.org", i)
		} else {
			input = fmt.Sprintf("api%04d.example.net", i)
		}
		if got, want := s.MatchIDs(input), []int{i, 642, 643}; !reflect.DeepEqual(got, want) {
			t.Fatalf("i=%d input=%q: %v != %v", i, input, got, want)
		}
	}
	for _, input := range []string{"no-matching-domains", "bar", "HELLO", "", "\xff", "x\n"} {
		if got, want := s.MatchIDs(input), oracle(patterns, input); !reflect.DeepEqual(got, want) {
			t.Fatalf("input=%q: %v != %v", input, got, want)
		}
	}
}

func TestLiteralTrieAnchorsAndDuplicates(t *testing.T) {
	patterns := []string{"foo", "foobar", "food", "foo", "^foo$", "^foo$", "^foobar$", "(?m)^foo$", `\Afoo\z`, "unicode世界", "unicode世", "foo$", "^foo", "^foobar", "foo.*bar"}
	s, err := CompileWithOptions(patterns, Options{DisablePrefilter: true, MaxStates: 3, MaxTransitions: 4})
	if err != nil {
		t.Fatal(err)
	}
	m := s.groups[0]
	naive := 0
	for _, p := range patterns {
		tree, err := syntax.Parse(p, syntax.Perl)
		if err != nil {
			t.Fatal(err)
		}
		program, err := syntax.Compile(tree.Simplify())
		if err != nil {
			t.Fatal(err)
		}
		naive += len(program.Inst)
	}
	if len(m.inst) >= naive {
		t.Fatalf("trie failed to share prefixes: instructions=%d individually=%d", len(m.inst), naive)
	}
	for _, text := range []string{"", "foo", "barfood", "foobar", "foobar\n", "foo\nfoo", "unicode世界", "unicode世", "foo___bar", "FOO", "\xfffoo"} {
		if got, want := s.MatchIDs(text), oracle(patterns, text); !reflect.DeepEqual(got, want) {
			t.Fatalf("input=%q: got %v, want %v", text, got, want)
		}
	}
}

func TestUnicodeEquivalenceRanges(t *testing.T) {
	patterns := []string{`\p{Greek}+`, `[α-ω]+`, `\p{Han}`, `[界世]+`, `[A-Z]+`, `\bcat\b`, `^🌟+$`, `[^猫]+`, `foo.*bar`}
	s, err := CompileWithOptions(patterns, Options{DisablePrefilter: true})
	if err != nil {
		t.Fatal(err)
	}
	m := s.groups[0]
	if len(m.unicodeBounds) == 0 || m.unicodeClasses <= 1 {
		t.Fatalf("unicode partition not built: bounds=%d classes=%d", len(m.unicodeBounds), m.unicodeClasses)
	}
	rng := rand.New(rand.NewSource(20261019))
	alphabet := []rune("abcdefCATαβγδxyzω猫世界🌟\nK")
	for i := 0; i < 1800; i++ {
		var b strings.Builder
		for j := rng.Intn(30); j > 0; j-- {
			b.WriteRune(alphabet[rng.Intn(len(alphabet))])
		}
		input := b.String()
		if got, want := s.MatchIDs(input), oracle(patterns, input); !reflect.DeepEqual(got, want) {
			t.Fatalf("input=%q got %v want %v", input, got, want)
		}
	}
}

func TestTrackedCacheBudgetAndShardConcurrency(t *testing.T) {
	patterns := make([]string, 300)
	for i := range patterns {
		patterns[i] = fmt.Sprintf(`^resource%03d\.service\.net$`, i)
	}
	patterns = append(patterns, `(?i)foo`, `\btest\b`, `a.*b`)
	for _, reset := range []bool{false, true} {
		s, err := CompileWithOptions(patterns, Options{MaxCacheBytes: 4096, MaxStates: 120, MaxTransitions: 512, ResetCacheOnFull: reset})
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for j := 0; j < 10; j++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				for i := 0; i < 100; i++ {
					input := fmt.Sprintf("resource%03d.service.net", (i+worker*31)%300)
					got := s.MatchIDs(input)
					want := oracle(patterns, input)
					if !reflect.DeepEqual(got, want) {
						t.Errorf("reset=%v input=%q: got %v want %v", reset, input, got, want)
						return
					}
				}
			}(j)
		}
		wg.Wait()
		stats := s.CacheStats()
		if stats.Bytes > 4096 || stats.States > 120 || stats.Transitions > 512 {
			t.Fatalf("reset=%v stats exceeded budget: %+v", reset, stats)
		}
	}
}

func TestAdaptiveReadyBitset(t *testing.T) {
	sc := new(scratch)
	input := make([]uint32, 0, 1000)
	for i := 0; i < 700; i++ {
		input = append(input, uint32((i*83)%1000))
	}
	input = append(input, input...)
	got := canonicalReady(input, sc)
	copyOf := append([]uint32(nil), got...)
	if !sort.SliceIsSorted(got, func(i, j int) bool { return got[i] < got[j] }) {
		t.Fatal("unsorted ready set")
	}
	if len(got) != 700 {
		t.Fatalf("got %d unique, want 700", len(got))
	}
	for _, v := range got {
		if v >= 1000 {
			t.Fatal("invalid value")
		}
	}
	got = canonicalReady(append(input[:0], copyOf...), sc)
	if !reflect.DeepEqual(got, copyOf) {
		t.Fatal("bitset scratch data leaked")
	}
}

func TestRuleSetStreamPriorityAndEmpty(t *testing.T) {
	rules := []Rule{{ID: 90, Operator: RuleAND, Patterns: []string{"early", "late"}}, {ID: 40, Patterns: []string{"early"}}, {ID: 7, Operator: RuleAND, Patterns: []string{"nothing", "late"}}}
	s, err := CompileRules(rules)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		input string
		first uint32
		match bool
		all   []uint32
	}{
		{"early", 40, true, []uint32{40}},
		{"early....late", 90, true, []uint32{90, 40}},
		{"late early", 90, true, []uint32{90, 40}},
		{"none", 0, false, nil},
		{"nothing late", 7, true, []uint32{7}},
	} {
		first, ok := s.MatchFirstRule(tc.input)
		if first != tc.first || ok != tc.match || s.MatchAny(tc.input) != tc.match || !reflect.DeepEqual(s.MatchRuleIDs(tc.input), tc.all) {
			t.Fatalf("input=%q first=(%d,%v) all=%v", tc.input, first, ok, s.MatchRuleIDs(tc.input))
		}
	}
}

func TestPlainRegexBehaviorDifferentialWithAliases(t *testing.T) {
	patterns := []string{"foo", "foo", "foobar", "food", "foo", "^foo$", "^foo$", `\Afoo\z`, `(?i)foo`, `(?i)foo`, "^猫$", `\p{Han}`, ".*", `(?m)^`, `\x{FFFD}`}
	std := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		std[i] = regexp.MustCompile(p)
	}
	for _, opt := range []Options{{}, {DisablePrefilter: true}, {MaxStates: 1, MaxTransitions: 1}, {MaxCacheBytes: 1, ResetCacheOnFull: true}} {
		s, err := CompileWithOptions(patterns, opt)
		if err != nil {
			t.Fatal(err)
		}
		for _, input := range []string{"", "foo", "foobar", "food", "\nfoo", "FOO", "猫", "foo\n", "\xff"} {
			var want []int
			for i, re := range std {
				if re.MatchString(input) {
					want = append(want, i)
				}
			}
			got := s.MatchIDs(input)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("opt=%+v input=%q got=%v want=%v", opt, input, got, want)
			}
		}
	}
}

// An early lower-priority hit is safe only after the AC mask proves the
// higher-priority AND rule cannot complete anywhere in this input.
func TestRuleSetCandidatePriorityPruning(t *testing.T) {
	rules := []Rule{
		{ID: 100, Operator: RuleAND, Patterns: []string{`^priorityABCDEF`, `suffixXYZ$`}},
		{ID: 200, Operator: RuleOR, Patterns: []string{`secondaryFOO`}},
		{ID: 300, Operator: RuleOR, Patterns: []string{`fallbackBAR`}},
	}
	for i := 0; i < 900; i++ {
		rules = append(rules, Rule{ID: uint32(1000 + i), Operator: RuleOR, Patterns: []string{fmt.Sprintf(`^random_%06d\.example$`, i)}})
	}
	for _, options := range []Options{{}, {DisableCandidateShards: true}, {DisablePrefilter: true}, {MaxStates: 1, MaxTransitions: 1, MaxCacheBytes: 1}} {
		set, err := CompileRulesWithOptions(rules, options)
		if err != nil {
			t.Fatal(err)
		}
		for _, input := range []string{
			"secondaryFOO.....fallbackBAR",
			"priorityABCDEF_secondaryFOO_suffixXYZ",
			"priorityABCDEF_suffixXYZ_secondaryFOO",
			"fallbackBAR", "nothing", "priorityABCDEF", "secondaryFOO", "random_000800.example",
		} {
			var want []uint32
			for _, rule := range rules {
				matched := rule.Operator == RuleAND
				for _, pattern := range rule.Patterns {
					matches := regexp.MustCompile(pattern).MatchString(input)
					if rule.Operator == RuleAND {
						matched = matched && matches
					} else {
						matched = matched || matches
					}
				}
				if matched {
					want = append(want, rule.ID)
				}
			}
			got := set.MatchRuleIDs(input)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("opts=%+v input=%q got %v want %v", options, input, got, want)
			}
			first, ok := set.MatchFirstRule(input)
			if ok != (len(want) > 0) || (ok && first != want[0]) {
				t.Fatalf("opts=%+v input=%q got first=(%d,%v) want=%v", options, input, first, ok, want)
			}
		}
	}
}

// Verify transient uncached states never overwrite a state retained in a
// shared DFA cache, even when the cache is tiny and many inputs interleave.
func TestTransientFallbackOwnership(t *testing.T) {
	patterns := []string{`^foo(?:bar|baz)\d+$`, `(?m)^hello$`, `\bcat\b`, `.*world`, `foo`, `foo`, `bar|baz`}
	for _, opt := range []Options{{MaxStates: 1, MaxTransitions: 1}, {MaxStates: 2, MaxTransitions: 2}, {MaxStates: 8, MaxTransitions: 16, ResetCacheOnFull: true}} {
		set, err := CompileWithOptions(patterns, opt)
		if err != nil {
			t.Fatal(err)
		}
		for repeat := 0; repeat < 20; repeat++ {
			for _, input := range []string{"foobar123", "foobaz123", "foo", "HELLO", "hello\nworld", "cat bar", "baz", "", "hello", "\xfffoo"} {
				got, want := set.MatchIDs(input), oracle(patterns, input)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("opts=%+v repeat=%d input=%q got %v want %v", opt, repeat, input, got, want)
				}
			}
		}
	}
}

// Prefix factoring may only remove a deterministic literal from the original
// AST; every remainder must retain regexp/syntax's exact matching semantics.
func TestGeneralRegexpPrefixFactoring(t *testing.T) {
	patterns := []string{
		`^foobar[0-9]+$`, `^foobaz(?:cat|dog)$`, `^foobar[a-z]{2,5}`, `^fooabc\b`,
		`foobar[0-9]+`, `foobar[0-9]+`, `(?m)^foobar\d+$`, `^foobar$`,
		`foobar.*baz`, `foobaz.*qux`, `foobar(?:a|b)?`, `foobar\p{Greek}+`,
		`foo世界.+`, `foo世界$`, `^foo世界.*x$`, `foobar\A`, `foobar\z`,
		`(?i)foobar[0-9]+`, `(?s)foobar.*qux`, `(foobar)+`, `^foobar\.com$`,
	}
	for _, opt := range []Options{{}, {DisablePrefilter: true}, {MaxStates: 1, MaxTransitions: 1}, {MaxCacheBytes: 1}} {
		s, err := CompileWithOptions(patterns, opt)
		if err != nil {
			t.Fatal(err)
		}
		candidates := []string{"", "foobar", "foobar5", "foobazcat", "foobazdog", "foobarXY", "foobar10", "foobar2a", "foobarA", "foobar\\n", "foobarqux", "foobar123baz", "foobarabc", "foobarαβ", "foobar.com", "foobardog", "foo世界abc", "foo世界x", "FOOBAR1", "foobar\nx", "foobar\n"}
		rng := rand.New(rand.NewSource(77))
		alphabet := []rune("foabrcatdg01239ABC.-_\n世界αβ")
		for i := 0; i < 400; i++ {
			length := rng.Intn(36)
			var b strings.Builder
			for j := 0; j < length; j++ {
				b.WriteRune(alphabet[rng.Intn(len(alphabet))])
			}
			candidates = append(candidates, b.String())
		}
		for _, input := range candidates {
			got, want := s.MatchIDs(input), oracle(patterns, input)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("opts=%+v input=%q got=%v want=%v", opt, input, got, want)
			}
		}
	}
}
