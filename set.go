// Package regexpset matches many Go/RE2-compatible regular expressions in one
// pass. It compiles the patterns into a shared NFA and lazily caches DFA states.
package regexpset

import (
	"fmt"
	"regexp/syntax"
	"sort"
	"sync"
	"unicode/utf8"
)

// Options bounds the shared DFA cache. Zero values select defaults. Once a
// limit is reached, matching continues with uncached NFA-derived states.
type Options struct {
	MaxStates      int
	MaxTransitions int
}

// CacheStats describes currently retained (not temporary) DFA states/edges.
type CacheStats struct {
	States      int
	Transitions int
}

// Set is safe for concurrent matching. Patterns are indexed from zero in the
// order passed to Compile; only the existence of a match is reported.
type Set struct {
	inst     []syntax.Inst
	starts   []uint32
	matchIDs []int
	count    int
	limits   Options

	mu         sync.Mutex
	states     map[uint64][]*dfaState
	initial    map[syntax.EmptyOp]*dfaState
	stateCount int
	edgeCount  int
	pool       sync.Pool
}

type edgeKey struct {
	rune    rune
	context syntax.EmptyOp
}

type dfaState struct {
	ready   []uint32
	matches []int
	cached  bool
	mu      sync.RWMutex
	edges   map[edgeKey]*dfaState
}

type scratch struct {
	seen       []uint32
	generation uint32
	stack      []uint32
	ready      []uint32
	matches    []int
	raw        []uint32
	matched    []bool
	ids        []int
}

// Compile builds a set with bounded, shared DFA caching.
func Compile(patterns []string) (*Set, error) {
	return CompileWithOptions(patterns, Options{})
}

// CompileWithOptions accepts the regexp/syntax Perl (Go regexp) syntax.
// Invalid expressions are returned with their zero-based pattern index.
func CompileWithOptions(patterns []string, options Options) (*Set, error) {
	if options.MaxStates < 0 || options.MaxTransitions < 0 {
		return nil, fmt.Errorf("regexpset: cache limits must be non-negative")
	}
	if options.MaxStates == 0 {
		options.MaxStates = 4096
	}
	if options.MaxTransitions == 0 {
		options.MaxTransitions = 65536
	}

	s := &Set{count: len(patterns), limits: options,
		states: make(map[uint64][]*dfaState), initial: make(map[syntax.EmptyOp]*dfaState)}
	for id, expression := range patterns {
		parsed, err := syntax.Parse(expression, syntax.Perl)
		if err != nil {
			return nil, fmt.Errorf("regexpset: pattern %d: %w", id, err)
		}
		program, err := syntax.Compile(parsed.Simplify())
		if err != nil {
			return nil, fmt.Errorf("regexpset: pattern %d: %w", id, err)
		}
		// Instruction pointers are uint32, matching the syntax.Prog format.
		if uint64(len(s.inst))+uint64(len(program.Inst)) > uint64(^uint32(0)) {
			return nil, fmt.Errorf("regexpset: combined program is too large")
		}
		offset := uint32(len(s.inst))
		s.starts = append(s.starts, offset+uint32(program.Start))
		for _, instruction := range program.Inst {
			switch instruction.Op {
			case syntax.InstAlt, syntax.InstAltMatch:
				instruction.Out += offset
				instruction.Arg += offset
			case syntax.InstCapture, syntax.InstEmptyWidth, syntax.InstNop,
				syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny,
				syntax.InstRuneAnyNotNL:
				instruction.Out += offset
			}
			s.inst = append(s.inst, instruction)
			if instruction.Op == syntax.InstMatch {
				s.matchIDs = append(s.matchIDs, id)
			} else {
				s.matchIDs = append(s.matchIDs, -1)
			}
		}
	}
	s.pool.New = func() any { return &scratch{seen: make([]uint32, len(s.inst)), matched: make([]bool, s.count)} }
	return s, nil
}

// Len is the number of original expressions, including duplicates.
func (s *Set) Len() int { return s.count }

// CacheStats returns the number of retained states and transitions.
func (s *Set) CacheStats() CacheStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return CacheStats{States: s.stateCount, Transitions: s.edgeCount}
}

// MatchAny reports whether at least one pattern matches any substring of text.
func (s *Set) MatchAny(text string) bool {
	if s.count == 0 {
		return false
	}
	sc := s.pool.Get().(*scratch)
	defer s.pool.Put(sc)
	state, pos, prev := s.begin(text, sc)
	if len(state.matches) != 0 {
		return true
	}
	for pos < len(text) {
		r, width := utf8.DecodeRuneInString(text[pos:])
		pos += width
		next := rune(-1)
		if pos < len(text) {
			next, _ = utf8.DecodeRuneInString(text[pos:])
		}
		state = s.advance(state, r, syntax.EmptyOpContext(r, next), sc)
		if len(state.matches) != 0 {
			return true
		}
		prev = r
	}
	_ = prev
	return false
}

// MatchIDs returns all matching pattern indexes in ascending order, without
// duplicates. Its return value does not alias the set's cache.
func (s *Set) MatchIDs(text string) []int { return s.MatchInto(text, nil) }

