package regexpset

import (
	"fmt"
	"reflect"
	"regexp"
	"sync"
	"testing"
)

func TestASCIIClassesExact(t *testing.T) {
	patterns := []string{`[a-z]+`, `[A-Z]+`, `(?i)foo`, `[^0-9]`, `\w+`, `\s+`, `(?s).`, `\n`, `\bword\b`, `x|z`}
	s, err := CompileWithOptions(patterns, Options{DisablePrefilter: true})
	if err != nil {
		t.Fatal(err)
	}
	m := s.groups[0]
	if m.asciiClasses < 2 || m.asciiClasses > 128 {
		t.Fatalf("classes=%d", m.asciiClasses)
	}
	for i := 0; i < 128; i++ {
		for j := 0; j < 128; j++ {
			if m.asciiClass[i] != m.asciiClass[j] {
				continue
			}
			for _, inst := range m.inst {
				if inst.MatchRune(rune(i)) != inst.MatchRune(rune(j)) {
					t.Fatalf("class %d maps distinct runes %q %q for %v", m.asciiClass[i], i, j, inst)
				}
			}
		}
	}
	// Confirm sharing across an actual cached transition, not just the table.
	for _, str := range []string{"HELLO123\n", "foobar", "ABC", "___", "012", "世界"} {
		if got, want := s.MatchIDs(str), oracle(patterns, str); !reflect.DeepEqual(got, want) {
			t.Fatalf("%q: %v != %v", str, got, want)
		}
	}
}

func TestGroupBudgetsAndCacheFallback(t *testing.T) {
	patterns := []string{`^foo$`, `^bar$`, `.*`, `(?i)word`, `\d+`, `(?m)^`}
	for _, limit := range []int{1, 2, 3, 5, 16} {
		s, err := CompileWithOptions(patterns, Options{MaxStates: limit, MaxTransitions: limit})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 20; i++ {
			for _, input := range []string{"foo", "bar", "abc", "", "\n123\n", "WORD", "\xff"} {
				if got, want := s.MatchIDs(input), oracle(patterns, input); !reflect.DeepEqual(got, want) {
					t.Fatalf("limit=%d input=%q: %v != %v", limit, input, got, want)
				}
			}
		}
		st := s.CacheStats()
		if st.States > limit || st.Transitions > limit {
			t.Fatalf("limit=%d stats=%+v", limit, st)
		}
	}
}

func TestResetGenerationsConcurrent(t *testing.T) {
	patterns := []string{`^domain[0-9]+\.example$`, `(?:x|y)+z`, `^host123\.example$`, `(?i)fOo`, `\btest\b`, `.*ab+`, `^$`}
	s, err := CompileWithOptions(patterns, Options{MaxStates: 3, MaxTransitions: 2, ResetCacheOnFull: true})
	if err != nil {
		t.Fatal(err)
	}
	texts := []string{"", "foo", "test", "fooo", "host123.example", "domain999.example", "yxyz", "abc", "xabc", "zzzz", "\n", "xxyzzz"}
	expected := make([][]int, len(texts))
	for i, input := range texts {
		expected[i] = oracle(patterns, input)
	}
	var wg sync.WaitGroup
	errors := make(chan string, 64)
	for worker := 0; worker < 16; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				index := (worker*17 + i*31) % len(texts)
				got := s.MatchIDs(texts[index])
				if !reflect.DeepEqual(got, expected[index]) {
					select {
					case errors <- fmt.Sprintf("worker=%d input=%q %v != %v", worker, texts[index], got, expected[index]):
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
	st := s.CacheStats()
	if st.Resets == 0 {
		t.Fatal("reset behavior was not exercised")
	}
	if st.States > 3 || st.Transitions > 2 {
		t.Fatalf("unbounded stats: %+v", st)
	}
}

func TestZeroWidthAndInvalidUTF8(t *testing.T) {
	patterns := []string{`\A`, `\z`, `\b`, `\B`, `^`, `$`, `(?m)^`, `(?m)$`, `^\x{FFFD}$`, `\x{FFFD}`, `(?s).`, `\n`, `\p{Han}`, `(?i)k`}
	texts := []string{"", "\xff", "\xff\xff", "\ufffd", "foo\r\nbar", "x\n", "\n", "猫", "K", "K", "k", "\x00", "x\xe2\x82"}
	for _, opts := range []Options{{}, {DisablePrefilter: true}, {MaxStates: 1, MaxTransitions: 1}, {MaxStates: 3, MaxTransitions: 3, ResetCacheOnFull: true}} {
		set, err := CompileWithOptions(patterns, opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, str := range texts {
			want := make([]int, 0)
			for i, p := range patterns {
				if regexp.MustCompile(p).MatchString(str) {
					want = append(want, i)
				}
			}
			got := set.MatchIDs(str)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("opts=%+v text=%q got=%v want=%v", opts, str, got, want)
			}
		}
	}
}

func TestMaxIntegerBudget(t *testing.T) {
	// Large user-specified cache counts must not overflow during partitioning.
	maxInt := int(^uint(0) >> 1)
	s, err := CompileWithOptions([]string{`foo`, `.*`, `bar`}, Options{MaxStates: maxInt, MaxTransitions: maxInt})
	if err != nil {
		t.Fatal(err)
	}
	if !s.MatchAny("foo") {
		t.Fatal("unexpected no match")
	}
	sum := 0
	for _, g := range s.groups {
		if g.maxStates < 0 || g.maxTransitions < 0 {
			t.Fatal("budget overflow")
		}
		sum += g.maxStates
	}
	if sum != maxInt {
		t.Fatalf("budget=%d != %d", sum, maxInt)
	}
}
