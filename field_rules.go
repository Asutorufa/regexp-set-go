package regexpset

import (
	"fmt"
	"sort"
	"sync"
)

// ExprOp is the operation represented by a field-aware logical expression.
// A leaf (ExprPattern) searches one named string field using Go regexp syntax.
// AND/OR/NOT combine children recursively and short-circuit during evaluation.
type ExprOp uint8

const (
	ExprPattern ExprOp = iota
	ExprAnd
	ExprOr
	ExprNot
)

// Expr is a recursive field-aware Boolean expression. Pattern leaves have
// Field and Pattern set. Logical nodes only have Op and Children set.
// Missing fields do not match, including an empty regular expression.
// The empty string is a valid field value and may match zero-width patterns.
type Expr struct {
	Op       ExprOp
	Field    string
	Pattern  string
	Children []Expr
}

// FieldRule is a priority-ordered routing/classification rule. Earlier rules
// take priority regardless of ID. IDs must be distinct (zero is permitted).
type FieldRule struct {
	ID   uint32
	When Expr
}

type compiledExpr struct {
	op       ExprOp
	field    int
	leaf     int
	children []compiledExpr
}

type compiledFieldRule struct {
	id   uint32
	root compiledExpr
}

type fieldMatcher struct {
	name    string
	set     *Set
	leafIDs []int // indexes into FieldRuleSet's global leaf namespace
}

// FieldRuleSet compiles all regex leaves for each field into one Set, rather
// than one automaton per Rule or Expr. Evaluating a field is lazy and cached
// for the duration of MatchFirstRule / MatchRuleIDs. The same instance is safe
// for concurrent matching; a caller must not mutate fields concurrently.
// Note that cache limits in Options currently apply PER DISTINCT FIELD.
type FieldRuleSet struct {
	rules  []compiledFieldRule
	fields []fieldMatcher
	leaves int
	pool   sync.Pool
}

type fieldScratch struct {
	fieldsSeen    []bool
	leavesMatched []bool
	matchedIDs    []int
}

// CompileFieldRules compiles nested AND/OR/NOT rules against named input fields.
func CompileFieldRules(rules []FieldRule) (*FieldRuleSet, error) {
	return CompileFieldRulesWithOptions(rules, Options{})
}

// CompileFieldRulesWithOptions accepts the same engine options as Compile.
// Each field's regexes share one Set. The fields are passed at query time.
func CompileFieldRulesWithOptions(rules []FieldRule, opts Options) (*FieldRuleSet, error) {
	s := &FieldRuleSet{}
	index := make(map[string]int)
	seenRule := make(map[uint32]struct{}, len(rules))
	patterns := make([][]string, 0)
	var compile func(e Expr, loc string) (compiledExpr, error)
	compile = func(e Expr, loc string) (compiledExpr, error) {
		out := compiledExpr{op: e.Op}
		switch e.Op {
		case ExprPattern:
			if e.Field == "" || len(e.Children) != 0 {
				return out, fmt.Errorf("regexpset: %s: pattern requires field and no children", loc)
			}
			idx, exists := index[e.Field]
			if !exists {
				idx = len(s.fields)
				index[e.Field] = idx
				s.fields = append(s.fields, fieldMatcher{name: e.Field})
				patterns = append(patterns, nil)
			}
			out.field = idx
			out.leaf = s.leaves
			s.leaves++
			patterns[idx] = append(patterns[idx], e.Pattern)
			s.fields[idx].leafIDs = append(s.fields[idx].leafIDs, out.leaf)
		case ExprAnd, ExprOr, ExprNot:
			if e.Field != "" || e.Pattern != "" {
				return out, fmt.Errorf("regexpset: %s: logical node may not contain field or pattern", loc)
			}
			if e.Op == ExprNot && len(e.Children) != 1 {
				return out, fmt.Errorf("regexpset: %s: NOT requires exactly one child", loc)
			}
			if (e.Op == ExprAnd || e.Op == ExprOr) && len(e.Children) < 2 {
				return out, fmt.Errorf("regexpset: %s: AND/OR require at least two children", loc)
			}
			out.children = make([]compiledExpr, 0, len(e.Children))
			for i, child := range e.Children {
				c, err := compile(child, fmt.Sprintf("%s.child[%d]", loc, i))
				if err != nil {
					return out, err
				}
				out.children = append(out.children, c)
			}
		default:
			return out, fmt.Errorf("regexpset: %s: invalid expression op %d", loc, e.Op)
		}
		return out, nil
	}
	for i, r := range rules {
		if _, ok := seenRule[r.ID]; ok {
			return nil, fmt.Errorf("regexpset: field rule %d: duplicate ID %d", i, r.ID)
		}
		seenRule[r.ID] = struct{}{}
		node, err := compile(r.When, fmt.Sprintf("rule[%d]", i))
		if err != nil {
			return nil, err
		}
		s.rules = append(s.rules, compiledFieldRule{id: r.ID, root: node})
	}
	for i := range s.fields {
		matcher, err := CompileWithOptions(patterns[i], opts)
		if err != nil {
			return nil, fmt.Errorf("regexpset: field %q: %w", s.fields[i].name, err)
		}
		s.fields[i].set = matcher
	}
	s.pool.New = func() any {
		return &fieldScratch{fieldsSeen: make([]bool, len(s.fields)), leavesMatched: make([]bool, s.leaves)}
	}
	return s, nil
}

