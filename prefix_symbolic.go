package regexpset

import (
	"fmt"
	"regexp/syntax"
	"sort"
)

// symbolNode provides structural factoring for guaranteed consuming prefixes
// that include character classes, case-folded literals, and any-rune tokens.
// Two regexes may share an instruction iff the consuming AST atom is identical.
// This is deliberately a syntactic proof, not a claim that arbitrary regular
// languages can be minimized with polynomial preprocessing.
type symbolNode struct {
	children map[string]*symbolNode
	atom     *syntax.Regexp // incoming consuming atom from its parent
	matches  []int
	tails    []uint32
}

func prefixAtom(r *syntax.Regexp) bool {
	switch r.Op {
	case syntax.OpCharClass, syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return true
	case syntax.OpLiteral:
		return len(r.Rune) == 1
	}
	return false
}

func splitSymbols(r *syntax.Regexp) ([]*syntax.Regexp, *syntax.Regexp) {
	switch r.Op {
	case syntax.OpLiteral:
		if len(r.Rune) == 0 {
			break
		}
		atoms := make([]*syntax.Regexp, len(r.Rune))
		for i, codepoint := range r.Rune {
			atoms[i] = &syntax.Regexp{Op: syntax.OpLiteral, Flags: r.Flags, Rune: []rune{codepoint}}
		}
		return atoms, &syntax.Regexp{Op: syntax.OpEmptyMatch}
	case syntax.OpCharClass, syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return []*syntax.Regexp{r}, &syntax.Regexp{Op: syntax.OpEmptyMatch}
	case syntax.OpCapture:
		return splitSymbols(r.Sub[0]) // captures are not returned by this engine
	case syntax.OpConcat:
		var out []*syntax.Regexp
		for i, sub := range r.Sub {
			prefix, tail := splitSymbols(sub)
			out = append(out, prefix...)
			if tail.Op != syntax.OpEmptyMatch {
				return out, concatRegex(append([]*syntax.Regexp{tail}, r.Sub[i+1:]...)...)
			}
			if len(prefix) == 0 && sub.Op != syntax.OpEmptyMatch {
				return out, concatRegex(append([]*syntax.Regexp{sub}, r.Sub[i+1:]...)...)
			}
		}
		return out, &syntax.Regexp{Op: syntax.OpEmptyMatch}
	case syntax.OpAlternate:
		if len(r.Sub) == 0 {
			break
		}
		prefixes := make([][]*syntax.Regexp, len(r.Sub))
		tails := make([]*syntax.Regexp, len(r.Sub))
		for i, sub := range r.Sub {
			prefixes[i], tails[i] = splitSymbols(sub)
		}
		common := len(prefixes[0])
		for _, p := range prefixes[1:] {
			if len(p) < common {
				common = len(p)
			}
		}
		for i := 0; i < common; i++ {
			for _, p := range prefixes[1:] {
				if prefixes[0][i].String() != p[i].String() {
					common = i
					break
				}
			}
			if common == i {
				break
			}
		}
		if common == 0 {
			break
		}
		residual := make([]*syntax.Regexp, len(r.Sub))
		for i, p := range prefixes {
			residual[i] = concatRegex(append(p[common:], tails[i])...)
		}
		return append([]*syntax.Regexp(nil), prefixes[0][:common]...), (&syntax.Regexp{Op: syntax.OpAlternate, Sub: residual}).Simplify()
	case syntax.OpPlus, syntax.OpRepeat:
		min, max := 1, -1
		if r.Op == syntax.OpRepeat {
			min, max = r.Min, r.Max
		}
		if min <= 0 {
			break
		}
		prefix, tail := splitSymbols(r.Sub[0])
		if len(prefix) == 0 {
			break
		}
		repeat := &syntax.Regexp{Op: syntax.OpRepeat, Min: min - 1, Max: max, Sub: []*syntax.Regexp{r.Sub[0]}}
		if max >= 0 {
			repeat.Max = max - 1
		}
		return prefix, concatRegex(tail, repeat)
	}
	return nil, r
}

