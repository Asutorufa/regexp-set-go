//go:build amd64 && !purego

package regexpset

import (
	"math/rand"
	"strings"
	"testing"
)

func TestSSE2KernelDifferential(t *testing.T) {
	rng := rand.New(rand.NewSource(20261019))
	for n := 0; n < 1024; n++ {
		buf := make([]byte, n)
		rng.Read(buf)
		for _, target := range []byte{0, 1, 15, 16, 127, 128, 254, 255} {
			got := asmContainsByte(string(buf), target)
			want := strings.IndexByte(string(buf), target) >= 0
			if got != want {
				t.Fatalf("size=%d target=%d got=%v want=%v", n, target, got, want)
			}
		}
	}
}

func BenchmarkSSE2Kernel(b *testing.B) {
	input := strings.Repeat("a", 4096)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchmarkBool = asmContainsByte(input, 'z')
	}
}
