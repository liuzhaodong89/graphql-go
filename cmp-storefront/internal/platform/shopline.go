package platform

import (
	"fmt"
	"time"

	"github.com/shopline/cmp-storefront/internal/gql"
	"github.com/shopline/cmp-storefront/internal/scenario"
)

// Shopline targets the SHOPLINE Storefront GraphQL API.
//
// Verified from the SHOPLINE developer docs:
//   - POST https://{handle}.myshopline.com/storefront/graph/{version}/graphql.json
//   - Authorization: Bearer <storefront access token>
//   - version format v20250301 (date-stamped, v-prefixed)
//   - query product(handle, id) and query collection(handle, id) both exist
//   - Product has descriptionHtml but NO plain description
//   - Product.variants / Product.images are DEPRECATED plain arrays;
//     variantsV2 / imagesV2 are the connection forms
//   - ProductVariant has inventoryQuantity, NOT quantityAvailable
//   - Collection has descriptionHtml, updatedAt, products; no plain description
//   - Root products(first, query, sortKey, reverse) and collections(...) exist,
//     and the products `query` grammar matches Shopify's
//     (e.g. "variants.price:<100 OR (title:'X' AND updated_at:>'...')")
//   - Connections expose `nodes` alongside `edges`
//   - CustomerAccessToken is {accessToken: String, expiresAt: Date}
//   - customerAccessTokenCreate needs unauthenticated_write_customer_information
//   - ProductConnection additionally exposes totalCount and filters, which
//     Shopify does not -- those fields are deliberately not selected
//   - Rate limiting is TIME-based (1 second of compute per app per IP per
//     second), not request-count based like Shopify's. See runner.adaptive.
//
// The Image object's field list is not reachable in the public docs, so it is
// discovered at runtime by the negotiator rather than guessed.
type Shopline struct {
	Handle  string
	Version string
	Token   string
	Domain  string
	client  *gql.Client
	fm      *fieldMap
}

// shoplineRenames are the confirmed schema divergences. Everything else uses
// the canonical (Shopify-shaped) name.
var shoplineRenames = map[string]string{
	"variants":          "variantsV2",
	"images":            "imagesV2",
	"quantityAvailable": "inventoryQuantity",
}

func NewShopline(handle, version, token, domain string, extra map[string]string, timeout time.Duration, identity bool) *Shopline {
	host := domain
	if host == "" {
		host = handle + ".myshopline.com"
	}
	s := &Shopline{Handle: handle, Version: version, Token: token, Domain: host,
		fm: newFieldMap(shoplineRenames)}
	s.client = gql.NewClient(
		fmt.Sprintf("%s/storefront/graph/%s/graphql.json", baseURL(host), version),
		mergeHeaders(map[string]string{"Authorization": "Bearer " + token}, extra),
		timeout, identity,
	)
	return s
}

func (s *Shopline) Name() string             { return "shopline" }
func (s *Shopline) Client() *gql.Client      { return s.client }
func (s *Shopline) Disable(canonical string) { s.fm.Disable(canonical) }
func (s *Shopline) DisabledFields() []string { return s.fm.Disabled() }

func (s *Shopline) Bind(sc scenario.Scenario, v Vars) (Bound, error) {
	return bind(sc, s.fm, sc.OpName(), varsFor(sc, v))
}

func (s *Shopline) Canonical(native string) string { return s.fm.canonical(native) }

func (s *Shopline) NativeNames(canonical map[string][]string) map[string][]string {
	return nativeNames(s.fm, canonical)
}

func (s *Shopline) TypeProbe() (string, []string) {
	return typeProbeQuery, []string{"Product", "ProductVariant", "Image", "Collection"}
}
