package regexpset

import (
	"regexp/syntax"
	"sort"
	"unicode/utf8"
)

// buildUnicodeClasses partitions non-ASCII runes at every possible boundary of
// a consuming instruction. Adjacent ranges are joined only if EVERY rune
// instruction agrees about their representative runes. Case-folded rune
// instructions are deliberately excluded: their Unicode simple-fold cycles
// need more boundaries than the raw instruction intervals supply.
// Limits avoid excessive compilation work on giant Unicode-heavy corpora.
func (m *machine) buildUnicodeClasses() {
	bounds := []rune{128, utf8.MaxRune + 1}
	for _, inst := range m.inst {
		switch inst.Op {
		case syntax.InstRune1, syntax.InstRune:
			if inst.Arg&uint32(syntax.FoldCase) != 0 {
				return
			}
			if len(inst.Rune) == 1 {
				bounds = appendBounds(bounds, inst.Rune[0], inst.Rune[0])
			} else {
				for j := 0; j+1 < len(inst.Rune); j += 2 {
					bounds = appendBounds(bounds, inst.Rune[j], inst.Rune[j+1])
				}
			}
		}
		if len(bounds) > 4096 {
			return
		}
	}
	sort.Slice(bounds, func(i, j int) bool { return bounds[i] < bounds[j] })
	n := 0
	for _, b := range bounds {
		if n == 0 || bounds[n-1] != b {
			bounds[n] = b
			n++
		}
	}
	bounds = bounds[:n]
	if n < 2 || uint64(n)*uint64(len(m.inst)) > 2_000_000 {
		return
	}
	classes := make([]int32, n-1)
	var last rune = -1
	var id int32
	for i := range classes {
		point := bounds[i]
		if i > 0 && !sameUnicodeActions(m.inst, last, point) {
			id++
		}
		classes[i] = id
		last = point
	}
	m.unicodeBounds = bounds
	m.unicodeClass = classes
	m.unicodeClasses = int(id) + 1
}

func appendBounds(bounds []rune, first, last rune) []rune {
	if first < 128 {
		first = 128
	}
	if last < 128 || first > utf8.MaxRune {
		return bounds
	}
	if last > utf8.MaxRune {
		last = utf8.MaxRune
	}
	return append(bounds, first, last+1)
}

func sameUnicodeActions(insts []syntax.Inst, a, b rune) bool {
	for i := range insts {
		inst := &insts[i]
		switch inst.Op {
		case syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny, syntax.InstRuneAnyNotNL:
			if inst.MatchRune(a) != inst.MatchRune(b) {
				return false
			}
		}
	}
	return true
}
