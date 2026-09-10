package scenario

import (
	"fmt"
	"sort"
	"strings"
)

// Arg is one GraphQL argument, always passed through a variable so the query
// text stays byte-identical across samples.
type Arg struct {
	Name    string // argument name on the field, e.g. "handle"
	Var     string // variable name, e.g. "handle" -> $handle
	VarType string // GraphQL type, e.g. "String!"
}

// Sel is a selection expressed in CANONICAL field names. Both platforms render
// their query from the same Sel tree, which is what guarantees the two requests
// ask for the same information in the same shape -- something two hand-written
// query strings can silently drift out of.
type Sel struct {
	Field    string
	Args     []Arg
	Conn     bool // Relay connection: children go inside `nodes { ... }`
	Scalars  []string
	Children []*Sel
}

// Mapper translates a canonical field name into the platform's own name.
// Returning ok=false drops the field, which is how capability negotiation
// removes a field a platform does not implement.
type Mapper func(canonical string) (name string, ok bool)

// Render produces the query text. Scalars keep their declared order so the
// request bytes stay stable between runs.
func Render(op, opName string, root *Sel, m Mapper) (string, error) {
	var body strings.Builder
	vars := map[string]string{}
	if err := renderSel(&body, root, m, 1, vars); err != nil {
		return "", err
	}

	var head strings.Builder
	head.WriteString(op)
	head.WriteString(" ")
	head.WriteString(opName)
	if len(vars) > 0 {
		names := make([]string, 0, len(vars))
		for n := range vars {
			names = append(names, n)
		}
		sort.Strings(names)
		head.WriteString("(")
		for i, n := range names {
			if i > 0 {
				head.WriteString(", ")
			}
			fmt.Fprintf(&head, "$%s: %s", n, vars[n])
		}
		head.WriteString(")")
	}
	head.WriteString(" {\n")
	head.WriteString(body.String())
	head.WriteString("}")
	return head.String(), nil
}

func renderSel(b *strings.Builder, s *Sel, m Mapper, depth int, vars map[string]string) error {
	name, ok := m(s.Field)
	if !ok {
		return fmt.Errorf("field %q is not available on this platform", s.Field)
	}
	pad := strings.Repeat("  ", depth)

	b.WriteString(pad)
	b.WriteString(name)
	if len(s.Args) > 0 {
		b.WriteString("(")
		for i, a := range s.Args {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(b, "%s: $%s", a.Name, a.Var)
			vars[a.Var] = a.VarType
		}
		b.WriteString(")")
	}
	b.WriteString(" {\n")

	inner := depth + 1
	innerPad := strings.Repeat("  ", inner)
	if s.Conn {
		b.WriteString(innerPad + "nodes {\n")
		inner++
		innerPad = strings.Repeat("  ", inner)
	}

	for _, sc := range s.Scalars {
		n, ok := m(sc)
		if !ok {
			continue // negotiated away on this platform
		}
		b.WriteString(innerPad + n + "\n")
	}
	for _, child := range s.Children {
		if err := renderSel(b, child, m, inner, vars); err != nil {
			return err
		}
	}

	if s.Conn {
		b.WriteString(strings.Repeat("  ", inner-1) + "}\n")
	}
	b.WriteString(pad + "}\n")
	return nil
}

// Leaves lists every canonical scalar the tree selects, in path form. Both
// platforms must produce the same list or the size gate is comparing different
// amounts of information.
func Leaves(s *Sel, m Mapper) []string {
	var out []string
	var walk func(cur *Sel, prefix string)
	walk = func(cur *Sel, prefix string) {
		if _, ok := m(cur.Field); !ok {
			return
		}
		p := prefix + cur.Field
		if cur.Conn {
			p += ".nodes"
		}
		for _, sc := range cur.Scalars {
			if _, ok := m(sc); !ok {
				continue
			}
			out = append(out, p+"."+sc)
		}
		for _, c := range cur.Children {
			walk(c, p+".")
		}
	}
	walk(s, "")
	return out
}

// AllScalars lists every canonical scalar in the tree regardless of mapping,
// which is the starting point for capability negotiation.
func AllScalars(s *Sel) []string {
	var out []string
	var walk func(cur *Sel)
	walk = func(cur *Sel) {
		out = append(out, cur.Scalars...)
		for _, c := range cur.Children {
			walk(c)
		}
	}
	walk(s)
	return out
}

// PathMap returns canonical response path -> platform response path for every
// node and leaf in the tree. The adapters use it to resolve required paths and
// to fold platform names back onto canonical ones for byte attribution, so a
// rename like variants->variantsV2 never has to be maintained by hand.
func PathMap(s *Sel, m Mapper) map[string]string {
	out := map[string]string{}
	var walk func(cur *Sel, canonPrefix, platPrefix string)
	walk = func(cur *Sel, canonPrefix, platPrefix string) {
		name, ok := m(cur.Field)
		if !ok {
			return
		}
		cp := canonPrefix + cur.Field
		pp := platPrefix + name
		out[cp] = pp
		if cur.Conn {
			cp += ".nodes"
			pp += ".nodes"
			out[cp] = pp
		}
		for _, sc := range cur.Scalars {
			n, ok := m(sc)
			if !ok {
				continue
			}
			out[cp+"."+sc] = pp + "." + n
		}
		for _, c := range cur.Children {
			walk(c, cp+".", pp+".")
		}
	}
	walk(s, "", "")
	return out
}
