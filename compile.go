package regexpset

import (
	"fmt"
	"regexp/syntax"
	"sync"
)

// add merges the syntax program directly; its accepting instructions retain
// their original IDs. Captures are epsilon transitions because we report
// existence only, not submatch locations or match ordering.
func (m *machine) add(id int, tree *syntax.Regexp) error {
	// Canonically equivalent expressions share compiled instructions. Distinct
	// Pattern IDs are still reported independently (important for RuleAND).
	// This factors complete duplicate programs, NOT arbitrary shared prefixes.
	key := tree.String()
	if matchPCs, ok := m.compiled[key]; ok {
		for _, pc := range matchPCs {
			m.aliases[pc] = append(m.aliases[pc], id)
		}
		return nil
	}
	if m.tryLiteralTrie(id, tree) {
		return nil
	}
	if used, err := m.addPrefixSuffix(id, tree); used || err != nil {
		return err
	}
	p, err := syntax.Compile(tree)
	if err != nil {
		return err
	}
	if uint64(len(m.inst))+uint64(len(p.Inst)) > uint64(^uint32(0)) {
		return fmt.Errorf("combined NFA instruction limit exceeded")
	}
	offset := uint32(len(m.inst))
	matchPCs := make([]uint32, 0, 1)
	m.starts = append(m.starts, offset+uint32(p.Start))
	for _, inst := range p.Inst {
		switch inst.Op {
		case syntax.InstAlt, syntax.InstAltMatch:
			inst.Out += offset
			inst.Arg += offset
		case syntax.InstCapture, syntax.InstEmptyWidth, syntax.InstNop,
			syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny,
			syntax.InstRuneAnyNotNL:
			inst.Out += offset
		}
		m.inst = append(m.inst, inst)
		if inst.Op == syntax.InstMatch {
			matchPCs = append(matchPCs, uint32(len(m.matchIDs)))
			m.matchIDs = append(m.matchIDs, id)
		} else {
			m.matchIDs = append(m.matchIDs, -1)
		}
	}
	m.compiled[key] = matchPCs
	return nil
}

func (m *machine) finish() {
	m.finishLiteralTrie()
	m.buildASCIIClasses()
	m.buildUnicodeClasses()
	n, count := len(m.inst), m.count
	m.pool = sync.Pool{New: func() any { return &scratch{seen: make([]uint32, n), matched: make([]bool, count)} }}
}

// buildASCIIClasses groups characters that satisfy exactly the same consuming
// NFA instructions. Context-sensitive assertions remain part of the edge key.
// This keeps correctness even with (?i), negated classes, and word boundaries.
func (m *machine) buildASCIIClasses() {
	classes := 1
	for _, inst := range m.inst {
		switch inst.Op {
		case syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny, syntax.InstRuneAnyNotNL:
		default:
			continue
		}
		if classes == 128 {
			break
		}
		var remap [128][2]uint8
		for i := range remap {
			remap[i] = [2]uint8{255, 255}
		}
		var next [128]uint8
		nextCount := 0
		for r := 0; r < 128; r++ {
			match := 0
			if inst.MatchRune(rune(r)) {
				match = 1
			}
			id := &remap[m.asciiClass[r]][match]
			if *id == 255 {
				*id = uint8(nextCount)
				nextCount++
			}
			next[r] = *id
		}
		if nextCount != classes {
			m.asciiClass = next
			classes = nextCount
		}
	}
	m.asciiClasses = classes
}
