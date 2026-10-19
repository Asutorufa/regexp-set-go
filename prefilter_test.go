package regexpset

import (
	"math/rand"
	"reflect"
	"regexp"
	"regexp/syntax"
	"strings"
	"testing"
)

func TestMandatoryLiteralSafety(t *testing.T) {
	examples := []struct{ pattern, expected string }{
		{`^host123\.example\.com$`, `host123.example.com`},
		{`\btracker\b`, `tracker`},
		{`(?:ab|xy)prefix[0-9]+`, `prefix`},
		{`(?i:FoObAr)`, ``},
		{`(?i:foo)caseSensitive`, `caseSensitive`},
		{`(foo|bar)`, ``},
		{`(?:foo)?bar`, `bar`},
		{`(?:foo)+bar`, `foo`},
		{`(?:foo)*`, ``},
		{`(?m)^`, ``},
		{`\p{Han}+`, ``},
		{`世界abc`, `abc`},
		{`(?:somestring|somestringelse)`, ``},
		{`\x{FFFD}abc`, `abc`},
	}
	for _, tc := range examples {
		parsed, err := syntax.Parse(tc.pattern, syntax.Perl)
		if err != nil {
			t.Fatal(err)
		}
		got := mandatoryASCII(parsed.Simplify())
		if strings.Contains(tc.pattern, "somestring") {
			continue
		}
		if got != tc.expected {
			t.Errorf("%q: got required literal %q, want %q", tc.pattern, got, tc.expected)
		}
	}
}

func TestPrefilterFalseNegatives(t *testing.T) {
	patterns := []string{
		`foo`, `fooo?`, `foo|bar`, `(?i)foo`, `afoobar`, `foo[0-9]+`,
		`^foo$`, `\bfoobar\b`, `(?m)^foobar`, `foo.*bar`,
		`(?:foo|bar)quux`, `fo[oa]bar`, `(?:foo)+`, `(?:foo)?`,
		`(?i:xyz)foobar`, `a世界`, `\x{FFFD}foo`,
	}
	for _, options := range []Options{
		{}, {MaxStates: 1, MaxTransitions: 1},
		{MaxStates: 7, MaxTransitions: 3, ResetCacheOnFull: true},
		{DisablePrefilter: true},
	} {
		s, err := CompileWithOptions(patterns, options)
		if err != nil {
			t.Fatal(err)
		}
		randSource := rand.New(rand.NewSource(20261019))
		alphabet := []string{"f", "o", "b", "a", "r", "x", "y", "z", "q", "u", "1", "8", "\n", " ", "世界", "\xff"}
		for i := 0; i < 1500; i++ {
			var input strings.Builder
			for j := randSource.Intn(32); j > 0; j-- {
				input.WriteString(alphabet[randSource.Intn(len(alphabet))])
			}
			got := s.MatchIDs(input.String())
			want := oracle(patterns, input.String())
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("opts=%+v input=%q: got %v, want %v", options, input.String(), got, want)
			}
		}
		for _, str := range []string{"foobar", "foo1", "barquux", "\xfffoo", "foobar\nfoobar", "", "FOO", "xyzfoobar"} {
			if got, want := s.MatchIDs(str), oracle(patterns, str); !reflect.DeepEqual(got, want) {
				t.Fatalf("opts=%+v input=%q: got %v want %v", options, str, got, want)
			}
		}
	}
}

func TestPrefilterACOverlapping(t *testing.T) {
	literals := []string{"foobar", "barfoo", "foo", "abcabc", "abc", "zzzzz", "\x00xy"}
	f := newPrefilter(literals)
	rng := rand.New(rand.NewSource(31))
	alphabet := []byte("abcdfoorzx\x00 ")
	for i := 0; i < 4000; i++ {
		text := make([]byte, rng.Intn(70))
		for j := range text {
			text[j] = alphabet[rng.Intn(len(alphabet))]
		}
		expected := false
		for _, lit := range literals {
			expected = expected || strings.Contains(string(text), lit)
		}
		if got := f.containsAny(string(text)); got != expected {
			t.Fatalf("text=%q got %v want %v", text, got, expected)
		}
	}
}

func TestPrefilterDisabledStillMatches(t *testing.T) {
	rules := []string{`^prefix-1234$`, `\bimportant\b`, `(?i)unicode`, `.*`}
	with, err := Compile(rules)
	if err != nil {
		t.Fatal(err)
	}
	without, err := CompileWithOptions(rules, Options{DisablePrefilter: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(with.groups) != 2 || len(without.groups) != 1 {
		t.Fatal("wrong engine grouping")
	}
	for _, str := range []string{"", "abc", "prefix-1234", "IMPORTANT", "notimportant", "\xff"} {
		if a, b := with.MatchIDs(str), without.MatchIDs(str); !reflect.DeepEqual(a, b) {
			t.Fatalf("%q: %v != %v", str, a, b)
		}
	}
}

func TestMandatoryLiteralDifferentialGenerated(t *testing.T) {
	ops := []string{"^", "", "\\b", "[a-z]*", "(?i:x)*"}
	tails := []string{"", "$", "[0-9]*", "(?:abc|xyz)", "\\b"}
	inputs := []string{"foo", "abcfoo9", "XYZabcfoo", "foo\n", "\xfffoo", "", "世界foo"}
	for _, head := range ops {
		for _, tail := range tails {
			pattern := head + "foo" + tail
			re := regexp.MustCompile(pattern)
			tree, err := syntax.Parse(pattern, syntax.Perl)
			if err != nil {
				t.Fatal(err)
			}
			required := mandatoryASCII(tree.Simplify())
			for _, input := range inputs {
				if re.MatchString(input) && required != "" && !strings.Contains(input, required) {
					t.Fatalf("unsafe mandatory literal pattern=%q input=%q literal=%q", pattern, input, required)
				}
			}
		}
	}
}
