package regexpset

import (
	"math/bits"
	"regexp/syntax"
	"sort"
)

type scratch struct {
	seen              []uint32
	generation        uint32
	stack, ready, raw []uint32
	matches, ids      []int
	matched           []bool
	bitset            []uint64
	touchedWords      []int
	transient         dfaState // caller-local uncached state, reused after each consumed input rune
}

// closure seeds each Unicode boundary with its memoized unanchored-start
// closure. The expensive epsilon walk from every pattern root is performed
// only once per observed empty-width context; all raw states are still
// explored for the current input position.
func (m *machine) closure(raw []uint32, ctx syntax.EmptyOp, sc *scratch) *dfaState {
	roots := m.startClosure(ctx)
	state := m.expand(raw, nil, ctx, sc)
	sc.ready = append(sc.ready, roots.ready...)
	sc.matches = append(sc.matches, roots.matches...)
	sc.ready = canonicalReady(sc.ready, sc)
	sort.Ints(sc.matches)
	sc.matches = uniqueInts(sc.matches)
	state.ready, state.matches = sc.ready, sc.matches
	return state
}

// startClosure is immutable after publication. Each context carries its own
// zero-width assertions, so an anchored start cannot be erroneously reused
// at a later position.
func (m *machine) startClosure(ctx syntax.EmptyOp) *dfaState {
	if found, ok := m.startCache.Load(ctx); ok {
		return found.(*dfaState)
	}
	m.startMu.Lock()
	defer m.startMu.Unlock()
	if found, ok := m.startCache.Load(ctx); ok {
		return found.(*dfaState)
	}
	scratch := &scratch{seen: make([]uint32, len(m.inst))}
	state := m.expand(nil, m.starts, ctx, scratch)
	state.ready = canonicalReady(state.ready, scratch)
	sort.Ints(state.matches)
	state.matches = uniqueInts(state.matches)
	m.startCache.Store(ctx, state)
	return state
}

func (m *machine) expand(raw, starts []uint32, ctx syntax.EmptyOp, sc *scratch) *dfaState {
	sc.generation++
	if sc.generation == 0 {
		clear(sc.seen)
		sc.generation = 1
	}
	sc.stack = sc.stack[:0]
	sc.ready = sc.ready[:0]
	sc.matches = sc.matches[:0]
	sc.stack = append(sc.stack, raw...)
	sc.stack = append(sc.stack, starts...)
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
			sc.matches = append(sc.matches, m.aliases[pc]...)
		case syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny, syntax.InstRuneAnyNotNL:
			sc.ready = append(sc.ready, pc)
		}
	}
	// The active uncached state lives in call-local scratch rather than
	// allocating one heap object per cache miss. advance consumes its old
	// frontier before the next closure overwrites this value.
	sc.transient = dfaState{ready: sc.ready, matches: sc.matches}
	return &sc.transient
}

// canonicalReady uses sorting for sparse frontiers and a word-indexed bitset
// for dense frontiers. Clearing only touched words keeps the dense path cheap.
func canonicalReady(ready []uint32, sc *scratch) []uint32 {
	if len(ready) < 128 {
		sort.Slice(ready, func(i, j int) bool { return ready[i] < ready[j] })
		return uniqueU32(ready)
	}
	var max uint32
	for _, pc := range ready {
		if pc > max {
			max = pc
		}
	}
	// With very sparse high instruction PCs, sorting is cheaper than allocating
	// a huge bitset. Avoid pathological scratch memory growth.
	words := int(max>>6) + 1
	if words > len(ready)*4 {
		sort.Slice(ready, func(i, j int) bool { return ready[i] < ready[j] })
		return uniqueU32(ready)
	}
	if cap(sc.bitset) < words {
		sc.bitset = make([]uint64, words)
	}
	sc.bitset = sc.bitset[:words]
	sc.touchedWords = sc.touchedWords[:0]
	for _, pc := range ready {
		idx := int(pc >> 6)
		if sc.bitset[idx] == 0 {
			sc.touchedWords = append(sc.touchedWords, idx)
		}
		sc.bitset[idx] |= uint64(1) << (pc & 63)
	}
	sort.Ints(sc.touchedWords)
	out := ready[:0]
	for _, idx := range sc.touchedWords {
		mask := sc.bitset[idx]
		for mask != 0 {
			bit := bits.TrailingZeros64(mask)
			out = append(out, uint32(idx*64+bit))
			mask &= mask - 1
		}
		sc.bitset[idx] = 0
	}
	return out
}

func uniqueU32(v []uint32) []uint32 {
	if len(v) == 0 {
		return v
	}
	out := v[:1]
	for _, value := range v[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}
func uniqueInts(v []int) []int {
	if len(v) == 0 {
		return v
	}
	out := v[:1]
	for _, value := range v[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
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
