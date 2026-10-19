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

func BenchmarkACRootByteVectorReject(b *testing.B) {
	literal := []string{"host00001.example.com", "host00002.example.com", "host00003.example.com"}
	normal := newPrefilter(literal)
	noFast := newPrefilter(literal)
	noFast.oneRoot = false
	input := strings.Repeat("x", 4096)
	for _, tc := range []struct {
		name string
		f    *prefilter
	}{{"SIMD-byte-prescreen", normal}, {"AC-only", noFast}} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			var result bool
			for i := 0; i < b.N; i++ {
				result = tc.f.containsAny(input)
			}
			benchmarkBool = result
		})
	}
}

func TestSymbolicPrefixSharingDifferential(t *testing.T) {
	patterns := []string{
		`(?i:foobar)[0-9]+`, `(?i:foobar)[a-z]+`,
		`(?:[a-z][0-9])foo`, `(?:[a-z][0-9])bar`,
		`(?:[a-z]|[a-z])[0-9]`, `(?:[a-z]|[a-z])foo`,
		`^[a-z]+$`, `^[a-z]{1,3}foo$`,
		`(?m)^.[a-z]+$`, `(?m)^.foo$`,
		`(?s).+foo`, `(?s).+bar`,
		`(?i:k)[a-z]{2,4}`, `(?i:k)\x{212A}`,
		`\x{FFFD}foo`, `[\p{Han}]+bar`,
	}
	set, err := CompileWithOptions(patterns, Options{DisablePrefilter: true, MaxStates: 3, MaxTransitions: 10, EvictLRU: true})
	if err != nil {
		t.Fatal(err)
	}
	regexes := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		regexes[i] = regexp.MustCompile(p)
	}
	inputs := []string{"foo", "foobar", "FoObAr99", "FOOBARxyz", "kaaa", "Kka", "éfoo", "\xfffoo", "汉汉bar", "\nfoo", "\nabc", "a1foo", "aafoo", "abcbar", "", "\x00"}
	rng := rand.New(rand.NewSource(54321))
	for i := 0; i < 800; i++ {
		var buf []byte
		source := []byte("abcdfo012KK汉\n")
		for j := 0; j < rng.Intn(45); j++ {
			buf = append(buf, source[rng.Intn(len(source))])
		}
		inputs = append(inputs, string(buf))
	}
	for _, input := range inputs {
		var want []int
		for i, re := range regexes {
			if re.MatchString(input) {
				want = append(want, i)
			}
		}
		got := set.MatchIDs(input)
		if !slices.Equal(want, got) {
			t.Fatalf("input=%q got=%v want=%v", input, got, want)
		}
	}
}

func TestSymbolicPrefixReducesCompiledPrograms(t *testing.T) {
	patterns := make([]string, 128)
	for i := range patterns {
		patterns[i] = `(?i:abcdef)[a-z]{2}` + string(rune('a'+i%26)) + "[0-9]+"
	}
	set, err := CompileWithOptions(patterns, Options{DisablePrefilter: true})
	if err != nil {
		t.Fatal(err)
	}
	instructions := 0
	for _, m := range set.groups {
		instructions += len(m.inst)
	}
	if instructions >= len(patterns)*12 {
		t.Fatalf("symbolic prefix factoring ineffective: %d instructions", instructions)
	}
}