func (s *FieldRuleSet) Len() int          { return len(s.rules) }
func (s *FieldRuleSet) PatternCount() int { return s.leaves }

// evaluate applies the compiled Boolean AST. Each existing field is scanned
// no more than once per Match call, however many leaf conditions refer to it.
func (s *FieldRuleSet) evaluate(e *compiledExpr, fields map[string]string, sc *fieldScratch) bool {
	switch e.op {
	case ExprPattern:
		if !sc.fieldsSeen[e.field] {
			fm := &s.fields[e.field]
			text, ok := fields[fm.name]
			sc.fieldsSeen[e.field] = true
			if ok {
				sc.matchedIDs = fm.set.MatchInto(text, sc.matchedIDs[:0])
				for _, id := range sc.matchedIDs {
					sc.leavesMatched[fm.leafIDs[id]] = true
				}
			}
		}
		return sc.leavesMatched[e.leaf]
	case ExprAnd:
		for i := range e.children {
			if !s.evaluate(&e.children[i], fields, sc) {
				return false
			}
		}
		return true
	case ExprOr:
		for i := range e.children {
			if s.evaluate(&e.children[i], fields, sc) {
				return true
			}
		}
		return false
	case ExprNot:
		return !s.evaluate(&e.children[0], fields, sc)
	}
	return false
}

func (s *FieldRuleSet) getScratch() *fieldScratch { return s.pool.Get().(*fieldScratch) }
func (s *FieldRuleSet) release(sc *fieldScratch) {
	clear(sc.fieldsSeen)
	clear(sc.leavesMatched)
	sc.matchedIDs = sc.matchedIDs[:0]
	s.pool.Put(sc)
}

// MatchFirstRule returns the highest-priority satisfied rule. Only fields
// relevant to expressions visited during priority evaluation are scanned.
func (s *FieldRuleSet) MatchFirstRule(fields map[string]string) (uint32, bool) {
	if len(s.rules) == 0 {
		return 0, false
	}
	sc := s.getScratch()
	defer s.release(sc)
	for i := range s.rules {
		if s.evaluate(&s.rules[i].root, fields, sc) {
			return s.rules[i].id, true
		}
	}
	return 0, false
}
func (s *FieldRuleSet) MatchAny(fields map[string]string) bool {
	_, ok := s.MatchFirstRule(fields)
	return ok
}

// MatchRuleIDs returns all matching rule IDs in input priority order.
func (s *FieldRuleSet) MatchRuleIDs(fields map[string]string) []uint32 {
	return s.MatchRuleIDsInto(fields, nil)
}
func (s *FieldRuleSet) MatchRuleIDsInto(fields map[string]string, dst []uint32) []uint32 {
	dst = dst[:0]
	if len(s.rules) == 0 {
		return dst
	}
	sc := s.getScratch()
	defer s.release(sc)
	for i := range s.rules {
		if s.evaluate(&s.rules[i].root, fields, sc) {
			dst = append(dst, s.rules[i].id)
		}
	}
	return dst
}

// Fields returns the names of compiled input fields in sorted order.
func (s *FieldRuleSet) Fields() []string {
	out := make([]string, 0, len(s.fields))
	for _, f := range s.fields {
		out = append(out, f.name)
	}
	sort.Strings(out)
	return out
}
