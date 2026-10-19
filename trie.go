package regexpset

import (
	"fmt"
	"regexp/syntax"
	"sort"
)

// Literal-only regexes are factored into a trie at compilation time. Roots
// are separated by start/end assertions so ^, (?m)^, $, (?m)$, and unanchored
// semantics never mix. Case-folded, optional and arbitrary regex constructs
// continue through regexp/syntax unchanged.
type literalAnchor struct{ begin, end syntax.EmptyOp }
type literalNode struct {
	next    map[rune]*literalNode
	matches []int
	tails   []uint32 // entry points for compiled non-literal suffix programs
}

func literalShape(tree *syntax.Regexp) ([]rune, literalAnchor, bool) {
	parts := []*syntax.Regexp{tree}
	if tree.Op == syntax.OpConcat {
		parts = tree.Sub
	}
	anchor := literalAnchor{}
	if len(parts) > 1 {
		switch parts[0].Op {
		case syntax.OpBeginText:
			anchor.begin = syntax.EmptyBeginText
			parts = parts[1:]
		case syntax.OpBeginLine:
			anchor.begin = syntax.EmptyBeginLine
			parts = parts[1:]
		}
	}
	if len(parts) > 1 {
		switch parts[len(parts)-1].Op {
		case syntax.OpEndText:
			anchor.end = syntax.EmptyEndText
			parts = parts[:len(parts)-1]
		case syntax.OpEndLine:
			anchor.end = syntax.EmptyEndLine
			parts = parts[:len(parts)-1]
		}
	}
	if len(parts) != 1 || parts[0].Op != syntax.OpLiteral || parts[0].Flags&syntax.FoldCase != 0 || len(parts[0].Rune) < 2 || len(parts[0].Rune) > 4096 {
		return nil, anchor, false
	}
	return parts[0].Rune, anchor, true
}

// regexPrefixShape recognizes a mandatory case-sensitive literal at the
// beginning of a concatenation, optionally preceded by a start assertion.
// The remainder stays a full regexp/syntax program: only the deterministic
// prefix is factored, so repeats/alternations/zero-width suffixes keep their
// standard semantics. Alternations at the top level are not transformed.
func regexPrefixShape(tree *syntax.Regexp) ([]rune, literalAnchor, *syntax.Regexp, bool) {
	if tree.Op != syntax.OpConcat || len(tree.Sub) < 2 {
		return nil, literalAnchor{}, nil, false
	}
	sub := tree.Sub
	anchor := literalAnchor{}
	switch sub[0].Op {
	case syntax.OpBeginText:
		anchor.begin = syntax.EmptyBeginText
		sub = sub[1:]
	case syntax.OpBeginLine:
		anchor.begin = syntax.EmptyBeginLine
		sub = sub[1:]
	}
	if len(sub) < 2 || sub[0].Op != syntax.OpLiteral || sub[0].Flags&syntax.FoldCase != 0 || len(sub[0].Rune) < 2 || len(sub[0].Rune) > 4096 {
		return nil, anchor, nil, false
	}
	var tail *syntax.Regexp
	if len(sub) == 2 {
		tail = sub[1]
	} else {
		tail = (&syntax.Regexp{Op: syntax.OpConcat, Sub: append([]*syntax.Regexp(nil), sub[1:]...)}).Simplify()
	}
	return sub[0].Rune, anchor, tail, true
}

