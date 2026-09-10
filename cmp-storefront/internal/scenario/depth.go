package scenario

import "strings"

// plumbing names are Relay connection wrappers. They add a JSON nesting level
// on both platforms identically, so they are excluded from the semantic depth.
var plumbing = map[string]bool{"edges": true, "node": true, "nodes": true}

// SemanticDepth derives the depth of a GraphQL document by scanning its
// selection sets. It is deliberately a scanner rather than a full parser: the
// queries in this harness are hand-written and fixed, and the point is to fail
// loudly if someone edits one past the depth budget.
func SemanticDepth(query string) int {
	q := stripLiteralsAndComments(query)
	depth, maxDepth, paren := 0, 0, 0
	var stack []bool
	ident := strings.Builder{}
	pending := ""

	flush := func() {
		if ident.Len() > 0 {
			pending = ident.String()
			ident.Reset()
		}
	}

	for _, r := range q {
		switch {
		case r == '(':
			flush()
			paren++
		case r == ')':
			paren--
		case paren > 0:
			// Argument values may contain braces (input objects); ignore them.
		case isIdentRune(r):
			ident.WriteRune(r)
		case r == '{':
			flush()
			counted := len(stack) > 0 && !plumbing[pending]
			stack = append(stack, counted)
			if counted {
				depth++
				if depth > maxDepth {
					maxDepth = depth
				}
			}
			pending = ""
		case r == '}':
			flush()
			if n := len(stack); n > 0 {
				if stack[n-1] {
					depth--
				}
				stack = stack[:n-1]
			}
			pending = ""
		default:
			flush()
		}
	}
	return maxDepth
}

func isIdentRune(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

func stripLiteralsAndComments(s string) string {
	var b strings.Builder
	inStr, inCmt := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inCmt:
			if c == '\n' {
				inCmt = false
				b.WriteByte(c)
			}
		case inStr:
			if c == '\\' {
				i++
			} else if c == '"' {
				inStr = false
			}
		case c == '#':
			inCmt = true
		case c == '"':
			inStr = true
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
