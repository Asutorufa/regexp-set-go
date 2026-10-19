package regexpset

import (
	"fmt"
	"math/rand"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
)

func oracle(patterns []string, text string) []int {
	var out []int
	for id, pattern := range patterns {
		if regexp.MustCompile(pattern).MatchString(text) {
			out = append(out, id)
		}
	}
	return out
}

func TestDifferential(t *testing.T) {
	patterns := []string{
		"", "a", "a+", "a*", "a?", "a{2,4}", "a|ab", "ab|a", "(ab)+", "a.*b",
		"(?s)a.*b", "(?i)hello", "(?i:AbC)", "[a-z]+", "[^0-9]+", "\\d{2,}",
		"\\w+", "\\bfoo\\b", "\\Bfoo\\B", "^a", "a$", "\\Aa", "a\\z",
		"(?m)^foo$", "^$", "(?m)^$", "\\s+", "\\p{Han}+", "[世-界]+",
		"é", "\\x{FFFD}", "(?:ab|bc)+", "a(?:b|c)d", "[[:alpha:]]+",
		"\\b", "\\B", "(?i)k", "(?i)σ", "a\\z", "\\A\\z", "a", "a",
	}
	inputs := []string{
		"", "a", "aaa", "aaab", "ab", "abcd", "ababc", "foo", "xfooy", " foo ",
		"foo\nbar", "\nfoo\n", "a\nb", "hello", "HELLO", "σ", "Σ", "K", "K",
		"世界", "é", "e\u0301", "abc123", " \t ", "\xFF", "a\xFFb", "\xef\xbf\xbd",
		"a\n", "\n", "\r\n", "\x00", "foo-bar", "foobar", "aXYZb",
	}
	for _, limit := range []int{1, 8, 512} {
		set, err := CompileWithOptions(patterns, Options{MaxStates: limit, MaxTransitions: limit})
		if err != nil {
			t.Fatal(err)
		}
		for _, input := range inputs {
			expected := oracle(patterns, input)
			got := set.MatchIDs(input)
			if !reflect.DeepEqual(got, expected) {
				t.Fatalf("cache=%d input=%q: got %v, want %v", limit, input, got, expected)
			}
			if set.MatchAny(input) != (len(expected) != 0) {
				t.Fatalf("MatchAny(%q) wrong", input)
			}
			dst := make([]int, 0, len(patterns))
			got = set.MatchInto(input, dst)
			if !reflect.DeepEqual(got, expected) {
				t.Fatalf("MatchInto(%q): got %v, want %v", input, got, expected)
			}
		}
		stats := set.CacheStats()
		if stats.States > limit || stats.Transitions > limit {
			t.Fatalf("unbounded cache: %+v", stats)
		}
	}
}

func TestRandomDifferential(t *testing.T) {
	patterns := []string{
		"^a", "a$", "\\bcat\\b", "\\Bcat\\B", "(?m)^ab$", "(?s)a.*b",
		"(?:ab|bc)+", "[a-c]{1,3}", "(?i:abc)", "\\A\\z", "\\pL+", "\\d+",
		"a|ab", "(a|b)*c", "[^a]+", "a?b?", "(?m)^", "(?m)$", "\\b", "\\B",
	}
	rng := rand.New(rand.NewSource(1337))
	alphabet := []string{"a", "b", "c", "1", " ", "\n", "猫", "é", "\xFF", "_"}
	for _, limit := range []int{2, 32, 512} {
		set, err := CompileWithOptions(patterns, Options{MaxStates: limit, MaxTransitions: limit})
		if err != nil {
			t.Fatal(err)
		}
		compiled := make([]*regexp.Regexp, len(patterns))
		for i, p := range patterns {
			compiled[i] = regexp.MustCompile(p)
		}
		for trial := 0; trial < 1500; trial++ {
			var input strings.Builder
			for n := rng.Intn(40); n > 0; n-- {
				input.WriteString(alphabet[rng.Intn(len(alphabet))])
			}
			text := input.String()
			var expected []int
			for i, re := range compiled {
				if re.MatchString(text) {
					expected = append(expected, i)
				}
			}
			if got := set.MatchIDs(text); !reflect.DeepEqual(got, expected) {
				t.Fatalf("limit=%d trial=%d input=%q: got %v want %v", limit, trial, text, got, expected)
			}
		}
	}
}

