package regexpset

import (
	"errors"
	"regexp/syntax"
	"sort"
	"unicode/utf8"
)

// ErrStreamClosed is returned when writing after Finish or Close.
var ErrStreamClosed = errors.New("regexpset: stream already finished or closed")

// Stream incrementally scans one logical input using the same NFA/lazy-DFA
// machinery as Set. It does not buffer the complete input and never treats a
// Write boundary as a word or end-of-text boundary. Stream is not concurrent
// safe; distinct streams of the same Set are independent and concurrent safe.
//
// Streams intentionally do not use the AC prefilter (which requires a complete
// input) or exact-regexp candidate mode. All compiled machine groups run.
// Finish must be called (or Close to abandon results) to release scratch.
// Stream retains at most an incomplete UTF-8 rune between writes.
type Stream struct {
	machines   []streamMachine
	tails      []byte
	hasPending bool
	pending    rune
	finished   bool
	result     []int
}

type streamMachine struct {
	m       *machine
	scratch *scratch
	state   *dfaState
}

// NewStream begins an incremental match for exactly one complete input.
func (s *Set) NewStream() *Stream {
	stream := &Stream{machines: make([]streamMachine, len(s.groups))}
	for i, m := range s.groups {
		stream.machines[i] = streamMachine{m: m, scratch: m.pool.Get().(*scratch)}
		stream.machines[i].scratch.ids = stream.machines[i].scratch.ids[:0]
	}
	return stream
}

func (s *Stream) accept(r rune) {
	if !s.hasPending {
		for i := range s.machines {
			mm := &s.machines[i]
			mm.state = mm.m.begin(r, mm.scratch)
			mm.m.collect(mm.scratch, mm.state.matches)
		}
		s.hasPending = true
		s.pending = r
		return
	}
	for i := range s.machines {
		mm := &s.machines[i]
		mm.state = mm.m.advance(mm.state, s.pending, syntax.EmptyOpContext(s.pending, r), mm.scratch)
		mm.m.collect(mm.scratch, mm.state.matches)
	}
	s.pending = r
}

// Write consumes bytes incrementally. A split UTF-8 character is not decoded
// until the remaining bytes arrive, preserving Go regexp's UTF-8 semantics.
func (s *Stream) Write(p []byte) (int, error) {
	if s.finished {
		return 0, ErrStreamClosed
	}
	n := len(p)
	at := 0
	// Complete the at-most-three-byte suffix from the previous Write before
	// consuming the new chunk. No full-chunk copy is necessary.
	for len(s.tails) > 0 {
		if !utf8.FullRune(s.tails) {
			if at == len(p) {
				return n, nil
			}
			s.tails = append(s.tails, p[at])
			at++
			continue
		}
		r, w := utf8.DecodeRune(s.tails)
		s.accept(r)
		s.tails = s.tails[w:]
	}
	for at < len(p) {
		if !utf8.FullRune(p[at:]) {
			s.tails = append(s.tails, p[at:]...)
			break
		}
		r, w := utf8.DecodeRune(p[at:])
		s.accept(r)
		at += w
	}
	return n, nil
}

// Finish signals the actual end of the input, resolving end anchors and the
// final pending rune. It returns sorted matching pattern indexes, without
// duplicates. It is idempotent; callers own the result. Writing afterward
// returns ErrStreamClosed.
func (s *Stream) Finish() []int {
	if s.finished {
		return append([]int(nil), s.result...)
	}
	for len(s.tails) > 0 {
		r, w := utf8.DecodeRune(s.tails)
		s.accept(r)
		s.tails = s.tails[w:]
	}
	s.tails = nil
	if !s.hasPending {
		for i := range s.machines {
			mm := &s.machines[i]
			mm.state = mm.m.begin(-1, mm.scratch)
			mm.m.collect(mm.scratch, mm.state.matches)
		}
	} else {
		for i := range s.machines {
			mm := &s.machines[i]
			mm.state = mm.m.advance(mm.state, s.pending, syntax.EmptyOpContext(s.pending, -1), mm.scratch)
			mm.m.collect(mm.scratch, mm.state.matches)
		}
	}
	for i := range s.machines {
		mm := &s.machines[i]
		s.result = append(s.result, mm.scratch.ids...)
	}
	sort.Ints(s.result)
	s.Close()
	return append([]int(nil), s.result...)
}

// Close discards unfinished input and releases per-stream scratch storage.
func (s *Stream) Close() {
	if s.finished {
		return
	}
	s.finished = true
	s.tails = nil
	for i := range s.machines {
		mm := &s.machines[i]
		for _, id := range mm.scratch.ids {
			mm.scratch.matched[id] = false
		}
		mm.scratch.ids = mm.scratch.ids[:0]
		mm.m.pool.Put(mm.scratch)
		mm.scratch = nil
		mm.state = nil
	}
}

// RuleStream evaluates priority-ordered AND/OR rules over an incrementally
// supplied input. Each regex pattern is matched with one shared streaming Set.
// Results are finalized only after Finish, since an earlier-priority AND rule
// may not become satisfied until the final chunk.
type RuleStream struct {
	rules    *RuleSet
	stream   *Stream
	finished bool
	ids      []uint32
}

// NewStream creates an incremental matcher for the original flat RuleSet API.
func (s *RuleSet) NewStream() *RuleStream {
	return &RuleStream{rules: s, stream: s.set.NewStream()}
}

func (s *RuleStream) Write(p []byte) (int, error) { return s.stream.Write(p) }

// Finish returns all satisfied rule IDs, in declaration-priority order.
func (s *RuleStream) Finish() []uint32 {
	if s.finished {
		return append([]uint32(nil), s.ids...)
	}
	patterns := s.stream.Finish()
	sc := s.rules.pool.Get().(*ruleScratch)
	for _, p := range patterns {
		index := s.rules.patternToRule[p]
		if sc.hits[index] == 0 {
			sc.touched = append(sc.touched, index)
		}
		sc.hits[index]++
	}
	sort.Ints(sc.touched)
	for _, index := range sc.touched {
		if s.rules.satisfied(index, sc.hits[index]) {
			s.ids = append(s.ids, s.rules.rules[index].id)
		}
	}
	s.rules.release(sc)
	s.finished = true
	return append([]uint32(nil), s.ids...)
}

// Close discards unfinished input and its scratch data.
func (s *RuleStream) Close() { s.stream.Close() }
