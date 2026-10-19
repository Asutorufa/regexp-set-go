package regexpset

import (
	"math/rand"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestSIMDContainsByteDifferential(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for size := 0; size < 1030; size++ {
		buf := make([]byte, size)
		rng.Read(buf)
		for _, byteValue := range []byte{0, 1, 15, 'h', 127, 128, 255} {
			actual := fastContainsByte(string(buf), byteValue)
			expected := strings.IndexByte(string(buf), byteValue) >= 0
			if actual != expected {
				t.Fatalf("size %d needle %x got %v want %v", size, byteValue, actual, expected)
			}
		}
	}
	for _, position := range []int{0, 1, 15, 16, 31, 32, 63, 64, 127, 128, 255, 511} {
		buf := strings.Repeat("x", 512)
		data := []byte(buf)
		data[position] = 'q'
		if !fastContainsByte(string(data), 'q') {
			t.Fatalf("missed byte at position %d", position)
		}
	}
}

func TestExactFoldClassesAcrossLargeNFA(t *testing.T) {
	patterns := []string{`(?i)^k$`, `(?i)^s$`, `(?i)^σ$`, `(?i)^K$`, `(?i)^ſ$`, `(?i)κόσμος`, `(?i)abc`, `\bΣ\b`, `(?i)[a-z]+`, `(?i)[Α-Ω]+`}
	for i := 0; i < 300; i++ {
		patterns = append(patterns, `[0-9]{1,2}suffix`+string(rune('a'+i%26)))
	}
	inputs := []string{"K", "k", "K", "ſ", "s", "S", "σ", "ς", "Σ", "κόσμος", "ΚΌΣΜΟΣ", "aKs", "completely unrelated", ""}
	for _, text := range inputs {
		set, err := CompileWithOptions(patterns, Options{DisablePrefilter: true, MaxStates: 3, MaxTransitions: 6, EvictLRU: true})
		if err != nil {
			t.Fatal(err)
		}
		expected := make([]int, 0)
		for id, p := range patterns {
			r, err := regexp.Compile(p)
			if err != nil {
				t.Fatal(err)
			}
			if r.MatchString(text) {
				expected = append(expected, id)
			}
		}
		got := set.MatchIDs(text)
		if !slices.Equal(got, expected) {
			t.Fatalf("%q got %v want %v", text, got, expected)
		}
	}
}

func TestGeneralPrefixSharedDifferential(t *testing.T) {
	prefixes := []string{"foo", "foobar", "foobaz", "^foo", "(?:foobar|foobaz)", "(?:foobar|fooxyz)", "(?:foo|foobar)", "(?:foo|foobar)+", "(?:foo|foobar){1,3}", "(?:foo|bar)", "((?:fooa|foob))", `(?:fooa|foob)[a-z]+`}
	suffixes := []string{"", "$", "x+", "[0-9]+", "(?:bar|baz)", ".*z", "(?:[a-z]|b)c", "(?m)$"}
	patterns := []string{}
	for _, p := range prefixes {
		for _, s := range suffixes {
			patterns = append(patterns, p+s)
		}
	}
	set, err := CompileWithOptions(patterns, Options{DisablePrefilter: true, MaxStates: 2, MaxTransitions: 2, EvictLRU: true})
	if err != nil {
		t.Fatal(err)
	}
	reference := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		reference[i] = regexp.MustCompile(p)
	}
	rng := rand.New(rand.NewSource(17))
	samples := []string{"", "foobar", "foobaz", "fooxyz", "foox", "foobarbar", "foobbar", "foo12", "foobar12", "foofoobarbar", "foobarxyz", "FOOBAR", "K", "foobar\nx", "foofoofoofoobar"}
	for i := 0; i < 900; i++ {
		var b strings.Builder
		chars := "foabxryz0123c\n"
		for j := 0; j < rng.Intn(50); j++ {
			b.WriteByte(chars[rng.Intn(len(chars))])
		}
		samples = append(samples, b.String())
	}
	for _, sample := range samples {
		want := []int{}
		for id, re := range reference {
			if re.MatchString(sample) {
				want = append(want, id)
			}
		}
		got := set.MatchIDs(sample)
		if !slices.Equal(got, want) {
			t.Fatalf("input=%q got=%v want=%v", sample, got, want)
		}
	}
}

func TestLRURevocableHandlesAndBudget(t *testing.T) {
	patterns := []string{`a.*z`, `b.*z`, `[a-z]+\d+`, `(?i)K+`, `\bfoo\b`, `^someprefix(?:a|b){2,5}suffix$`}
	for i := 0; i < 100; i++ {
		patterns = append(patterns, "entry"+strings.Repeat("a", i%17)+"[0-9]+")
	}
	set, err := CompileWithOptions(patterns, Options{MaxStates: 3, MaxTransitions: 8, MaxCacheBytes: 2048, EvictLRU: true, DisablePrefilter: true})
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 900; i++ {
		sample := strings.Repeat("a", rng.Intn(24)) + "entry" + strings.Repeat("a", rng.Intn(17)) + "42z"
		got := set.MatchIDs(sample)
		var want []int
		for id, p := range patterns {
			if regexp.MustCompile(p).MatchString(sample) {
				want = append(want, id)
			}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("%q got %v want %v", sample, got, want)
		}
		stat := set.CacheStats()
		if stat.States > 3 || stat.Transitions > 8 || stat.Bytes > 2048 {
			t.Fatalf("cache budget exceeded: %+v", stat)
		}
		for _, g := range set.groups {
			cg := g.generation.Load()
			if !g.evictLRU {
				continue
			}
			// Evicted targets must not be kept alive by an incoming edge.
			cg.slots.Range(func(k, v any) bool {
				st := v.(*dfaState)
				if st.evicted.Load() {
					t.Errorf("evicted state still has a slot")
				}
				st.mu.RLock()
				for _, ref := range st.edges {
					if ref.state != nil {
						t.Errorf("LRU edge retained direct state pointer")
					}
				}
				st.mu.RUnlock()
				return true
			})
		}
	}
}

func BenchmarkSIMDNeedle(b *testing.B) {
	inputs := []string{strings.Repeat("a", 4096), strings.Repeat("a", 4095) + "h"}
	for i, s := range inputs {
		b.Run([]string{"miss", "hit"}[i], func(b *testing.B) {
			b.ReportAllocs()
			var found bool
			for n := 0; n < b.N; n++ {
				found = fastContainsByte(s, 'h')
			}
			benchmarkBool = found
		})
		b.Run([]string{"miss-stdlib", "hit-stdlib"}[i], func(b *testing.B) {
			b.ReportAllocs()
			var found bool
			for n := 0; n < b.N; n++ {
				found = strings.IndexByte(s, 'h') >= 0
			}
			benchmarkBool = found
		})
	}
}
