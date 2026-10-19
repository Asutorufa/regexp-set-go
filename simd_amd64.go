//go:build amd64 && !purego

package regexpset

import "strings"

// asmContainsByte searches 16 bytes at a time with SSE2 PCMPEQB/PMOVMSKB.
// Every amd64 Go target supports SSE2, so no runtime AVX feature gate is needed.
//
//go:noescape
func asmContainsByte(s string, needle byte) bool

// The standard library has a more aggressively tuned SSE2/AVX2 bytealg
// dispatcher on common CPUs; use it on the production hot path rather than
// forcing the portable 16-byte SSE2 kernel where it benchmarks slower.
func fastContainsByte(s string, needle byte) bool { return strings.IndexByte(s, needle) >= 0 }
