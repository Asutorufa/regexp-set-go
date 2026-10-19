package regexpset

import (
	"math/rand"
	"reflect"
	"regexp"
	"testing"
	"unicode/utf8"
)

func TestUnicodeFoldEquivalence(t *testing.T) {
	cases := [][]string{
		{`(?i)k`, `(?i)s`, `(?i)σ`},
		{`(?i)[a-z]`, `(?i)[σΣς]`},
		{`(?i)\x{212A}`, `(?i)\x{017F}`},
		{`(?i)α`, `(?i)Μ`},
	}
	alphabet := []rune("kKsSKſΣσςαΑΜμİıåÅ😺")
	rng := rand.New(rand.NewSource(48301))
	for _, patterns := range cases {
		s, err := Compile(patterns)
		if err != nil {
			t.Fatal(err)
		}
		for j := 0; j < 500; j++ {
			r := alphabet[rng.Intn(len(alphabet))]
			if j < 200 {
				r = rune(rng.Intn(utf8.MaxRune + 1))
			}
			text := string(r)
			var want []int
			for i, p := range patterns {
				if regexp.MustCompile(p).MatchString(text) {
					want = append(want, i)
				}
			}
			if got := s.MatchIDs(text); !reflect.DeepEqual(got, want) {
				t.Fatalf("patterns=%v rune=%U got=%v want=%v", patterns, r, got, want)
			}
		}
	}
}
