package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/shopline/cmp-storefront/internal/platform"
	"github.com/shopline/cmp-storefront/internal/scenario"
)

// Negotiation records what a scenario looks like after both platforms have
// agreed on a field set they can both serve.
type Negotiation struct {
	Scenario string            `json:"scenario"`
	Runnable bool              `json:"runnable"`
	Blocked  string            `json:"blocked_reason,omitempty"`
	Removed  []string          `json:"removed_fields,omitempty"`
	Reasons  map[string]string `json:"removal_reasons,omitempty"`
	Leaves   []string          `json:"leaf_fields,omitempty"`
}

// Field-not-found messages differ per implementation; these cover the shapes
// both storefronts emit.
var fieldErrPatterns = []*regexp.Regexp{
	regexp.MustCompile(`Cannot query field ['"]([A-Za-z_][A-Za-z0-9_]*)['"]`),
	regexp.MustCompile(`Field ['"]([A-Za-z_][A-Za-z0-9_]*)['"] doesn't exist`),
	regexp.MustCompile(`Unknown field ['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?`),
	regexp.MustCompile(`undefinedField.*['"]([A-Za-z_][A-Za-z0-9_]*)['"]`),
}

var accessErrPattern = regexp.MustCompile(`(?i)(access denied|not approved|requires? .*scope|unauthorized)`)

// Negotiate discovers, per scenario, the field set both platforms can serve,
// and removes anything only one of them supports.
//
// This exists because two published schemas are not enough to write the query
// pair by hand: SHOPLINE's Image type is not in the public docs, Shopify's
// quantityAvailable silently returns null without the inventory scope, and the
// customer-token mutation is only live on stores still using legacy customer
// accounts. Guessing any of those produces either a one-sided failure (which
// discards every pair) or a one-sided null (which quietly opens a size gap).
//
// A field is only ever removed from BOTH platforms, so the two sides keep
// asking for the same information.
func Negotiate(ctx context.Context, ps []platform.Platform, scs []scenario.Scenario, v platform.Vars) []Negotiation {
	var out []Negotiation

	for _, sc := range scs {
		n := Negotiation{Scenario: sc.ID, Runnable: true, Reasons: map[string]string{}}

		// Bounded: each round removes at least one field, and the field set is
		// finite.
		maxRounds := len(scenario.AllScalars(sc.Root)) + 2
		for round := 0; round < maxRounds; round++ {
			var (
				drop     string
				dropWhy  string
				blocked  string
				nullOnly = map[string]int{}
				probed   int
			)

			for _, p := range ps {
				b, err := p.Bind(sc, v)
				if err != nil {
					blocked = fmt.Sprintf("%s: %v", p.Name(), err)
					break
				}
				s := Exec(ctx, p, sc, v, "negotiate", false)
				probed++

				if s.OK {
					// Detect fields that resolve to null on this platform but
					// carry data on the other: an unrequested asymmetry that
					// would land straight in the size delta.
					for _, f := range nullLeafFields(ctx, p, sc, v, b) {
						nullOnly[f]++
					}
					continue
				}

				switch s.FailKind {
				case FailGraphQL:
					if native, ok := extractField(s.FailMsg); ok {
						// The server names its own field; optionality is
						// declared canonically.
						f := p.Canonical(native)
						if sc.IsOptional(f) {
							drop, dropWhy = f, fmt.Sprintf("%s: %s", p.Name(), trim(s.FailMsg))
						} else {
							blocked = fmt.Sprintf("%s requires %q which %s rejects: %s",
								sc.ID, f, p.Name(), trim(s.FailMsg))
						}
					} else if accessErrPattern.MatchString(s.FailMsg) {
						blocked = fmt.Sprintf("%s: access denied -- %s", p.Name(), trim(s.FailMsg))
					} else {
						blocked = fmt.Sprintf("%s: %s", p.Name(), trim(s.FailMsg))
					}
				case FailMissing, FailNullData:
					blocked = fmt.Sprintf("%s: %s (%s)", p.Name(), s.FailKind, trim(s.FailMsg))
				case FailThrottled:
					blocked = fmt.Sprintf("%s: throttled during negotiation; lower the configured rate", p.Name())
				default:
					blocked = fmt.Sprintf("%s: %s (%s)", p.Name(), s.FailKind, trim(s.FailMsg))
				}
				if drop != "" || blocked != "" {
					break
				}
			}

			if blocked != "" {
				n.Runnable, n.Blocked = false, blocked
				break
			}
			if drop != "" {
				for _, p := range ps {
					p.Disable(drop)
				}
				n.Removed = append(n.Removed, drop)
				n.Reasons[drop] = dropWhy
				continue
			}

			// Everything succeeded. Drop optional fields that only one platform
			// actually populates.
			if len(ps) > 1 {
				var asym string
				for f, c := range nullOnly {
					if c > 0 && c < len(ps) && sc.IsOptional(f) {
						asym = f
						break
					}
				}
				if asym != "" {
					for _, p := range ps {
						p.Disable(asym)
					}
					n.Removed = append(n.Removed, asym)
					n.Reasons[asym] = "returned null on one platform only; dropping it keeps the payloads comparable"
					continue
				}
			}
			break
		}

		if n.Runnable && len(ps) > 0 {
			if b, err := ps[0].Bind(sc, v); err == nil {
				n.Leaves = leafPaths(b)
			}
		}
		sort.Strings(n.Removed)
		out = append(out, n)
	}
	return out
}

// nullLeafFields re-runs the bound query and reports which selected leaf fields
// came back null everywhere they appear.
func nullLeafFields(ctx context.Context, p platform.Platform, sc scenario.Scenario, v platform.Vars, b platform.Bound) []string {
	s := Exec(ctx, p, sc, v, "negotiate-null", true)
	if !s.OK || len(s.Fields) == 0 {
		return nil
	}
	// Fields carrying only the key name (no value bytes beyond null) surface as
	// very small entries; instead, re-parse to check values directly.
	raw := s.RawData
	if len(raw) == 0 {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	var out []string
	for _, canonical := range scenario.AllScalars(sc.Root) {
		if !sc.IsOptional(canonical) {
			continue
		}
		if allNull(decoded, canonical) {
			out = append(out, canonical)
		}
	}
	return out
}

// allNull reports whether every occurrence of a leaf key in the tree is null.
// Returns false when the key does not appear at all.
func allNull(v any, key string) bool {
	found, nonNull := false, false
	var walk func(any)
	walk = func(cur any) {
		switch t := cur.(type) {
		case map[string]any:
			for k, sub := range t {
				if k == key {
					found = true
					if sub != nil {
						nonNull = true
					}
					continue
				}
				walk(sub)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(v)
	return found && !nonNull
}

func leafPaths(b platform.Bound) []string {
	var out []string
	for canonical, platformPath := range b.PathAlias {
		if canonical != platformPath {
			out = append(out, canonical+" -> "+platformPath)
		}
	}
	sort.Strings(out)
	return out
}

func extractField(msg string) (string, bool) {
	for _, re := range fieldErrPatterns {
		if m := re.FindStringSubmatch(msg); len(m) > 1 {
			return m[1], true
		}
	}
	return "", false
}

func trim(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 160 {
		return s[:157] + "..."
	}
	return s
}
