package regexpset

import (
	"regexp/syntax"
	"strings"
)

// mandatoryASCII returns a byte string present in *every* match of r, or "".
// It deliberately ignores case-folded literals, alternatives, optional
// branches and Unicode literals: a false negative here is unacceptable.
func mandatoryASCII(r *syntax.Regexp) string {
	switch r.Op {
	case syntax.OpLiteral:
		if r.Flags&syntax.FoldCase != 0 {
			return ""
		}
		var best string
		run := make([]byte, 0, len(r.Rune))
		for _, char := range r.Rune {
			if char < 128 {
				run = append(run, byte(char))
				continue
			}
			if len(run) > len(best) {
				best = string(run)
			}
			run = run[:0]
		}
		if len(run) > len(best) {
			best = string(run)
		}
		if len(best) >= 3 {
			return best
		}
		return ""
	case syntax.OpCapture, syntax.OpPlus:
		return mandatoryASCII(r.Sub[0])
	case syntax.OpRepeat:
		if r.Min >= 1 {
			return mandatoryASCII(r.Sub[0])
		}
	case syntax.OpConcat:
		// A concatenation contains every mandatory substring of each child.
		// Taking the longest gives a cheap, selective rejection filter.
		best := ""
		for _, child := range r.Sub {
			if v := mandatoryASCII(child); len(v) > len(best) {
				best = v
			}
		}
		return best
	case syntax.OpAlternate:
		// A common required substring of all alternatives is still required.
		// Only accept identical candidates or their literal intersection.
		if len(r.Sub) == 0 {
			return ""
		}
		candidate := mandatoryASCII(r.Sub[0])
		if candidate == "" {
			return ""
		}
		for _, sub := range r.Sub[1:] {
			other := mandatoryASCII(sub)
			if !strings.Contains(other, candidate) {
				return ""
			}
		}
		return candidate
	}
	return ""
}

// prefilter is a sparse Aho-Corasick automaton. Every terminal contains a
// bitmask of candidate machines, not just a Boolean existence flag. Failure
// links inherit outputs so overlapping/contained literals are never missed.
// A bit identifies a whole regex group, not a particular regex: the group
// still verifies matches using its full NFA/DFA.
type prefilter struct{ nodes []acNode }
type acNode struct {
	next   map[byte]int
	fail   int
	output uint64
}

// newPrefilter preserves the simple existence matcher for callers/tests.
func newPrefilter(literals []string) *prefilter {
	groups := make([]int, len(literals))
	return newPrefilterGroups(literals, groups)
}

// groups maps each mandatory literal to its candidate-machine index [0, 63].
func newPrefilterGroups(literals []string, groups []int) *prefilter {
	f := &prefilter{nodes: []acNode{{}}}
	for i, lit := range literals {
		at := 0
		for j := 0; j < len(lit); j++ {
			ch := lit[j]
			child, ok := f.nodes[at].next[ch]
			if !ok {
				child = len(f.nodes)
				if f.nodes[at].next == nil {
					f.nodes[at].next = make(map[byte]int)
				}
				f.nodes[at].next[ch] = child
				f.nodes = append(f.nodes, acNode{})
			}
			at = child
		}
		f.nodes[at].output |= uint64(1) << uint(groups[i])
	}
	queue := make([]int, 0, len(f.nodes))
	for _, child := range f.nodes[0].next {
		queue = append(queue, child)
	}
	for i := 0; i < len(queue); i++ {
		at := queue[i]
		for ch, child := range f.nodes[at].next {
			next := f.nodes[at].fail
			for next != 0 {
				if _, ok := f.nodes[next].next[ch]; ok {
					break
				}
				next = f.nodes[next].fail
			}
			if dest, ok := f.nodes[next].next[ch]; ok {
				f.nodes[child].fail = dest
			}
			f.nodes[child].output |= f.nodes[f.nodes[child].fail].output
			queue = append(queue, child)
		}
	}
	return f
}

func (f *prefilter) candidateGroups(input string) uint64 {
	at := 0
	var groups uint64
	for i := 0; i < len(input); i++ {
		ch := input[i]
		next, found := f.nodes[at].next[ch]
		for !found && at != 0 {
			at = f.nodes[at].fail
			next, found = f.nodes[at].next[ch]
		}
		if found {
			at = next
		} else {
			at = 0
		}
		groups |= f.nodes[at].output
	}
	return groups
}

func (f *prefilter) containsAny(input string) bool {
	return f.candidateGroups(input) != 0
}
