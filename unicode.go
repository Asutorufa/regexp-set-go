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
	var folding bool
	for _, inst := range m.inst {
		switch inst.Op {
		case syntax.InstRune1, syntax.InstRune:
			if inst.Arg&uint32(syntax.FoldCase) != 0 {
				folding = true
				continue
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
	if folding {
		m.buildFoldAwareUnicodeClasses()
		return
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

// buildFoldAwareUnicodeClasses makes exact input-character equivalence classes
// for small programs containing FoldCase instructions. Unicode SimpleFold is
// not limited to adjacent code points or simple ASCII upper/lower pairs (e.g.
// Kelvin sign, long s, Greek sigma). Scanning every Unicode scalar and using
// syntax.Inst.MatchRune itself avoids assumptions about the folding tables.
// A work budget prevents huge rule sets from taking unbounded compile time;
// the fallback (no Unicode equivalence table) remains exact.
func (m *machine) buildFoldAwareUnicodeClasses() {
	consumers := make([]*syntax.Inst, 0, 8)
	for i := range m.inst {
		switch m.inst[i].Op {
		case syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny, syntax.InstRuneAnyNotNL:
			consumers = append(consumers, &m.inst[i])
		}
	}
	if len(consumers) == 0 || len(consumers) > 8 {
		return
	}
	// Use one bit per consuming instruction: identical signatures are exactly
	// equivalent for the rune-consuming part of all NFA transitions.
	signature := func(r rune) uint8 {
		var bits uint8
		for i, inst := range consumers {
			if inst.MatchRune(r) {
				bits |= 1 << uint(i)
			}
		}
		return bits
	}
	bounds := make([]rune, 1, 128)
	bounds[0] = 128
	classes := make([]int32, 0, 128)
	classIDs := make(map[uint8]int32)
	prior := signature(128)
	classIDs[prior] = 0
	classes = append(classes, 0)
	for r := rune(129); r <= utf8.MaxRune; r++ {
		current := signature(r)
		if current == prior {
			continue
		}
		id, ok := classIDs[current]
		if !ok {
			id = int32(len(classIDs))
			classIDs[current] = id
		}
		bounds = append(bounds, r)
		classes = append(classes, id)
		if len(classes) > 8192 {
			return
		} // safe per-rune fallback
		prior = current
	}
	bounds = append(bounds, utf8.MaxRune+1)
	m.unicodeBounds = bounds
	m.unicodeClass = classes
	m.unicodeClasses = len(classIDs)
}
