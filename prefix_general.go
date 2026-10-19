package regexpset

import "regexp/syntax"

// splitGuaranteedPrefix separates a deterministic, case-sensitive starting
// literal from a regexp AST without changing its accepted language. It
// understands concatenation, captures, alternation and positive repetition.
// Constructs with no guaranteed consuming prefix are left intact.
// Captures can be erased because this engine reports existence, not spans.
func splitGuaranteedPrefix(r *syntax.Regexp) ([]rune, *syntax.Regexp) {
	switch r.Op {
	case syntax.OpLiteral:
		if r.Flags&syntax.FoldCase != 0 || len(r.Rune) == 0 {
			break
		}
		return append([]rune(nil), r.Rune...), &syntax.Regexp{Op: syntax.OpEmptyMatch}
	case syntax.OpCapture:
		return splitGuaranteedPrefix(r.Sub[0])
	case syntax.OpConcat:
		var prefix []rune
		residual := make([]*syntax.Regexp, 0, len(r.Sub))
		for i, child := range r.Sub {
			p, tail := splitGuaranteedPrefix(child)
			prefix = append(prefix, p...)
			if tail.Op != syntax.OpEmptyMatch {
				residual = append(residual, tail)
				residual = append(residual, r.Sub[i+1:]...)
				return prefix, concatRegex(residual...)
			}
			if len(p) == 0 { // empty match is safe to pass over
				// An assertion or other opaque node is not an empty match.
				if child.Op != syntax.OpEmptyMatch {
					residual = append(residual, child)
					residual = append(residual, r.Sub[i+1:]...)
					return prefix, concatRegex(residual...)
				}
			}
		}
		return prefix, &syntax.Regexp{Op: syntax.OpEmptyMatch}
	case syntax.OpAlternate:
		if len(r.Sub) == 0 {
			break
		}
		prefixes := make([][]rune, len(r.Sub))
		tails := make([]*syntax.Regexp, len(r.Sub))
		for i, child := range r.Sub {
			prefixes[i], tails[i] = splitGuaranteedPrefix(child)
		}
		common := len(prefixes[0])
		for _, p := range prefixes[1:] {
			if len(p) < common {
				common = len(p)
			}
		}
		for i := 0; i < common; i++ {
			value := prefixes[0][i]
			same := true
			for _, p := range prefixes[1:] {
				if p[i] != value {
					same = false
					break
				}
			}
			if !same {
				common = i
				break
			}
		}
		if common == 0 {
			break
		}
		alts := make([]*syntax.Regexp, len(tails))
		for i, tail := range tails {
			alts[i] = concatRegex(literalRegex(prefixes[i][common:]), tail)
		}
		return append([]rune(nil), prefixes[0][:common]...), (&syntax.Regexp{Op: syntax.OpAlternate, Sub: alts}).Simplify()
	case syntax.OpPlus, syntax.OpRepeat:
		min, max := 1, -1
		if r.Op == syntax.OpRepeat {
			min, max = r.Min, r.Max
		}
		if min <= 0 {
			break
		}
		p, tail := splitGuaranteedPrefix(r.Sub[0])
		if len(p) == 0 {
			break
		}
		remaining := &syntax.Regexp{Op: syntax.OpRepeat, Min: min - 1, Max: max, Sub: []*syntax.Regexp{r.Sub[0]}}
		if max >= 0 {
			remaining.Max = max - 1
		}
		// A repeat with min==0 is still permitted to consume the child again.
		return p, concatRegex(tail, remaining)
	}
	return nil, r
}

func literalRegex(r []rune) *syntax.Regexp {
	if len(r) == 0 {
		return &syntax.Regexp{Op: syntax.OpEmptyMatch}
	}
	return &syntax.Regexp{Op: syntax.OpLiteral, Rune: append([]rune(nil), r...)}
}

func concatRegex(parts ...*syntax.Regexp) *syntax.Regexp {
	filtered := make([]*syntax.Regexp, 0, len(parts))
	for _, r := range parts {
		if r == nil || r.Op == syntax.OpEmptyMatch {
			continue
		}
		if r.Op == syntax.OpConcat {
			filtered = append(filtered, r.Sub...)
		} else {
			filtered = append(filtered, r)
		}
	}
	switch len(filtered) {
	case 0:
		return &syntax.Regexp{Op: syntax.OpEmptyMatch}
	case 1:
		return filtered[0]
	default:
		return (&syntax.Regexp{Op: syntax.OpConcat, Sub: filtered}).Simplify()
	}
}
