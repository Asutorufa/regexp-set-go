package regexpset

import (
	"fmt"
	"math/rand"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
)

func exactCorpus(n int) []string {
	patterns := make([]string, n)
	for i := range patterns {
		patterns[i] = fmt.Sprintf(`(?:^|\.)svc%05d\.example\.(?:net|org)$`, i)
	}
	return patterns
}

func TestExactCandidateDifferential(t *testing.T) {
	patterns := exactCorpus(256)
	patterns = append(patterns, `^foo|bar$`, `(?i)WORLD`, `^$`, `\b(?:foo|bar)\b`, `[0-9]+`, `svc00042`, `\x{FFFD}`)
	baseline := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		baseline[i] = regexp.MustCompile(p)
	}
	opts := []Options{
		{}, {DisableExactCandidates: true}, {DisableCandidateShards: true},
		{MaxStates: 3, MaxTransitions: 3, MaxCacheBytes: 1300, EvictLRU: true},
		{MaxStates: 3, MaxTransitions: 3, MaxCacheBytes: 1300, DisableExactCandidates: true, EvictLRU: true},
		{MaxStates: 3, MaxTransitions: 3, ResetCacheOnFull: true},
	}
	inputs := []string{"", "FOO", "foo", "WORLD", "www.svc00042.example.net", "svc00042.example.net", "www.svc00255.example.org", "bar", "invalid\xff", "svc00042", "foo bar", "svc00042.example.bad"}
	rng := rand.New(rand.NewSource(123))
	for i := 0; i < 140; i++ {
		if i%3 == 0 {
			inputs = append(inputs, fmt.Sprintf("www.svc%05d.example.net", rng.Intn(500)))
		} else {
			inputs = append(inputs, fmt.Sprintf("%d.%s", i, strings.Repeat("a", rng.Intn(15))))
		}
	}
	for _, opt := range opts {
		set, err := CompileWithOptions(patterns, opt)
		if err != nil {
			t.Fatal(err)
		}
		for _, input := range inputs {
			var want []int
			for i, re := range baseline {
				if re.MatchString(input) {
					want = append(want, i)
				}
			}
			if got := set.MatchIDs(input); !reflect.DeepEqual(got, want) {
				t.Fatalf("opts=%+v input=%q got=%v want=%v", opt, input, got, want)
			}
			if got := set.MatchAny(input); got != (len(want) > 0) {
				t.Fatalf("opts=%+v any(%q)=%v want=%v", opt, input, got, len(want) > 0)
			}
		}
		stats := set.CacheStats()
		if stats.States > normalizedStates(opt) || stats.Transitions > normalizedEdges(opt) {
			t.Fatalf("cache count limit: %+v", stats)
		}
		if opt.MaxCacheBytes > 0 && stats.Bytes > opt.MaxCacheBytes {
			t.Fatalf("cache bytes exceeded: %+v budget=%d", stats, opt.MaxCacheBytes)
		}
	}
}
func normalizedStates(o Options) int {
	if o.MaxStates == 0 {
		return 4096
	}
	return o.MaxStates
}
func normalizedEdges(o Options) int {
	if o.MaxTransitions == 0 {
		return 65536
	}
	return o.MaxTransitions
}

func TestLRUEvictionPressure(t *testing.T) {
	patterns := []string{`ab`, `bc`, `cd`, `abc`, `xyz`, `^ab`, `\bword\b`, `.*$`}
	oracle := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		oracle[i] = regexp.MustCompile(p)
	}
	set, err := CompileWithOptions(patterns, Options{DisablePrefilter: true, EvictLRU: true, MaxStates: 4, MaxTransitions: 4, MaxCacheBytes: 1200})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		input := fmt.Sprintf("ab %d bc %s xyz word", i, strings.Repeat("z", i%11))
		var want []int
		for id, re := range oracle {
			if re.MatchString(input) {
				want = append(want, id)
			}
		}
		if got := set.MatchIDs(input); !reflect.DeepEqual(got, want) {
			t.Fatalf("%q got=%v want=%v", input, got, want)
		}
		st := set.CacheStats()
		if st.States > 4 || st.Transitions > 4 || st.Bytes > 1200 || st.States < 0 || st.Transitions < 0 || st.Bytes < 0 {
			t.Fatalf("bad stats: %+v", st)
		}
	}
	if st := set.CacheStats(); st.Evictions == 0 {
		t.Fatalf("LRU did not evict: %+v", st)
	}
}

