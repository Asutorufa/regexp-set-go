package regexpset

import (
	"regexp/syntax"
	"sort"
	"unicode"
	"unicode/utf8"
)

// buildUnicodeClasses computes exact consuming-instruction equivalence ranges.
// For folded programs, boundaries also cover every Unicode SimpleFold cycle
// member. This includes the special non-ASCII members of ASCII cycles (the
// Kelvin sign and long s), as well as nonlocal Greek sigma variants.
// Large programs use an exact, lazy signature interner instead of a quadratic
// eager compile, not an unsafe approximate equivalence table.
func (m *machine) buildUnicodeClasses() {
	bounds := []rune{128, utf8.MaxRune + 1}
	folded := false
	consumers := 0
	for _, inst := range m.inst {
		switch inst.Op {
		case syntax.InstRune, syntax.InstRune1:
			consumers++
			folded = folded || inst.Arg&uint32(syntax.FoldCase) != 0
			if len(inst.Rune) == 1 {
				bounds = appendBounds(bounds, inst.Rune[0], inst.Rune[0])
			} else {
				for j := 0; j+1 < len(inst.Rune); j += 2 {
					bounds = appendBounds(bounds, inst.Rune[j], inst.Rune[j+1])
				}
			}
		case syntax.InstRuneAny, syntax.InstRuneAnyNotNL:
			consumers++
		}
		if len(bounds) > 65536 {
			m.unicodeConsumers = countUnicodeConsumers(m.inst)
			m.unicodeLazy = true
			return
		}
	}
	m.unicodeConsumers = consumers
	if consumers == 0 {
		return
	}
	if folded {
		for r := rune(128); r <= utf8.MaxRune; r++ {
			if unicode.SimpleFold(r) != r {
				bounds = append(bounds, r, r+1)
			}
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
	// The eagerly computed classes are strictly exact, but O(intervals *
	// consumers). Large compiles switch to equally exact lazy equivalence.
	if n < 2 || uint64(n)*uint64(consumers) > 4_000_000 {
		m.unicodeLazy = true
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

// lazyUnicodeClass interns complete rune-acceptance signatures. It is exact for
// every possible Unicode scalar including FoldCase when the eager table would
// require too much compile-time work. All synchronization is per-machine.
func (m *machine) lazyUnicodeClass(r rune) int32 {
	m.unicodeMu.RLock()
	if id, ok := m.unicodeRunes[r]; ok {
		m.unicodeMu.RUnlock()
		return id
	}
	m.unicodeMu.RUnlock()
	bits := make([]byte, (m.unicodeConsumers+7)/8)
	i := 0
	for j := range m.inst {
		inst := &m.inst[j]
		switch inst.Op {
		case syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny, syntax.InstRuneAnyNotNL:
			if inst.MatchRune(r) {
				bits[i/8] |= byte(1 << uint(i%8))
			}
			i++
		}
	}
	signature := string(bits)
	m.unicodeMu.Lock()
	defer m.unicodeMu.Unlock()
	if id, ok := m.unicodeRunes[r]; ok {
		return id
	}
	if m.unicodeRunes == nil {
		m.unicodeRunes = make(map[rune]int32)
		m.unicodeSignatures = make(map[string]int32)
	}
	id, ok := m.unicodeSignatures[signature]
	if !ok {
		id = int32(len(m.unicodeSignatures))
		m.unicodeSignatures[signature] = id
		m.unicodeClasses = len(m.unicodeSignatures)
	}
	m.unicodeRunes[r] = id
	return id
}

func countUnicodeConsumers(insts []syntax.Inst) (n int) {
	for _, inst := range insts {
		switch inst.Op {
		case syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny, syntax.InstRuneAnyNotNL:
			n++
		}
	}
	return n
}
