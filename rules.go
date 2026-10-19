package regexpset

import (
	"fmt"
	"regexp/syntax"
	"sort"
	"sync"
)

// RuleOperator determines how a rule combines its regular expressions.
// RuleOR is the zero value: one matching expression satisfies the rule.
// RuleAND requires every expression to match somewhere in the same input.
// Expressions are independent searches; their matches need not overlap.
type RuleOperator uint8

const (
	RuleOR RuleOperator = iota
	RuleAND
)

// Rule is a logical group of expressions. ID is an application-defined,
// unique identifier; input order defines priority (earlier is higher).
// Patterns retain their individual identities, even when their text is equal.
type Rule struct {
	ID       uint32
	Operator RuleOperator
	Patterns []string
}

type compiledRule struct {
	id           uint32
	operator     RuleOperator
	patterns     int
	firstPattern int // start of contiguous pattern indexes for this rule
}

// RuleSet matches all expressions with one underlying RegexSet, then evaluates
// AND/OR groups in priority order. It is safe to share between goroutines.
// It does not own or execute an action associated with a matched rule.
type RuleSet struct {
	set           *Set
	rules         []compiledRule
	patternToRule []int
	pool          sync.Pool
}

type ruleScratch struct {
	hits    []int
	touched []int
}

// CompileRules compiles rules using the default engine limits. Rule order is
// priority order: rules[0] has the highest priority.
func CompileRules(rules []Rule) (*RuleSet, error) {
	return CompileRulesWithOptions(rules, Options{})
}

// CompileRulesWithOptions shares the same underlying NFA/DFA, prefilter and
// cache budgets as CompileWithOptions. Invalid groups and duplicate IDs are
// rejected; invalid expressions include both rule and pattern indices.
func CompileRulesWithOptions(rules []Rule, options Options) (*RuleSet, error) {
	result := &RuleSet{
		rules: make([]compiledRule, 0, len(rules)),
	}
	seenIDs := make(map[uint32]int, len(rules))
	var patterns []string
	for index, rule := range rules {
		if previous, exists := seenIDs[rule.ID]; exists {
			return nil, fmt.Errorf("regexpset: rule %d: duplicate ID %d (already used by rule %d)", index, rule.ID, previous)
		}
		seenIDs[rule.ID] = index
		if rule.Operator != RuleOR && rule.Operator != RuleAND {
			return nil, fmt.Errorf("regexpset: rule %d: invalid operator %d", index, rule.Operator)
		}
		if len(rule.Patterns) == 0 {
			return nil, fmt.Errorf("regexpset: rule %d: no patterns", index)
		}
		result.rules = append(result.rules, compiledRule{
			id: rule.ID, operator: rule.Operator, patterns: len(rule.Patterns), firstPattern: len(patterns),
		})
		for _, pattern := range rule.Patterns {
			patterns = append(patterns, pattern)
			result.patternToRule = append(result.patternToRule, index)
		}
	}
	engine, err := CompileWithOptions(patterns, options)
	if err != nil {
		// Validate on the error path only, so normal compilation parses every
		// expression just once while failures identify the original location.
		for ruleIndex, rule := range rules {
			for patternIndex, pattern := range rule.Patterns {
				if _, parseErr := syntax.Parse(pattern, syntax.Perl); parseErr != nil {
					return nil, fmt.Errorf("regexpset: rule %d (ID %d) pattern %d: %w", ruleIndex, rule.ID, patternIndex, parseErr)
				}
			}
		}
		return nil, fmt.Errorf("regexpset: compile rules: %w", err)
	}
	result.set = engine
	result.pool.New = func() any {
		return &ruleScratch{hits: make([]int, len(result.rules))}
	}
	return result, nil
}

// Len returns the number of logical rules, not the number of expressions.
func (s *RuleSet) Len() int { return len(s.rules) }

// PatternCount reports the total number of regular expressions in the set.
func (s *RuleSet) PatternCount() int { return len(s.patternToRule) }

// CacheStats reports the shared engine's retained cache usage.
func (s *RuleSet) CacheStats() CacheStats { return s.set.CacheStats() }

// MatchAny reports whether any logical rule is satisfied.
func (s *RuleSet) MatchAny(input string) bool {
	if len(s.rules) == 0 {
		return false
	}
	sc := s.pool.Get().(*ruleScratch)
	defer s.release(sc)
	found := false
	s.countMatchesUntil(sc, input, func(index int) bool {
		if s.satisfied(index, sc.hits[index]) {
			found = true
			return true
		}
		return false
	})
	return found
}

