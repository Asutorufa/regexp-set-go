package regexpset

import (
	"fmt"
	"math/rand"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// TestRandomizedGrammarDifferential uses combinations of operators that are
// easy to get wrong when optimizing prefix tries, epsilon closures, and DFA
// context-sensitive transitions. Fixed seed ensures reproducible failures.
func TestRandomizedGrammarDifferential(t *testing.T) {
	rng := rand.New(rand.NewSource(20261019))
	atoms := []string{`a`, `abc`, `x`, `z`, `-`, `.`, `[a-z]`, `[A-Z]`, `[0-9]`, `[^a]`, `\w`, `\s`, `\d`, `界`, `É`, `(?i:ab)`, `\b`, `^`, `$`, `(?m:^ )`, `\x{FFFD}`}
	var expr func(int) string
	expr = func(depth int) string {
		if depth == 0 || rng.Intn(4) == 0 {
			return atoms[rng.Intn(len(atoms))]
		}
		a := expr(depth - 1)
		switch rng.Intn(6) {
		case 0:
			return `(?:` + a + `|` + expr(depth-1) + `)`
		case 1:
			return `(?:` + a + `)` + expr(depth-1)
		case 2:
			return `(?:` + a + `)?`
		case 3:
			return `(?:` + a + `)*`
		case 4:
			return `(?:` + a + `)+`
		default:
			return `(?:` + a + `){1,2}`
		}
	}
	// Grouping shortens repeated compilation while still exercising ID aliasing.
	patterns := make([]string, 0, 88)
	compiled := make([]*regexp.Regexp, 0, 88)
	for len(patterns) < 88 {
		p := expr(3)
		re, err := regexp.Compile(p)
		if err != nil {
			continue
		}
		patterns = append(patterns, p)
		compiled = append(compiled, re)
		if len(patterns)%19 == 0 {
			patterns = append(patterns, p)
			compiled = append(compiled, re)
		}
	}
	options := []Options{
		{}, {DisablePrefilter: true}, {DisableCandidateShards: true},
		{MaxStates: 1, MaxTransitions: 1, MaxCacheBytes: 1},
		{MaxStates: 2, MaxTransitions: 2, MaxCacheBytes: 256, ResetCacheOnFull: true},
	}
	inputs := []string{"", "a", "abc", "xyz", "aabc", "x\ny", "世界", "É", "\xff", "\xfe\xff", "a\nb", "abz", "google.com"}
	alphabet := []rune("abcxyz- .0123\n世界É")
	for i := 0; i < 72; i++ {
		var b strings.Builder
		for j, n := 0, rng.Intn(17); j < n; j++ {
			b.WriteRune(alphabet[rng.Intn(len(alphabet))])
		}
		inputs = append(inputs, b.String())
	}
	for _, opt := range options {
		s, err := CompileWithOptions(patterns, opt)
		if err != nil {
			t.Fatalf("Compile(%+v): %v", opt, err)
		}
		for _, input := range inputs {
			want := make([]int, 0)
			for i, re := range compiled {
				if re.MatchString(input) {
					want = append(want, i)
				}
			}
			got := s.MatchIDs(input)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("options=%+v input=%q got=%v want=%v\npatterns=%v", opt, input, got, want, fmt.Sprint(patterns))
			}
			if s.MatchAny(input) != (len(want) > 0) {
				t.Fatalf("options=%+v input=%q MatchAny disagreement", opt, input)
			}
		}
	}
}
