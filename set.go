// Package regexpset matches multiple Go/RE2 regular expressions while returning
// the original expression indexes. Programs are combined into Thompson NFAs,
// and equivalent frontiers are lazily cached as DFA states.
package regexpset

import (
	"fmt"
	"hash/fnv"
	"math/bits"
	"regexp"
	"regexp/syntax"
	"sort"
)

// Options bounds the total number of retained DFA states and transitions.
// Zero values select defaults; neither limit is a bound on total process memory.
type Options struct {
	MaxStates      int
	MaxTransitions int
	// MaxCacheBytes limits estimated retained DFA state/edge reservations.
	// Zero means no byte budget; compiled NFAs, AC and scratch are excluded.
	MaxCacheBytes int64
	// DisablePrefilter disables conservative mandatory-literal extraction.
	// The default uses an Aho-Corasick rejection filter for eligible rules.
	DisablePrefilter bool
	// DisableCandidateShards keeps one eligible-pattern DFA machine rather
	// than partitioning into multiple AC-selected candidate groups. This can
	// improve throughput on high-hit or high-cardinality inputs.
	DisableCandidateShards bool
	// DisableExactCandidates forces full shared-machine evaluation even when a
	// small number of individual patterns pass their necessary-literal filter.
	DisableExactCandidates bool
	// ResetCacheOnFull enables whole-generation cache recycling. It trades
	// extra compilation work for adaptability when input distributions change.
	ResetCacheOnFull bool
	// EvictLRU enables per-state LRU under capacity pressure (opt-in).
	EvictLRU bool
}

// CacheStats reports retained deterministic states and transitions across both
// engines. ASCIIClasses is the number of input equivalence classes in use.
type CacheStats struct {
	States         int
	Transitions    int
	Bytes          int64
	UnicodeClasses int
	ASCIIClasses   int
	Resets         int
	Evictions      int
}

// Set is immutable except for synchronized caches and safe for concurrent use.
// Each expression is assigned its original zero-based index, even for duplicates.
type Set struct {
	count            int
	groups           []*machine
	candidates       *prefilter
	groupBits        []uint64         // zero means unconditional, otherwise requires matching AC bucket
	patternGroupBits []uint64         // one safe necessary candidate bit per pattern (0: unconditional)
	exactPatterns    []*regexp.Regexp // compiled verifier for safe eligible-pattern candidates
	exactEligible    int
	disableExact     bool
}

// Compile compiles patterns using bounded caches and safe literal prefiltering.
func Compile(patterns []string) (*Set, error) { return CompileWithOptions(patterns, Options{}) }

// CompileWithOptions compiles Go regexp syntax (regexp/syntax.Perl). Matches
// are substring/existence matches, as in regexp.Regexp.MatchString.
func CompileWithOptions(patterns []string, opts Options) (*Set, error) {
	if opts.EvictLRU && opts.ResetCacheOnFull {
		return nil, fmt.Errorf("regexpset: EvictLRU and ResetCacheOnFull are mutually exclusive")
	}
	if opts.MaxStates < 0 || opts.MaxTransitions < 0 || opts.MaxCacheBytes < 0 {
		return nil, fmt.Errorf("regexpset: cache limits must be non-negative")
	}
	if opts.MaxStates == 0 {
		opts.MaxStates = 4096
	}
	if opts.MaxTransitions == 0 {
		opts.MaxTransitions = 65536
	}

	var regular, eligible []compiledPattern
	for id, source := range patterns {
		tree, err := syntax.Parse(source, syntax.Perl)
		if err != nil {
			return nil, fmt.Errorf("regexpset: pattern %d: %w", id, err)
		}
		p := compiledPattern{id: id, tree: tree.Simplify()}
		if !opts.DisablePrefilter {
			p.literal = mandatoryASCII(p.tree)
		}
		if p.literal == "" {
			regular = append(regular, p)
		} else {
			eligible = append(eligible, p)
		}
	}
	result := &Set{count: len(patterns), patternGroupBits: make([]uint64, len(patterns)), disableExact: opts.DisableExactCandidates}
	var gs []patternGroup
	if len(regular) > 0 {
		gs = append(gs, patternGroup{patterns: regular})
	}
	if len(eligible) > 0 {
		// For large sets, partition eligible patterns into independently cached
		// machines. The AC output tells us which machines to run. This avoids
		// making DFA state identity depend on per-input candidate bitmaps.
		shards := (len(eligible) + 511) / 512
		if opts.DisableCandidateShards {
			shards = 1
		}
		if shards < 1 {
			shards = 1
		}
		if shards > 16 {
			shards = 16
		}
		buckets := make([][]compiledPattern, shards)
		literals := make([]string, 0, len(eligible))
		ids := make([]int, 0, len(eligible))
		assigned := make([]int, 0, len(eligible))
		for _, p := range eligible {
			h := fnv.New32a()
			_, _ = h.Write([]byte(p.literal))
			bucket := int(h.Sum32() % uint32(shards))
			buckets[bucket] = append(buckets[bucket], p)
		}
		// Allocate compact candidate IDs (avoid empty buckets).
		for _, bucket := range buckets {
			if len(bucket) == 0 {
				continue
			}
			index := len(gs) - 0 // actual Set.groups index, regular comes first
			for _, p := range bucket {
				literals = append(literals, p.literal)
				assigned = append(assigned, index)
				ids = append(ids, p.id)
			}
			gs = append(gs, patternGroup{patterns: bucket, filter: true})
		}
		result.candidates = newPrefilterPatterns(literals, assigned, ids, len(patterns))
		result.exactEligible = len(eligible)
		if !opts.DisableExactCandidates {
			result.exactPatterns = make([]*regexp.Regexp, len(patterns))
			for _, p := range eligible {
				compiled, err := regexp.Compile(patterns[p.id])
				if err != nil {
					return nil, fmt.Errorf("regexpset: candidate %d: %w", p.id, err)
				}
				result.exactPatterns[p.id] = compiled
			}
		}
	}
	stateBudgets := splitBudget(opts.MaxStates, gs)
	edgeBudgets := splitBudget(opts.MaxTransitions, gs)
	var byteBudgets []int
	if opts.MaxCacheBytes > 0 {
		// Int is at least 64-bit on mainstream targets used in CI; clamp on
		// 32-bit systems rather than overflow when splitting.
		maxInt := uint64(^uint(0) >> 1)
		budget := opts.MaxCacheBytes
		if uint64(budget) > maxInt {
			budget = int64(maxInt)
		}
		byteBudgets = splitBudget(int(budget), gs)
	}
	for i, g := range gs {
		bytes := int64(0)
		if byteBudgets != nil {
			bytes = int64(byteBudgets[i])
			if bytes == 0 {
				bytes = -1
			} // group cannot retain any state
		}
		m := newMachine(len(patterns), len(g.patterns), stateBudgets[i], edgeBudgets[i], bytes, opts.ResetCacheOnFull, opts.EvictLRU)
		for _, p := range g.patterns {
			if g.filter {
				result.patternGroupBits[p.id] = uint64(1) << uint(i)
			}
			if err := m.add(p.id, p.tree); err != nil {
				return nil, fmt.Errorf("regexpset: pattern %d: %w", p.id, err)
			}
		}
		m.finish()
		result.groups = append(result.groups, m)
		if g.filter {
			result.groupBits = append(result.groupBits, uint64(1)<<uint(i))
		} else {
			result.groupBits = append(result.groupBits, 0)
		}
	}
	return result, nil
}