// addPrefixSuffix shares the leading literal trie but keeps the full suffix
// Thompson program. The suffix accepts with the original pattern ID.
func (m *machine) addPrefixSuffix(id int, tree *syntax.Regexp) (bool, error) {
	prefix, anchor, tail, ok := regexPrefixShape(tree)
	if !ok {
		return false, nil
	}
	p, err := syntax.Compile(tail.Simplify())
	if err != nil {
		return true, err
	}
	if uint64(len(m.inst))+uint64(len(p.Inst)) > uint64(^uint32(0)) {
		return true, fmt.Errorf("combined NFA instruction limit exceeded")
	}
	offset := uint32(len(m.inst))
	matchPCs := make([]uint32, 0, 1)
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
	if m.literalTries == nil {
		m.literalTries = make(map[literalAnchor]*literalNode)
	}
	node := m.literalTries[anchor]
	if node == nil {
		node = new(literalNode)
		m.literalTries[anchor] = node
	}
	for _, r := range prefix {
		if node.next == nil {
			node.next = make(map[rune]*literalNode)
		}
		next := node.next[r]
		if next == nil {
			next = new(literalNode)
			node.next[r] = next
		}
		node = next
	}
	node.tails = append(node.tails, offset+uint32(p.Start))
	m.compiled[tree.String()] = matchPCs
	return true, nil
}

func (m *machine) tryLiteralTrie(id int, tree *syntax.Regexp) bool {
	literal, anchor, ok := literalShape(tree)
	if !ok {
		return false
	}
	if m.literalTries == nil {
		m.literalTries = make(map[literalAnchor]*literalNode)
	}
	node := m.literalTries[anchor]
	if node == nil {
		node = new(literalNode)
		m.literalTries[anchor] = node
	}
	for _, r := range literal {
		if node.next == nil {
			node.next = make(map[rune]*literalNode)
		}
		next := node.next[r]
		if next == nil {
			next = new(literalNode)
			node.next[r] = next
		}
		node = next
	}
	node.matches = append(node.matches, id)
	return true
}

func (m *machine) appendTrieInst(inst syntax.Inst, id int) uint32 {
	at := uint32(len(m.inst))
	m.inst = append(m.inst, inst)
	m.matchIDs = append(m.matchIDs, id)
	return at
}

func (m *machine) emitLiteralNode(n *literalNode, end syntax.EmptyOp) uint32 {
	targets := make([]uint32, 0, len(n.next)+1+len(n.tails))
	targets = append(targets, n.tails...)
	if len(n.matches) > 0 {
		pc := m.appendTrieInst(syntax.Inst{Op: syntax.InstMatch}, n.matches[0])
		if len(n.matches) > 1 {
			m.aliases[pc] = append(m.aliases[pc], n.matches[1:]...)
		}
		if end != 0 {
			pc = m.appendTrieInst(syntax.Inst{Op: syntax.InstEmptyWidth, Arg: uint32(end), Out: pc}, -1)
		}
		targets = append(targets, pc)
	}
	keys := make([]rune, 0, len(n.next))
	for r := range n.next {
		keys = append(keys, r)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, r := range keys {
		next := m.emitLiteralNode(n.next[r], end)
		pc := m.appendTrieInst(syntax.Inst{Op: syntax.InstRune1, Rune: []rune{r}, Out: next}, -1)
		targets = append(targets, pc)
	}
	if len(targets) == 0 {
		panic("regexpset: invalid empty literal trie node")
	}
	root := targets[len(targets)-1]
	for i := len(targets) - 2; i >= 0; i-- {
		root = m.appendTrieInst(syntax.Inst{Op: syntax.InstAlt, Out: targets[i], Arg: root}, -1)
	}
	return root
}

func (m *machine) finishLiteralTrie() {
	keys := make([]literalAnchor, 0, len(m.literalTries))
	for k := range m.literalTries {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].begin != keys[j].begin {
			return keys[i].begin < keys[j].begin
		}
		return keys[i].end < keys[j].end
	})
	for _, anchor := range keys {
		root := m.emitLiteralNode(m.literalTries[anchor], anchor.end)
		if anchor.begin != 0 {
			root = m.appendTrieInst(syntax.Inst{Op: syntax.InstEmptyWidth, Arg: uint32(anchor.begin), Out: root}, -1)
		}
		m.starts = append(m.starts, root)
	}
	m.literalTries = nil
}
