package regexpset

import (
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sync"
	"testing"
)

func TestStreamDifferentialAcrossEveryByteBoundary(t *testing.T) {
	patterns := []string{`^$`, `^foo$`, `foo`, `foo.*bar`, `(?m)^bar$`, `\bbar\b`, `(?i)ä`, `\x{FFFD}`, `\Aabc\z`, `[a-z]+`, `a?`, `[\pL]+`, `(?:a|b)*c$`, `^.+$`, `^foo\z`}
	for _, opts := range []Options{{}, {DisablePrefilter: true}, {MaxStates: 1, MaxTransitions: 1}, {EvictLRU: true, MaxStates: 2, MaxTransitions: 2}} {
		set, err := CompileWithOptions(patterns, opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, input := range []string{"", "foo", "foobar", "xxfoo", "bar", "foo\nbar", "abc", "abc\n", "Ää", "世界", "a\xffb", "\xf0\x9f\x8c\x8d", "foo\xe4\xb8\x96bar", "aaaaaac"} {
			expected := set.MatchIDs(input)
			for size := 1; size <= len(input)+1; size++ {
				st := set.NewStream()
				for i := 0; i < len(input); i += size {
					end := i + size
					if end > len(input) {
						end = len(input)
					}
					if n, e := st.Write([]byte(input[i:end])); e != nil || n != end-i {
						t.Fatalf("write n=%d err=%v", n, e)
					}
				}
				got := st.Finish()
				if !reflect.DeepEqual(got, expected) {
					t.Fatalf("options=%+v input=%q size=%d got=%v want=%v", opts, input, size, got, expected)
				}
				if !reflect.DeepEqual(st.Finish(), expected) {
					t.Fatalf("non-idempotent Finish input=%q", input)
				}
				if _, err := st.Write([]byte("hello")); !errors.Is(err, ErrStreamClosed) {
					t.Fatalf("write after finish: %v", err)
				}
			}
		}
	}
}

func TestStreamRandomChunking(t *testing.T) {
	patterns := []string{`(?m)^abc$`, `^a.*z$`, `\bfoo\b`, `(?i)αβ`, `\w+`, `a|ab|abc`, `\x{FFFD}`, `\z`, `^`, `\b`}
	s, err := Compile(patterns)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(54321))
	abc := []byte("foo abc\n αβ xyz\xff\xf0\x9f\x8c\x8d ")
	for iteration := 0; iteration < 500; iteration++ {
		input := make([]byte, rng.Intn(160))
		for i := range input {
			input[i] = abc[rng.Intn(len(abc))]
		}
		st := s.NewStream()
		for pos := 0; pos < len(input); {
			step := rng.Intn(9) + 1
			if pos+step > len(input) {
				step = len(input) - pos
			}
			if _, e := st.Write(input[pos : pos+step]); e != nil {
				t.Fatal(e)
			}
			pos += step
		}
		if got, want := st.Finish(), s.MatchIDs(string(input)); !reflect.DeepEqual(got, want) {
			t.Fatalf("input=%q got=%v want=%v", input, got, want)
		}
	}
}

func TestStreamClose(t *testing.T) {
	s, _ := Compile([]string{"a"})
	st := s.NewStream()
	st.Write([]byte("unfinished"))
	st.Close()
	st.Close()
	if _, e := st.Write([]byte("x")); !errors.Is(e, ErrStreamClosed) {
		t.Fatal(e)
	}
}

func BenchmarkStreamChunks(b *testing.B) {
	patterns := benchmarkRules(1000)
	s, err := Compile(patterns)
	if err != nil {
		b.Fatal(err)
	}
	input := []byte("www.host00001.example.com")
	for _, size := range []int{1, 4, 32} {
		b.Run(fmt.Sprintf("chunk=%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				st := s.NewStream()
				for j := 0; j < len(input); j += size {
					end := j + size
					if end > len(input) {
						end = len(input)
					}
					st.Write(input[j:end])
				}
				_ = st.Finish()
			}
		})
	}
}

func TestRuleStreamDifferential(t *testing.T) {
	rules := []Rule{
		{ID: 90, Operator: RuleAND, Patterns: []string{`^foo`, `bar$`}},
		{ID: 10, Operator: RuleOR, Patterns: []string{`foo`, `bar`}},
		{ID: 5, Operator: RuleOR, Patterns: []string{`(?m)^hi$`, `\bworld\b`}},
	}
	set, err := CompileRules(rules)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"", "foo", "foobar", "foo\nbar", "hi\nworld", "bad", "barfoo", "\xfffoobar"} {
		for size := 1; size <= len(input)+1; size++ {
			stream := set.NewStream()
			for i := 0; i < len(input); i += size {
				j := i + size
				if j > len(input) {
					j = len(input)
				}
				stream.Write([]byte(input[i:j]))
			}
			if got, want := stream.Finish(), set.MatchRuleIDs(input); !reflect.DeepEqual(got, want) {
				t.Fatalf("input=%q size=%d got=%v want=%v", input, size, got, want)
			}
		}
	}
}

func TestIndependentStreamsConcurrent(t *testing.T) {
	set, err := CompileWithOptions([]string{`^foo$`, `\bbar\b`, `(?i)kelvin`, `\pL+`, `\x{FFFD}`}, Options{EvictLRU: true, MaxStates: 8, MaxTransitions: 16})
	if err != nil {
		t.Fatal(err)
	}
	cases := []string{"foo", "bar", "KELVIN", "世界", "\xff", "hello xyz"}
	var wg sync.WaitGroup
	for j := 0; j < 8; j++ {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				input := cases[(i+offset)%len(cases)]
				stream := set.NewStream()
				for k := 0; k < len(input); k++ {
					stream.Write([]byte(input[k : k+1]))
				}
				got, want := stream.Finish(), set.MatchIDs(input)
				if !reflect.DeepEqual(got, want) {
					t.Errorf("input=%q got=%v want=%v", input, got, want)
					return
				}
			}
		}(j)
	}
	wg.Wait()
}