type compiledPattern struct {
	id      int
	tree    *syntax.Regexp
	literal string
}
type patternGroup struct {
	patterns []compiledPattern
	filter   bool
}

// splitBudget apportions the entire limit; the final group receives any
// remainder. An internal allocation of zero explicitly disables caching.
func splitBudget(total int, groups []patternGroup) []int {
	allocated := make([]int, len(groups))
	remaining, remainingRules := total, 0
	for _, g := range groups {
		remainingRules += len(g.patterns)
	}
	for i, g := range groups {
		n := len(g.patterns)
		if i == len(groups)-1 {
			allocated[i] = remaining
		} else {
			high, low := bits.Mul64(uint64(remaining), uint64(n))
			part, _ := bits.Div64(high, low, uint64(remainingRules))
			allocated[i] = int(part)
		}
		remaining -= allocated[i]
		remainingRules -= n
	}
	return allocated
}

// Len returns the number of compiled expressions.
func (s *Set) Len() int { return s.count }

// MatchAny reports whether at least one pattern matches the input.
func (s *Set) MatchAny(input string) bool {
	return s.visitMatches(input, func(int) bool { return true })
}

// MatchIDs returns matching pattern indexes in ascending order.
func (s *Set) MatchIDs(input string) []int { return s.MatchInto(input, nil) }

func (s *Set) MatchInto(input string, dst []int) []int {
	dst = dst[:0]
	s.visitMatches(input, func(id int) bool { dst = append(dst, id); return false })
	sort.Ints(dst)
	return dst
}

func (s *Set) candidateMask(input string) uint64 {
	if s.candidates != nil {
		return s.candidates.candidateGroups(input)
	}
	return ^uint64(0)
}
func (s *Set) visitMatches(input string, emit func(int) bool) bool {
	return s.visitWithMask(input, s.candidateMask(input), emit)
}

// visitWithMask streams distinct matching IDs. Sparse AC outputs are verified
// directly by precompiled Go regexp; dense candidates use the shared DFA.
// Candidate sets never enter DFA state identity, so cache entries stay valid.
func (s *Set) visitWithMask(input string, mask uint64, emit func(int) bool) bool {
	return s.visitWithCandidates(input, mask, nil, emit)
}

// visitWithCandidates accepts an optional already-scanned candidate snapshot,
// so RuleSet priority decisions and verification share the same AC pass.
func (s *Set) visitWithCandidates(input string, mask uint64, candidates *candidateScratch, emit func(int) bool) bool {
	owned := false
	useExact := false
	if s.candidates != nil && !s.disableExact && mask != 0 {
		if candidates == nil {
			candidates = s.candidates.candidatePatterns(input)
			owned = true
		}
		n := len(candidates.ids)
		useExact = n > 0 && n <= 16 && n*8 <= s.exactEligible
	}
	if owned {
		defer s.candidates.releaseCandidates(candidates)
	}
	for i, g := range s.groups {
		if s.groupBits[i] != 0 && (mask&s.groupBits[i] == 0 || useExact) {
			continue
		}
		if g.visit(input, emit) {
			return true
		}
	}
	if useExact {
		for _, id := range candidates.ids {
			if s.exactPatterns[id].MatchString(input) && emit(id) {
				return true
			}
		}
	}
	return false
}

// CacheStats returns an aggregate snapshot of retained cache entries.
func (s *Set) CacheStats() CacheStats {
	var stats CacheStats
	for _, g := range s.groups {
		v := g.cacheStats()
		stats.States += v.States
		stats.Transitions += v.Transitions
		stats.Bytes += v.Bytes
		stats.UnicodeClasses += v.UnicodeClasses
		stats.ASCIIClasses += v.ASCIIClasses
		stats.Resets += v.Resets
		stats.Evictions += v.Evictions
	}
	return stats
}
