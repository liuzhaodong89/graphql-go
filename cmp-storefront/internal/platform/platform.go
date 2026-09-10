// Package platform adapts each storefront to the shared scenario contract.
//
// The adapters exist because the two schemas are NOT field-for-field identical
// (SHOPLINE has no plain-text description, its connection fields are
// variantsV2/imagesV2, and its inventory field is inventoryQuantity). The
// scenario layer speaks canonical paths; each adapter declares how its own
// schema realises them.
package platform

import (
	"strings"

	"github.com/shopline/cmp-storefront/internal/gql"
	"github.com/shopline/cmp-storefront/internal/scenario"
)

// Vars carries the per-sample inputs. The same Vars value is handed to both
// platforms for a paired sample, which is what makes the pair comparable.
type Vars struct {
	ProductHandle    string
	CollectionHandle string
	SearchTerm       string
	PageSize         int
	Email            string
	Password         string
}

// Bound is a scenario rendered for one platform.
type Bound struct {
	Query     string
	Operation string
	Variables map[string]any
	// PathAlias maps a canonical dotted path (as declared on the scenario) to
	// this platform's actual path in the response. Absent entries mean the
	// canonical path is already correct.
	PathAlias map[string]string
	// FieldRewrite is an ordered list of {from, to} path-prefix substitutions
	// applied to this platform's flattened leaf paths so that per-field byte
	// attribution lines the two responses up (e.g. variantsV2 -> variants).
	FieldRewrite [][2]string
}

// RewritePath maps one of this platform's response paths to the canonical form.
func (b Bound) RewritePath(p string) string {
	for _, r := range b.FieldRewrite {
		if strings.HasPrefix(p, r[0]) {
			return r[1] + p[len(r[0]):]
		}
	}
	return p
}

// Resolve translates a canonical path into this platform's response path.
func (b Bound) Resolve(canonical string) string {
	if b.PathAlias == nil {
		return canonical
	}
	if p, ok := b.PathAlias[canonical]; ok {
		return p
	}
	return canonical
}

type Platform interface {
	Name() string
	Client() *gql.Client
	Bind(s scenario.Scenario, v Vars) (Bound, error)
	// TypeProbe returns a schema query used by preflight to prove the selected
	// fields actually exist before any timing is collected.
	TypeProbe() (query string, typeNames []string)
	// NativeNames translates canonical type->field expectations into this
	// platform's own field names.
	NativeNames(canonical map[string][]string) map[string][]string
	// Canonical maps one of this platform's native field names back to the
	// canonical name. Server errors name native fields; removals are declared
	// canonically.
	Canonical(native string) string
	// Disable removes a canonical field from every query this platform renders.
	// Negotiation uses it to drop fields the platform does not implement; the
	// same field is then dropped on the other platform so both sides keep
	// asking for the same information.
	Disable(canonical string)
	DisabledFields() []string
}
