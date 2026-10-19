//go:build !amd64 || purego

package regexpset

import "strings"

func fastContainsByte(s string, needle byte) bool { return strings.IndexByte(s, needle) >= 0 }
