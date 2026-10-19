// Package regexpset matches multiple Go/RE2 regular expressions while returning
// the original expression indexes. Programs are combined into Thompson NFAs,
// and equivalent frontiers are lazily cached as DFA states.
package regexpset

import (
	"fmt"
	"math/bits"
	"regexp/syntax"
	"sort"
)

// Options bounds the total number of retained DFA states and transitions.
// Zero values select defaults; neither limit is a bound on total process memory.
type Options struct {
	MaxStates      int
	MaxTransitions int
	// DisablePrefilter disables conservative mandatory-literal extraction.
	// The default uses an Aho-Corasick rejection filter for eligible rules.
	DisablePrefilter bool
	// ResetCacheOnFull enables whole-generation cache recycling. It trades
	// extra compilation work for adaptability when input distributions change.
	ResetCacheOnFull bool
}

// CacheStats reports retained deterministic states and transitions across both
// engines. ASCIIClasses is the number of input equivalence classes in use.
type CacheStats struct {
	States       int
	Transitions  int
	ASCIIClasses int
	Resets       int
}

// Set is immutable except for synchronized caches and safe for concurrent use.
// Each expression is assigned its original zero-based index, even for duplicates.
type Set struct {
	count  int
	groups []*machine
}

// Compile compiles patterns using bounded caches and safe literal prefiltering.
func Compile(patterns []string) (*Set, error) { return CompileWithOptions(patterns, Options{}) }

// CompileWithOptions compiles Go regexp syntax (regexp/syntax.Perl). Matches
// are substring/existence matches, as in regexp.Regexp.MatchString.
func CompileWithOptions(patterns []string, opts Options) (*Set, error) {
	if opts.MaxStates < 0 || opts.MaxTransitions < 0 {
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
	result := &Set{count: len(patterns)}
	var gs []patternGroup
	if len(regular) != 0 {
		gs = append(gs, patternGroup{patterns: regular})
	}
	if len(eligible) != 0 {
		gs = append(gs, patternGroup{patterns: eligible, filter: true})
	}

	// Budget is shared across the engines, including when either limit is 1.
	// The engine with the most rules gets the remainder of each budget.
	stateBudgets := splitBudget(opts.MaxStates, gs)
	edgeBudgets := splitBudget(opts.MaxTransitions, gs)
	for i, g := range gs {
		m := newMachine(len(patterns), len(g.patterns), stateBudgets[i], edgeBudgets[i], opts.ResetCacheOnFull)
		if g.filter {
			literals := make([]string, len(g.patterns))
			for j, p := range g.patterns {
				literals[j] = p.literal
			}
			m.prefilter = newPrefilter(literals)
		}
		for _, p := range g.patterns {
			if err := m.add(p.id, p.tree); err != nil {
				return nil, fmt.Errorf("regexpset: pattern %d: %w", p.id, err)
			}
		}
		m.finish()
		result.groups = append(result.groups, m)
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
	for _, g := range s.groups {
		if g.matchAny(input) {
			return true
		}
	}
	return false
}

// MatchIDs returns matching pattern indexes in ascending order, once per rule.
func (s *Set) MatchIDs(input string) []int { return s.MatchInto(input, nil) }

// MatchInto appends matching indexes to dst[:0]; dst may be reused by callers.
// It never aliases internal cached storage.
func (s *Set) MatchInto(input string, dst []int) []int {
	dst = dst[:0]
	for _, g := range s.groups {
		dst = g.matchInto(input, dst)
	}
	sort.Ints(dst)
	return dst
}

// CacheStats returns an aggregate snapshot of retained cache entries.
func (s *Set) CacheStats() CacheStats {
	var stats CacheStats
	for _, g := range s.groups {
		v := g.cacheStats()
		stats.States += v.States
		stats.Transitions += v.Transitions
		stats.ASCIIClasses += v.ASCIIClasses
		stats.Resets += v.Resets
	}
	return stats
}
