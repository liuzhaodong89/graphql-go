package platform

import (
	"sort"
	"strings"

	"github.com/shopline/cmp-storefront/internal/scenario"
)

// fieldMap is a platform's canonical->native field naming, plus the set of
// fields negotiation has removed.
type fieldMap struct {
	rename   map[string]string
	reverse  map[string]string
	disabled map[string]bool
}

func newFieldMap(rename map[string]string) *fieldMap {
	rev := make(map[string]string, len(rename))
	for canonical, native := range rename {
		rev[native] = canonical
	}
	return &fieldMap{rename: rename, reverse: rev, disabled: map[string]bool{}}
}

// canonical maps a native field name back to the canonical one. Server errors
// name the platform's own field, while optionality and removals are declared
// canonically, so negotiation cannot act on an error without this.
func (f *fieldMap) canonical(native string) string {
	if c, ok := f.reverse[native]; ok {
		return c
	}
	return native
}

func (f *fieldMap) mapper() scenario.Mapper {
	return func(canonical string) (string, bool) {
		if f.disabled[canonical] {
			return "", false
		}
		if n, ok := f.rename[canonical]; ok {
			return n, true
		}
		return canonical, true
	}
}

// Disable removes a canonical field from every rendered query on this platform.
func (f *fieldMap) Disable(canonical string) { f.disabled[canonical] = true }

func (f *fieldMap) Disabled() []string {
	out := make([]string, 0, len(f.disabled))
	for k := range f.disabled {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// bind renders a scenario for one platform and derives the path aliases and
// attribution rewrites from the naming map, so a rename is declared once.
func bind(sc scenario.Scenario, fm *fieldMap, opName string, vars map[string]any) (Bound, error) {
	m := fm.mapper()
	q, err := scenario.Render(sc.Op, opName, sc.Root, m)
	if err != nil {
		return Bound{}, err
	}
	pm := scenario.PathMap(sc.Root, m)

	alias := map[string]string{}
	var rewrites [][2]string
	for canonical, platform := range pm {
		if canonical == platform {
			continue
		}
		alias[canonical] = platform
		rewrites = append(rewrites, [2]string{platform, canonical})
	}
	// Longest platform path first so a nested rename is not shadowed by its
	// parent's prefix.
	sort.Slice(rewrites, func(i, j int) bool { return len(rewrites[i][0]) > len(rewrites[j][0]) })

	return Bound{
		Query: q, Operation: opName, Variables: vars,
		PathAlias: alias, FieldRewrite: rewrites,
	}, nil
}

// SearchFilter builds the structured filter expression used by S7. Both
// platforms document the same grammar for the products `query` argument, so a
// structured predicate is comparable in a way free-text relevance search is not.
func SearchFilter(term string) string {
	return "title:'" + strings.ReplaceAll(term, "'", "") + "'"
}

// nativeNames translates a canonical type->fields expectation into the names
// this platform actually uses, so the schema probe can be declared once.
func nativeNames(fm *fieldMap, canonical map[string][]string) map[string][]string {
	m := fm.mapper()
	out := make(map[string][]string, len(canonical))
	for typ, fields := range canonical {
		var names []string
		for _, f := range fields {
			if n, ok := m(f); ok {
				names = append(names, n)
			}
		}
		out[typ] = names
	}
	return out
}

// baseURL allows the configured domain to carry an explicit scheme. A bare
// host gets https; "http://127.0.0.1:8080" is used verbatim, which is what
// makes the adapters drivable against a local stub or a staging environment
// that is not on TLS.
func baseURL(host string) string {
	if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
		return strings.TrimSuffix(host, "/")
	}
	return "https://" + host
}

// mergeHeaders layers configured extras over the platform's own auth header.
// An extra may not shadow the auth header: silently replacing it would turn
// every request into an anonymous one and report the resulting 401s as latency.
func mergeHeaders(base, extra map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range extra {
		out[k] = v
	}
	for k, v := range base {
		out[k] = v
	}
	return out
}
