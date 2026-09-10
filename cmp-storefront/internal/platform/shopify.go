package platform

import (
	"fmt"
	"time"

	"github.com/shopline/cmp-storefront/internal/gql"
	"github.com/shopline/cmp-storefront/internal/scenario"
)

// Shopify targets the Storefront GraphQL API.
//
// Verified against the Storefront API reference (latest = 2026-07):
//   - query product(handle: String, id: ID) -- handle is current, not deprecated
//   - Product: id/title/handle/description/descriptionHtml/productType/vendor/
//     availableForSale/createdAt/updatedAt/variants/images all exist
//   - ProductVariant: id/title/sku/availableForSale exist; quantityAvailable
//     exists but is marked "token access required"
//   - Image: id/url/width/height/altText all exist
//   - Collection: id/title/handle/updatedAt/description/products all exist
//   - Every connection exposes `nodes` alongside `edges`
//   - customerAccessTokenCreate still present, flagged "for legacy customer
//     accounts only"
//
// Scope: Product and Collection need unauthenticated_read_product_listings;
// quantityAvailable additionally needs unauthenticated_read_product_inventory.
// The negotiator discovers which of these the token actually has.
type Shopify struct {
	Shop    string
	Version string
	Token   string
	Domain  string
	client  *gql.Client
	fm      *fieldMap
}

// Canonical field names were chosen to match Shopify's schema, so this
// platform needs no renames -- only negotiation-driven removals.
func NewShopify(shop, version, token, domain string, extra map[string]string, timeout time.Duration, identity bool) *Shopify {
	host := domain
	if host == "" {
		host = shop + ".myshopify.com"
	}
	s := &Shopify{Shop: shop, Version: version, Token: token, Domain: host, fm: newFieldMap(nil)}
	s.client = gql.NewClient(
		fmt.Sprintf("%s/api/%s/graphql.json", baseURL(host), version),
		mergeHeaders(map[string]string{"X-Shopify-Storefront-Access-Token": token}, extra),
		timeout, identity,
	)
	return s
}

func (s *Shopify) Name() string             { return "shopify" }
func (s *Shopify) Client() *gql.Client      { return s.client }
func (s *Shopify) Disable(canonical string) { s.fm.Disable(canonical) }
func (s *Shopify) DisabledFields() []string { return s.fm.Disabled() }

func (s *Shopify) Bind(sc scenario.Scenario, v Vars) (Bound, error) {
	return bind(sc, s.fm, sc.OpName(), varsFor(sc, v))
}

func (s *Shopify) Canonical(native string) string { return s.fm.canonical(native) }

func (s *Shopify) NativeNames(canonical map[string][]string) map[string][]string {
	return nativeNames(s.fm, canonical)
}

func (s *Shopify) TypeProbe() (string, []string) {
	return typeProbeQuery, []string{"Product", "ProductVariant", "Image", "Collection"}
}

// varsFor builds the variable set. It is shared by both adapters so the two
// requests carry byte-identical variables.
func varsFor(sc scenario.Scenario, v Vars) map[string]any {
	switch sc.Kind {
	case scenario.KindByProductHandle:
		m := map[string]any{"handle": v.ProductHandle}
		if usesFirst(sc) {
			m["n"] = v.PageSize
		}
		return m
	case scenario.KindByCollectionHandle:
		return map[string]any{"handle": v.CollectionHandle, "n": v.PageSize}
	case scenario.KindSearch:
		return map[string]any{"q": SearchFilter(v.SearchTerm), "n": v.PageSize}
	case scenario.KindStatic:
		return map[string]any{"n": v.PageSize}
	case scenario.KindLogin:
		return map[string]any{"input": map[string]any{"email": v.Email, "password": v.Password}}
	}
	return map[string]any{}
}

func usesFirst(sc scenario.Scenario) bool {
	var has bool
	var walk func(s *scenario.Sel)
	walk = func(s *scenario.Sel) {
		for _, a := range s.Args {
			if a.Var == "n" {
				has = true
			}
		}
		for _, c := range s.Children {
			walk(c)
		}
	}
	walk(sc.Root)
	return has
}

const typeProbeQuery = `query TypeProbe($name: String!) {
  __type(name: $name) {
    name
    fields(includeDeprecated: true) {
      name
      isDeprecated
      deprecationReason
    }
  }
}`