// MatchInto writes sorted unique matching pattern indexes into dst[:0].
// Reusing dst reduces allocations for callers that retain result buffers.
func (s *Set) MatchInto(text string, dst []int) []int {
	dst = dst[:0]
	if s.count == 0 {
		return dst
	}
	sc := s.pool.Get().(*scratch)
	defer s.pool.Put(sc)
	sc.ids = sc.ids[:0]
	state, pos, _ := s.begin(text, sc)
	s.collect(sc, state.matches)
	for pos < len(text) && len(sc.ids) < s.count {
		r, width := utf8.DecodeRuneInString(text[pos:])
		pos += width
		next := rune(-1)
		if pos < len(text) {
			next, _ = utf8.DecodeRuneInString(text[pos:])
		}
		state = s.advance(state, r, syntax.EmptyOpContext(r, next), sc)
		s.collect(sc, state.matches)
	}
	sort.Ints(sc.ids)
	dst = append(dst, sc.ids...)
	for _, id := range sc.ids {
		sc.matched[id] = false
	}
	return dst
}

func (s *Set) collect(sc *scratch, ids []int) {
	for _, id := range ids {
		if !sc.matched[id] {
			sc.matched[id] = true
			sc.ids = append(sc.ids, id)
		}
	}
}

func (s *Set) begin(text string, sc *scratch) (*dfaState, int, rune) {
	next := rune(-1)
	if len(text) != 0 {
		next, _ = utf8.DecodeRuneInString(text)
	}
	ctx := syntax.EmptyOpContext(-1, next)
	s.mu.Lock()
	existing := s.initial[ctx]
	s.mu.Unlock()
	if existing != nil {
		return existing, 0, -1
	}
	state := s.closure(nil, ctx, sc)
	state = s.intern(state)
	if state.cached {
		s.mu.Lock()
		if s.initial[ctx] == nil {
			s.initial[ctx] = state
		} else {
			state = s.initial[ctx]
		}
		s.mu.Unlock()
	}
	return state, 0, -1
}

func (s *Set) advance(state *dfaState, r rune, ctx syntax.EmptyOp, sc *scratch) *dfaState {
	k := edgeKey{rune: r, context: ctx}
	if state.cached {
		state.mu.RLock()
		next := state.edges[k]
		state.mu.RUnlock()
		if next != nil {
			return next
		}
	}
	sc.raw = sc.raw[:0]
	for _, pc := range state.ready {
		inst := &s.inst[pc]
		if inst.MatchRune(r) {
			sc.raw = append(sc.raw, inst.Out)
		}
	}
	next := s.intern(s.closure(sc.raw, ctx, sc))
	if state.cached && next.cached {
		s.mu.Lock()
		if s.edgeCount < s.limits.MaxTransitions {
			state.mu.Lock()
			if state.edges == nil {
				state.edges = make(map[edgeKey]*dfaState)
			}
			if old := state.edges[k]; old != nil {
				next = old
			} else {
				state.edges[k] = next
				s.edgeCount++
			}
			state.mu.Unlock()
		}
		s.mu.Unlock()
	}
	return next
}

func (s *Set) closure(raw []uint32, ctx syntax.EmptyOp, sc *scratch) *dfaState {
	sc.generation++
	if sc.generation == 0 {
		clear(sc.seen)
		sc.generation = 1
	}
	sc.stack = sc.stack[:0]
	sc.ready = sc.ready[:0]
	sc.matches = sc.matches[:0]
	sc.stack = append(sc.stack, raw...)
	sc.stack = append(sc.stack, s.starts...)
	for len(sc.stack) != 0 {
		pc := sc.stack[len(sc.stack)-1]
		sc.stack = sc.stack[:len(sc.stack)-1]
		if sc.seen[pc] == sc.generation {
			continue
		}
		sc.seen[pc] = sc.generation
		inst := &s.inst[pc]
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
			sc.matches = append(sc.matches, s.matchIDs[pc])
		case syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny, syntax.InstRuneAnyNotNL:
			sc.ready = append(sc.ready, pc)
		}
	}
	// Canonicalize sets: equivalent NFA frontiers become one DFA state.
	sort.Slice(sc.ready, func(i, j int) bool { return sc.ready[i] < sc.ready[j] })
	sort.Ints(sc.matches)
	return &dfaState{ready: sc.ready, matches: sc.matches}
}

func hashState(state *dfaState) uint64 {
	const multiplier uint64 = 1099511628211
	h := uint64(14695981039346656037)
	for _, pc := range state.ready {
		h = (h ^ uint64(pc+1)) * multiplier
	}
	h = (h ^ uint64(0xffffffff)) * multiplier
	for _, id := range state.matches {
		h = (h ^ uint64(id+1)) * multiplier
	}
	return h
}

func equalState(a, b *dfaState) bool {
	if len(a.ready) != len(b.ready) || len(a.matches) != len(b.matches) {
		return false
	}
	for i, x := range a.ready {
		if x != b.ready[i] {
			return false
		}
	}
	for i, x := range a.matches {
		if x != b.matches[i] {
			return false
		}
	}
	return true
}

func (s *Set) intern(candidate *dfaState) *dfaState {
	h := hashState(candidate)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, state := range s.states[h] {
		if equalState(state, candidate) {
			return state
		}
	}
	// Even uncached states own their slices because scratch is reused.
	candidate.ready = append([]uint32(nil), candidate.ready...)
	candidate.matches = append([]int(nil), candidate.matches...)
	if s.stateCount < s.limits.MaxStates {
		candidate.cached = true
		s.states[h] = append(s.states[h], candidate)
		s.stateCount++
	}
	return candidate
}