func (m *machine) addSymbolicPrefix(id int, tree *syntax.Regexp) (bool, error) {
	source := tree
	anchor := literalAnchor{}
	if tree.Op == syntax.OpConcat && len(tree.Sub) > 1 {
		switch tree.Sub[0].Op {
		case syntax.OpBeginText:
			anchor.begin = syntax.EmptyBeginText
			source = concatRegex(tree.Sub[1:]...)
		case syntax.OpBeginLine:
			anchor.begin = syntax.EmptyBeginLine
			source = concatRegex(tree.Sub[1:]...)
		}
	}
	prefix, tail := splitSymbols(source)
	if len(prefix) == 0 || len(prefix) > 4096 {
		return false, nil
	}
	// Ordinary all-literal patterns take the specialized lower-overhead trie.
	distinct := false
	for _, p := range prefix {
		if p.Op != syntax.OpLiteral || p.Flags&syntax.FoldCase != 0 {
			distinct = true
			break
		}
	}
	if !distinct {
		return false, nil
	}

	prog, err := syntax.Compile(tail.Simplify())
	if err != nil {
		return true, err
	}
	if uint64(len(m.inst))+uint64(len(prog.Inst)) > uint64(^uint32(0)) {
		return true, fmt.Errorf("symbolic prefix: combined instruction overflow")
	}
	offset := uint32(len(m.inst))
	matchPCs := make([]uint32, 0, 1)
	for _, inst := range prog.Inst {
		switch inst.Op {
		case syntax.InstAlt, syntax.InstAltMatch:
			inst.Out += offset
			inst.Arg += offset
		case syntax.InstCapture, syntax.InstEmptyWidth, syntax.InstNop,
			syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny, syntax.InstRuneAnyNotNL:
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
	if m.symbolicTries == nil {
		m.symbolicTries = make(map[literalAnchor]*symbolNode)
	}
	root := m.symbolicTries[anchor]
	if root == nil {
		root = new(symbolNode)
		m.symbolicTries[anchor] = root
	}
	for _, atom := range prefix {
		if !prefixAtom(atom) {
			return false, fmt.Errorf("unexpected symbolic prefix atom %s", atom)
		}
		key := atom.String()
		if root.children == nil {
			root.children = make(map[string]*symbolNode)
		}
		child := root.children[key]
		if child == nil {
			child = &symbolNode{atom: atom}
			root.children[key] = child
		}
		root = child
	}
	root.tails = append(root.tails, offset+uint32(prog.Start))
	m.compiled[tree.String()] = matchPCs
	return true, nil
}

func (m *machine) symbolInst(atom *syntax.Regexp, next uint32) syntax.Inst {
	inst := syntax.Inst{Out: next}
	switch atom.Op {
	case syntax.OpLiteral:
		inst.Op = syntax.InstRune
		inst.Rune = append([]rune(nil), atom.Rune...)
		inst.Arg = uint32(atom.Flags & syntax.FoldCase)
	case syntax.OpCharClass:
		inst.Op = syntax.InstRune
		inst.Rune = append([]rune(nil), atom.Rune...)
		inst.Arg = uint32(atom.Flags & syntax.FoldCase)
	case syntax.OpAnyChar:
		inst.Op = syntax.InstRuneAny
		inst.Rune = []rune{0, 0x10ffff}
	case syntax.OpAnyCharNotNL:
		inst.Op = syntax.InstRuneAnyNotNL
		inst.Rune = []rune{0, 9, 11, 0x10ffff}
	default:
		panic("not a consuming symbolic atom")
	}
	return inst
}

func (m *machine) emitSymbolNode(n *symbolNode) uint32 {
	targets := make([]uint32, 0, len(n.tails)+len(n.children)+1)
	targets = append(targets, n.tails...)
	if len(n.matches) > 0 {
		pc := m.appendTrieInst(syntax.Inst{Op: syntax.InstMatch}, n.matches[0])
		if len(n.matches) > 1 {
			m.aliases[pc] = append(m.aliases[pc], n.matches[1:]...)
		}
		targets = append(targets, pc)
	}
	keys := make([]string, 0, len(n.children))
	for key := range n.children {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		child := n.children[key]
		next := m.emitSymbolNode(child)
		targets = append(targets, m.appendTrieInst(m.symbolInst(child.atom, next), -1))
	}
	if len(targets) == 0 {
		panic("empty symbolic trie")
	}
	root := targets[len(targets)-1]
	for i := len(targets) - 2; i >= 0; i-- {
		root = m.appendTrieInst(syntax.Inst{Op: syntax.InstAlt, Out: targets[i], Arg: root}, -1)
	}
	return root
}

func (m *machine) finishSymbolicTrie() {
	keys := make([]literalAnchor, 0, len(m.symbolicTries))
	for key := range m.symbolicTries {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].begin < keys[j].begin })
	for _, key := range keys {
		root := m.emitSymbolNode(m.symbolicTries[key])
		if key.begin != 0 {
			root = m.appendTrieInst(syntax.Inst{Op: syntax.InstEmptyWidth, Arg: uint32(key.begin), Out: root}, -1)
		}
		m.starts = append(m.starts, root)
	}
	m.symbolicTries = nil
}
