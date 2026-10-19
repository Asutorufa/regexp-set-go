package regexpset

import (
	"fmt"
	"regexp"
	"strconv"
	"testing"
)

// All benchmark variants use the same patterns, inputs, and precompiled sets.
// Report ns/op and B/op and run a separate compilation benchmark.
func benchmarkRules(count int) []string {
	patterns := make([]string, count)
	for i := range patterns {
		patterns[i] = fmt.Sprintf(`(?:^|\.)host%05d\.example\.(?:com|net)$`, i)
	}
	return patterns
}

func benchmarkInputs(hit bool, count int) []string {
	input := make([]string, 128)
	for i := range input {
		if hit && i%2 == 0 {
			input[i] = "www.host" + fmt.Sprintf("%05d", (i*1777)%count) + ".example.com"
		} else {
			input[i] = "www.unknown" + strconv.Itoa(i) + ".example.org"
		}
	}
	return input
}

func BenchmarkMatch(b *testing.B) {
	for _, count := range []int{10, 100, 1000} {
		patterns := benchmarkRules(count)
		set, err := Compile(patterns)
		if err != nil {
			b.Fatal(err)
		}
		naive := make([]*regexp.Regexp, len(patterns))
		for i, p := range patterns {
			naive[i] = regexp.MustCompile(p)
		}
		for _, hit := range []bool{false, true} {
			label := "miss"
			if hit {
				label = "mixed"
			}
			inputs := benchmarkInputs(hit, count)
			// Warm caches prior to measuring steady-state matching.
			for _, str := range inputs {
				set.MatchAny(str)
			}
			b.Run(fmt.Sprintf("rules=%d/%s/set-any", count, label), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					benchmarkBool = set.MatchAny(inputs[i%len(inputs)])
				}
			})
			b.Run(fmt.Sprintf("rules=%d/%s/regexp-loop", count, label), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					found := false
					for _, re := range naive {
						if re.MatchString(inputs[i%len(inputs)]) {
							found = true
							break
						}
					}
					benchmarkBool = found
				}
			})
			b.Run(fmt.Sprintf("rules=%d/%s/set-ids", count, label), func(b *testing.B) {
				b.ReportAllocs()
				var dst []int
				for i := 0; i < b.N; i++ {
					dst = set.MatchInto(inputs[i%len(inputs)], dst)
				}
				benchmarkIDs = dst
			})
		}
	}
}

func BenchmarkCompile(b *testing.B) {
	for _, count := range []int{10, 100, 1000} {
		patterns := benchmarkRules(count)
		b.Run(fmt.Sprintf("rules=%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				set, err := Compile(patterns)
				if err != nil {
					b.Fatal(err)
				}
				benchmarkSet = set
			}
		})
	}
}

var (
	benchmarkBool bool
	benchmarkIDs  []int
	benchmarkSet  *Set
)