func TestCandidateLRUConcurrent(t *testing.T) {
	patterns := exactCorpus(128)
	patterns = append(patterns, `^foo`, `\bword\b`, `(?i)world`)
	set, err := CompileWithOptions(patterns, Options{EvictLRU: true, MaxStates: 8, MaxTransitions: 8, MaxCacheBytes: 2500})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for worker := 0; worker < 12; worker++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < 80; i++ {
				input := fmt.Sprintf("www.svc%05d.example.net", (i+seed*17)%128)
				got := set.MatchIDs(input)
				want := []int{(i + seed*17) % 128}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s: got %v want %v", input, got, want)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	st := set.CacheStats()
	if st.States > 8 || st.Transitions > 8 || st.Bytes > 2500 || st.States < 0 || st.Transitions < 0 || st.Bytes < 0 {
		t.Fatalf("bad stats: %+v", st)
	}
}

func TestRuleSetExactCandidatePriority(t *testing.T) {
	rules := []Rule{{ID: 900, Operator: RuleAND, Patterns: []string{`svc001`, `\.example\.net$`}}, {ID: 100, Operator: RuleOR, Patterns: []string{`svc001`, `svc002`}}, {ID: 1, Patterns: []string{`^unrelated`}}}
	for _, opt := range []Options{{}, {DisableExactCandidates: true}, {EvictLRU: true, MaxStates: 3, MaxTransitions: 3, MaxCacheBytes: 1500}} {
		set, err := CompileRulesWithOptions(rules, opt)
		if err != nil {
			t.Fatal(err)
		}
		tests := []struct {
			input string
			ids   []uint32
		}{{"www.svc001.example.net", []uint32{900, 100}}, {"www.svc002.invalid", []uint32{100}}, {"unrelated", []uint32{1}}, {"nothing", nil}}
		for _, tc := range tests {
			if got := set.MatchRuleIDs(tc.input); !reflect.DeepEqual(got, tc.ids) {
				t.Fatalf("opts=%+v input=%q got=%v want=%v", opt, tc.input, got, tc.ids)
			}
			id, ok := set.MatchFirstRule(tc.input)
			if ok != (len(tc.ids) > 0) || (ok && id != tc.ids[0]) {
				t.Fatalf("opts=%+v input=%q got=(%d,%v) want=%v", opt, tc.input, id, ok, tc.ids)
			}
		}
	}
}

// Evicting while readers traverse cached edges must never corrupt the NFA
// frontier, resurrect an evicted target, or break AND/OR aggregation.
func TestLRUConcurrentSharedMachineOracle(t *testing.T) {
	patterns := []string{`(?:abc|bcd)+`, `(?m)^header`, `\bword\b`, `(?i)hello`, `a+b+`, `[^a-z]+`, `^z+`, `(?:foo|bar)baz`, `\x{FFFD}`}
	compiled := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		compiled[i] = regexp.MustCompile(p)
	}
	set, err := CompileWithOptions(patterns, Options{DisablePrefilter: true, MaxStates: 6, MaxTransitions: 5, MaxCacheBytes: 1600, EvictLRU: true})
	if err != nil {
		t.Fatal(err)
	}
	inputs := []string{"abcabc", "foobar", "foobaz", "aabbb", "header\nworld", "word", "heLLo", "\xff", "bcdabc", "zzz", "42", "unrelated"}
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 95; i++ {
				input := inputs[(i*11+w*3)%len(inputs)]
				var want []int
				for id, re := range compiled {
					if re.MatchString(input) {
						want = append(want, id)
					}
				}
				got := set.MatchIDs(input)
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%q got=%v want=%v", input, got, want)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	stats := set.CacheStats()
	if stats.States > 6 || stats.Transitions > 5 || stats.Bytes > 1600 || stats.Bytes < 0 {
		t.Fatalf("capacity violation: %+v", stats)
	}
	if stats.Evictions < 1 {
		t.Fatalf("no eviction under shared-machine pressure: %+v", stats)
	}
}

func TestExactCandidatesFalsePositiveAndDense(t *testing.T) {
	patterns := exactCorpus(64)
	patterns = append(patterns, `foo.*bar`, `foo.*bar`, `^bar`, `(?i)CASEFOLD`, `^$`)
	set, err := Compile(patterns)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"foo only", "foo bar", "www.svc00002.example.net", "svc00002.example.netfoo", "CASEFOLD", ""} {
		var want []int
		for i, pat := range patterns {
			if regexp.MustCompile(pat).MatchString(input) {
				want = append(want, i)
			}
		}
		if got := set.MatchIDs(input); !reflect.DeepEqual(got, want) {
			t.Fatalf("input=%q got %v want %v", input, got, want)
		}
	}
}

func TestCachePolicyConflict(t *testing.T) {
	_, err := CompileWithOptions([]string{"foo"}, Options{EvictLRU: true, ResetCacheOnFull: true})
	if err == nil {
		t.Fatal("expected cache policy conflict")
	}
}
