package regexpset

import (
	"regexp/syntax"
	"sort"
)

type scratch struct {
	seen              []uint32
	generation        uint32
	stack, ready, raw []uint32
	matches, ids      []int
	matched           []bool
}

// closure includes the starts at each Unicode rune boundary: this implements
// unanchored substring matching. Empty-width conditions are checked against
// both adjacent runes, not just the rune that was consumed.
func (m *machine) closure(raw []uint32, ctx syntax.EmptyOp, sc *scratch) *dfaState {
	sc.generation++
	if sc.generation == 0 {
		clear(sc.seen)
		sc.generation = 1
	}
	sc.stack = sc.stack[:0]
	sc.ready = sc.ready[:0]
	sc.matches = sc.matches[:0]
	sc.stack = append(sc.stack, raw...)
	sc.stack = append(sc.stack, m.starts...)
	for len(sc.stack) != 0 {
		pc := sc.stack[len(sc.stack)-1]
		sc.stack = sc.stack[:len(sc.stack)-1]
		if sc.seen[pc] == sc.generation {
			continue
		}
		sc.seen[pc] = sc.generation
		inst := &m.inst[pc]
		switch inst.Op {
		case syntax.InstAlt, syntax.InstAltMatch:
			sc.stack = append(sc.stack, inst.Out, inst.Arg)
		case syntax.InstNop, syntax.InstCapture:
			sc.stack = append(sc.stack, inst.Out)
		case syntax.InstEmptyWidth:
			if ctx&syntax.EmptyOp(inst.Arg) == syntax.EmptyOp(inst.Arg) {
				sc.stack = append(sc.stack, inst.Out)
			}
		case syntax.InstMatch:
			sc.matches = append(sc.matches, m.matchIDs[pc])
		case syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny, syntax.InstRuneAnyNotNL:
			sc.ready = append(sc.ready, pc)
		}
	}
	sort.Slice(sc.ready, func(i, j int) bool { return sc.ready[i] < sc.ready[j] })
	sort.Ints(sc.matches)
	return &dfaState{ready: sc.ready, matches: sc.matches}
}

func (m *machine) collect(sc *scratch, matches []int) {
	for _, id := range matches {
		// IDs are indexes relative to the *original* input list, but the
		// engine's matched slice is sized to the original list as well.
		if !sc.matched[id] {
			sc.matched[id] = true
			sc.ids = append(sc.ids, id)
		}
	}
}
