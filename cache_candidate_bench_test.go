package regexpset

import (
	"fmt"
	"testing"
)

func BenchmarkExactCandidates(b *testing.B) {
	patterns := exactCorpus(2048)
	for _, disabled := range []bool{false, true} {
		mode := "exact"
		if disabled {
			mode = "shared-dfa"
		}
		set, err := CompileWithOptions(patterns, Options{DisableExactCandidates: disabled})
		if err != nil {
			b.Fatal(err)
		}
		for _, hit := range []bool{false, true} {
			typ := "miss"
			input := "unrelated.invalid"
			if hit {
				typ = "hit"
				input = "www.svc00999.example.net"
			}
			b.Run(fmt.Sprintf("%s/%s", mode, typ), func(b *testing.B) {
				set.MatchAny(input)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					benchmarkBool = set.MatchAny(input)
				}
			})
		}
	}
}

func BenchmarkEvictLRU(b *testing.B) {
	patterns := []string{`(?:foo|bar)+`, `\bword\b`, `a+b`, `^foo`, `bar$`, `(?:abc)+`, `\d+`}
	for _, evict := range []bool{false, true} {
		mode := "fallback"
		if evict {
			mode = "lru"
		}
		set, err := CompileWithOptions(patterns, Options{DisablePrefilter: true, MaxStates: 8, MaxTransitions: 8, MaxCacheBytes: 3500, EvictLRU: evict})
		if err != nil {
			b.Fatal(err)
		}
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				benchmarkBool = set.MatchAny(fmt.Sprintf("foo%d bar word", i%37))
			}
		})
	}
}
