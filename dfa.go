package regexpset

import (
	"regexp/syntax"
	"sync"
	"unicode/utf8"
)

type machine struct {
	inst      []syntax.Inst
	starts    []uint32
	matchIDs  []int
	count     int // number of original rules (ID namespace)
	ruleCount int // number of rules in this NFA
	prefilter *prefilter

	maxStates, maxTransitions int
	resetOnFull               bool
	epoch                     uint64
	resets                    int
	asciiClass                [128]uint8
	asciiClasses              int

	mu                    sync.Mutex
	states                map[uint64][]*dfaState
	initial               map[syntax.EmptyOp]*dfaState
	stateCount, edgeCount int
	pool                  sync.Pool
}

type edgeKey struct {
	symbol  int32
	context syntax.EmptyOp
}

type dfaState struct {
	ready   []uint32
	matches []int
	cached  bool
	epoch   uint64
	mu      sync.RWMutex
	edges   map[edgeKey]*dfaState
}

func newMachine(count, rules, maxStates, maxTransitions int, resetOnFull bool) *machine {
	return &machine{
		count: count, ruleCount: rules, maxStates: maxStates,
		maxTransitions: maxTransitions,
		resetOnFull:    resetOnFull,
		states:         make(map[uint64][]*dfaState),
		initial:        make(map[syntax.EmptyOp]*dfaState),
	}
}

func (m *machine) cacheStats() CacheStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return CacheStats{States: m.stateCount, Transitions: m.edgeCount, ASCIIClasses: m.asciiClasses, Resets: m.resets}
}

// nextRune follows regexp's malformed UTF-8 behavior (RuneError, width 1).
func nextRune(text string, pos int) rune {
	if pos >= len(text) {
		return -1
	}
	if text[pos] < utf8.RuneSelf {
		return rune(text[pos])
	}
	r, _ := utf8.DecodeRuneInString(text[pos:])
	return r
}

func consumeRune(text string, pos int) (rune, int) {
	if text[pos] < utf8.RuneSelf {
		return rune(text[pos]), 1
	}
	return utf8.DecodeRuneInString(text[pos:])
}

func (m *machine) matchAny(text string) bool {
	if m.prefilter != nil && !m.prefilter.containsAny(text) {
		return false
	}
	sc := m.pool.Get().(*scratch)
	defer m.pool.Put(sc)
	state := m.begin(nextRune(text, 0), sc)
	if len(state.matches) != 0 {
		return true
	}
	for pos := 0; pos < len(text); {
		r, width := consumeRune(text, pos)
		pos += width
		state = m.advance(state, r, syntax.EmptyOpContext(r, nextRune(text, pos)), sc)
		if len(state.matches) != 0 {
			return true
		}
	}
	return false
}

func (m *machine) matchInto(text string, dst []int) []int {
	if m.prefilter != nil && !m.prefilter.containsAny(text) {
		return dst
	}
	sc := m.pool.Get().(*scratch)
	defer m.pool.Put(sc)
	sc.ids = sc.ids[:0]
	state := m.begin(nextRune(text, 0), sc)
	m.collect(sc, state.matches)
	for pos := 0; pos < len(text) && len(sc.ids) < m.ruleCount; {
		r, width := consumeRune(text, pos)
		pos += width
		state = m.advance(state, r, syntax.EmptyOpContext(r, nextRune(text, pos)), sc)
		m.collect(sc, state.matches)
	}
	dst = append(dst, sc.ids...)
	for _, id := range sc.ids {
		sc.matched[id] = false
	}
	return dst
}

func (m *machine) begin(next rune, sc *scratch) *dfaState {
	ctx := syntax.EmptyOpContext(-1, next)
	m.mu.Lock()
	state := m.initial[ctx]
	m.mu.Unlock()
	if state != nil {
		return state
	}
	state = m.intern(m.closure(nil, ctx, sc))
	if state.cached {
		m.mu.Lock()
		if state.epoch != m.epoch {
			m.mu.Unlock()
			return state
		}
		if old := m.initial[ctx]; old != nil {
			state = old
		} else {
			m.initial[ctx] = state
		}
		m.mu.Unlock()
	}
	return state
}

func (m *machine) symbol(r rune) int32 {
	if r >= 0 && r < 128 {
		return int32(m.asciiClass[r])
	}
	return int32(128 + r) // no overlap with ASCII equivalence-class IDs
}

func (m *machine) advance(state *dfaState, r rune, ctx syntax.EmptyOp, sc *scratch) *dfaState {
	key := edgeKey{symbol: m.symbol(r), context: ctx}
	if state.cached {
		state.mu.RLock()
		found := state.edges[key]
		state.mu.RUnlock()
		if found != nil {
			return found
		}
	}
	sc.raw = sc.raw[:0]
	for _, pc := range state.ready {
		inst := &m.inst[pc]
		if inst.MatchRune(r) {
			sc.raw = append(sc.raw, inst.Out)
		}
	}
	next := m.intern(m.closure(sc.raw, ctx, sc))
	if state.cached && next.cached {
		m.mu.Lock()
		if m.edgeCount >= m.maxTransitions && m.resetOnFull && m.maxTransitions > 0 {
			m.resetLocked()
		}
		if m.edgeCount < m.maxTransitions && state.epoch == m.epoch && next.epoch == m.epoch {
			state.mu.Lock()
			if state.edges == nil {
				state.edges = make(map[edgeKey]*dfaState)
			}
			if old := state.edges[key]; old != nil {
				next = old
			} else {
				state.edges[key] = next
				m.edgeCount++
			}
			state.mu.Unlock()
		}
		m.mu.Unlock()
	}
	return next
}

func hashState(state *dfaState) uint64 {
	const prime uint64 = 1099511628211
	hash := uint64(14695981039346656037)
	for _, pc := range state.ready {
		hash = (hash ^ uint64(pc+1)) * prime
	}
	hash = (hash ^ uint64(0xffffffff)) * prime
	for _, id := range state.matches {
		hash = (hash ^ uint64(id+1)) * prime
	}
	return hash
}

func equalState(a, b *dfaState) bool {
	if len(a.ready) != len(b.ready) || len(a.matches) != len(b.matches) {
		return false
	}
	for i, pc := range a.ready {
		if pc != b.ready[i] {
			return false
		}
	}
	for i, id := range a.matches {
		if id != b.matches[i] {
			return false
		}
	}
	return true
}

func (m *machine) intern(candidate *dfaState) *dfaState {
	// Uncached states can borrow the current match's scratch slices; they
	// remain valid until the next advance, which consumes them before closure.
	if m.maxStates == 0 {
		return candidate
	}
	key := hashState(candidate)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.states[key] {
		if equalState(candidate, existing) {
			return existing
		}
	}
	if m.stateCount >= m.maxStates && m.resetOnFull {
		m.resetLocked()
	}
	if m.stateCount < m.maxStates {
		candidate.epoch = m.epoch
		candidate.ready = append([]uint32(nil), candidate.ready...)
		candidate.matches = append([]int(nil), candidate.matches...)
		candidate.cached = true
		m.states[key] = append(m.states[key], candidate)
		m.stateCount++
	}
	return candidate
}

// resetLocked disconnects the cache without mutating existing states. Readers
// already holding states from prior generations may finish safely; stale edges
// remain correct but are not inserted into the new generation.
func (m *machine) resetLocked() {
	m.states = make(map[uint64][]*dfaState)
	m.initial = make(map[syntax.EmptyOp]*dfaState)
	m.stateCount, m.edgeCount = 0, 0
	m.epoch++
	m.resets++
}