// MatchFirstRule returns the highest-priority satisfied rule. Candidate AC
// masks make a rule provably impossible when an AND member (or every OR
// member) belongs to an excluded mandatory-literal shard. As soon as the
// earliest still-possible rule is complete, no later input can change the
// winner, so scanning can end without sacrificing priority.
func (s *RuleSet) MatchFirstRule(input string) (uint32, bool) {
	if len(s.rules) == 0 {
		return 0, false
	}
	candidates := s.set.candidateMask(input)
	sc := s.pool.Get().(*ruleScratch)
	defer s.release(sc)
	// Defer looking for the earliest viable rule until a rule actually
	// matches. No-match workloads avoid scanning all logical rules here.
	earliest := -1
	s.countMatchesWithMask(sc, input, candidates, func(index int) bool {
		if !s.satisfied(index, sc.hits[index]) {
			return false
		}
		if earliest == -1 {
			earliest = s.firstPossibleRule(candidates)
		}
		return earliest == index
	})
	best := len(s.rules)
	for _, index := range sc.touched {
		if index < best && s.satisfied(index, sc.hits[index]) {
			best = index
		}
	}
	if best == len(s.rules) {
		return 0, false
	}
	return s.rules[best].id, true
}

// rulePossible never declares a matching rule impossible: filtered patterns
// have a necessary AC substring, while unfiltered patterns are always viable.
func (s *RuleSet) rulePossible(index int, candidates uint64) bool {
	rule := s.rules[index]
	if rule.operator == RuleAND {
		for p := rule.firstPattern; p < rule.firstPattern+rule.patterns; p++ {
			mask := s.set.patternGroupBits[p]
			if mask != 0 && candidates&mask == 0 {
				return false
			}
		}
		return true
	}
	for p := rule.firstPattern; p < rule.firstPattern+rule.patterns; p++ {
		mask := s.set.patternGroupBits[p]
		if mask == 0 || candidates&mask != 0 {
			return true
		}
	}
	return false
}

func (s *RuleSet) firstPossibleRule(candidates uint64) int {
	for index := range s.rules {
		if s.rulePossible(index, candidates) {
			return index
		}
	}
	return len(s.rules)
}

// MatchRuleIDs returns every satisfied rule ID in priority order (not numeric
// ID order). Each logical rule appears at most once.
func (s *RuleSet) MatchRuleIDs(input string) []uint32 {
	return s.MatchRuleIDsInto(input, nil)
}

// MatchRuleIDsInto writes all satisfied rule IDs into dst[:0]. The caller may
// reuse dst on subsequent calls; the result never aliases internal storage.
func (s *RuleSet) MatchRuleIDsInto(input string, dst []uint32) []uint32 {
	dst = dst[:0]
	if len(s.rules) == 0 {
		return dst
	}
	sc := s.pool.Get().(*ruleScratch)
	defer s.release(sc)
	s.countMatches(sc, input)
	// Only the touched rules can match. Sorting indexes restores explicit
	// priority without iterating over thousands of unmatched logical rules.
	sort.Ints(sc.touched)
	for _, index := range sc.touched {
		if s.satisfied(index, sc.hits[index]) {
			dst = append(dst, s.rules[index].id)
		}
	}
	return dst
}

func (s *RuleSet) satisfied(index, hits int) bool {
	if s.rules[index].operator == RuleAND {
		return hits == s.rules[index].patterns
	}
	return hits > 0
}

func (s *RuleSet) countMatches(sc *ruleScratch, input string) {
	s.countMatchesUntil(sc, input, nil)
}

// A rule is complete as soon as its last required pattern arrives. For any
// existence query we can stop on the first complete rule. For first-priority
// matching, early exit is safe only when rule index zero is complete.
func (s *RuleSet) countMatchesUntil(sc *ruleScratch, input string, stop func(int) bool) {
	s.countMatchesWithMask(sc, input, s.set.candidateMask(input), stop)
}

func (s *RuleSet) countMatchesWithMask(sc *ruleScratch, input string, candidates uint64, stop func(int) bool) {
	s.set.visitWithMask(input, candidates, func(patternID int) bool {
		index := s.patternToRule[patternID]
		if sc.hits[index] == 0 {
			sc.touched = append(sc.touched, index)
		}
		sc.hits[index]++
		return stop != nil && stop(index)
	})
}

func (s *RuleSet) release(sc *ruleScratch) {
	for _, index := range sc.touched {
		sc.hits[index] = 0
	}
	sc.touched = sc.touched[:0]
	s.pool.Put(sc)
}