func TestCompilationAndEmptySet(t *testing.T) {
	if _, err := Compile([]string{"valid", "["}); err == nil || !strings.Contains(err.Error(), "pattern 1") {
		t.Fatalf("wanted indexed syntax error, got %v", err)
	}
	if _, err := CompileWithOptions([]string{"x"}, Options{MaxStates: -1}); err == nil {
		t.Fatal("negative state limit accepted")
	}
	if _, err := CompileWithOptions([]string{"x"}, Options{MaxTransitions: -1}); err == nil {
		t.Fatal("negative edge limit accepted")
	}
	set, err := Compile(nil)
	if err != nil {
		t.Fatal(err)
	}
	if set.Len() != 0 || set.MatchAny("x") || len(set.MatchIDs("x")) != 0 {
		t.Fatal("empty set matched")
	}
	if got := set.MatchInto("x", []int{123}); len(got) != 0 {
		t.Fatalf("MatchInto: %v", got)
	}
}

func TestRuleIDStabilityAndDst(t *testing.T) {
	set, err := Compile([]string{"foo", "bar", "foo", ".*", "^foo$"})
	if err != nil {
		t.Fatal(err)
	}
	want := []int{0, 2, 3, 4}
	if got := set.MatchIDs("foo"); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	reuse := make([]int, 3, 8)
	for i := 0; i < 5; i++ {
		got := set.MatchInto("foo", reuse)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("reuse: %v", got)
		}
		reuse = got
		if got := set.MatchIDs("q"); !reflect.DeepEqual(got, []int{3}) {
			t.Fatalf("stale marks: %v", got)
		}
	}
}

func TestConcurrent(t *testing.T) {
	patterns := []string{"^abc$", "abc", "a+", "\\bfoo\\b", "(?i)世界", "\\d+", "^$", "\\Bxy", "(?m)^line$"}
	set, err := CompileWithOptions(patterns, Options{MaxStates: 20, MaxTransitions: 30})
	if err != nil {
		t.Fatal(err)
	}
	inputs := []string{"", "abc", "abcx", "foo", "foobar", "世界", "123", "line\nfoo", "aabb", "xyxy"}
	expect := make([][]int, len(inputs))
	for i, str := range inputs {
		expect[i] = oracle(patterns, str)
	}
	var wg sync.WaitGroup
	errors := make(chan string, 64)
	for worker := 0; worker < 24; worker++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				index := (id*31 + i) % len(inputs)
				got := set.MatchIDs(inputs[index])
				if !reflect.DeepEqual(got, expect[index]) {
					select {
					case errors <- fmt.Sprintf("%q: %v != %v", inputs[index], got, expect[index]):
					default:
					}
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
}

func TestLargePatternSet(t *testing.T) {
	var patterns []string
	for i := 0; i < 2000; i++ {
		patterns = append(patterns, fmt.Sprintf("^prefix-%04d-(?:ok|yes)$", i))
	}
	set, err := CompileWithOptions(patterns, Options{MaxStates: 64, MaxTransitions: 128})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int{0, 1, 17, 500, 1999} {
		input := fmt.Sprintf("prefix-%04d-yes", id)
		if got := set.MatchIDs(input); !reflect.DeepEqual(got, []int{id}) {
			t.Fatalf("%q: got %v", input, got)
		}
	}
	if got := set.MatchIDs("prefix-2000-yes"); len(got) != 0 {
		t.Fatalf("unexpected: %v", got)
	}
	stats := set.CacheStats()
	if stats.States > 64 || stats.Transitions > 128 {
		t.Fatal(stats)
	}
}

func FuzzMatchesRegexp(f *testing.F) {
	patterns := []string{"", "^ab+$", "\\bfoo\\b", "(?m)^foo$", "(?s)a.*b", "[[:alpha:]]+", "\\p{Han}+", "(?i)strasse", "\\A\\z"}
	set, err := CompileWithOptions(patterns, Options{MaxStates: 5, MaxTransitions: 5})
	if err != nil {
		f.Fatal(err)
	}
	for _, input := range []string{"", "foo", "abbb", "a\nb", "世界", "\xfffoo", "\x00"} {
		f.Add(input)
	}
	f.Fuzz(func(t *testing.T, input string) {
		if len(input) > 4096 {
			t.Skip()
		}
		got := set.MatchIDs(input)
		want := oracle(patterns, input)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("input=%q got=%v want=%v", input, got, want)
		}
		if !sort.IntsAreSorted(got) {
			t.Fatalf("unordered IDs: %v", got)
		}
	})
}
