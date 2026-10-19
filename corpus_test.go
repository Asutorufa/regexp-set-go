package regexpset

import (
	"math/rand"
	"reflect"
	"regexp"
	"testing"
)

// TestCorpusDifferential tests independently generated *sets* of expressions,
// comparing every returned ID to regexp.Regexp.MatchString.
func TestCorpusDifferential(t *testing.T) {
	atoms := []string{`abc`, `foo`, `xyz`, `\d`, `[a-z]`, `\p{Han}`, `(?i:word)`, `\b`, `\x{FFFD}`, `.`, `\n`}
	prefixes := []string{``, `^`, `\A`, `(?:x|y)?`, `.*`, `(?m)^`}
	suffixes := []string{``, `$`, `\z`, `+`, `*`, `{1,3}`, `(?:foo|bar)`, `[0-9]?`}
	inputs := []string{"", "abc", "xabc", "foobar", "xyz", "123", "word", "WORD", "\\p", "世界", "猫", "\xffabc", "foo\nbar", "xyz\ufffd"}
	rng := rand.New(rand.NewSource(0xbadc0de))
	for corpus := 0; corpus < 30; corpus++ {
		patterns := make([]string, 20)
		for i := range patterns {
			for {
				candidate := prefixes[rng.Intn(len(prefixes))] + atoms[rng.Intn(len(atoms))] + suffixes[rng.Intn(len(suffixes))]
				if _, err := regexp.Compile(candidate); err == nil {
					patterns[i] = candidate
					break
				}
			}
		}
		set, err := CompileWithOptions(patterns, Options{MaxStates: 9, MaxTransitions: 13, ResetCacheOnFull: corpus%2 == 0})
		if err != nil {
			t.Fatal(err)
		}
		for _, input := range inputs {
			if got, want := set.MatchIDs(input), oracle(patterns, input); !reflect.DeepEqual(got, want) {
				t.Fatalf("corpus=%d input=%q patterns=%q got=%v want=%v", corpus, input, patterns, got, want)
			}
		}
	}
}
