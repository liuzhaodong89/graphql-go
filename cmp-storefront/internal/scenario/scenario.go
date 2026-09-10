// Package scenario defines the pre-checkout comparison set.
//
// Every scenario is a *semantic* contract expressed as a canonical selection
// tree. Both platforms render their query from the same tree, so the two
// requests cannot drift apart the way two hand-maintained query strings would.
// Platform-specific field names live in the adapters.
//
// Depth rule (semantic): entering the root field's selection set is level 1;
// each further nested business object adds one. Relay connection wrappers
// (edges/node, nodes) do not count -- they are transport plumbing, symmetric
// across both platforms, and counting them would make depth<=2 unsatisfiable
// for any list query. Mutation payloads need no exception: the payload's own
// fields sit at level 2 under natural counting.
package scenario

// Group separates read traffic from auth/write traffic. They differ in rate
// limits, in server-side cost profile, and in what a latency number means, so
// they are never scored together.
type Group string

const (
	GroupReadOnly Group = "readonly"
	GroupAuth     Group = "auth"
)

// Kind tells the runner how to supply variables for each sample.
type Kind string

const (
	KindByProductHandle    Kind = "product_handle"
	KindByCollectionHandle Kind = "collection_handle"
	KindSearch             Kind = "search"
	KindStatic             Kind = "static"
	KindLogin              Kind = "login"
)

type Scenario struct {
	ID    string
	Title string
	Group Group
	Kind  Kind
	Op    string // "query" or "mutation"
	Root  *Sel
	// Depth is the semantic depth asserted by the design; the preflight
	// re-derives it from the rendered query and fails on mismatch.
	Depth int
	// RequiredPaths are canonical dotted paths into `data` that must be present
	// and non-null for a sample to count as successful. Without these, a
	// response of {"data":{"product":null}} passes as a 200 success and quietly
	// makes the not-found path look fast.
	RequiredPaths []string
	// EchoCheck asserts a returned value equals the request variable, catching
	// caches or routers that serve the wrong entity.
	EchoCheck [2]string // {canonical data path, variable name}
	// Optional lists canonical fields that may be negotiated away when a
	// platform does not implement them. Everything else is mandatory: losing it
	// invalidates the scenario rather than shrinking it.
	Optional []string
	Notes    string
}

var (
	argHandle = Arg{Name: "handle", Var: "handle", VarType: "String!"}
	argFirst  = Arg{Name: "first", Var: "n", VarType: "Int!"}
	argQuery  = Arg{Name: "query", Var: "q", VarType: "String!"}
	argInput  = Arg{Name: "input", Var: "input", VarType: "CustomerAccessTokenCreateInput!"}
)

// All returns the scenario set in execution order.
func All() []Scenario {
	return []Scenario{
		{
			// The only scenario that needs nothing but a store token: no
			// catalogue data, no sales-channel binding. It is therefore the
			// one comparison available while SHOPLINE's channelHandle
			// requirement blocks every product and collection query.
			//
			// What it measures is connection + dispatch overhead, NOT query
			// engine work: the payload is tens of bytes. Read it as "how far
			// is each platform's edge", not "which query engine is faster".
			ID: "S0_shop_info", Title: "Shop info (store-token only)",
			Group: GroupReadOnly, Kind: KindStatic, Op: "query", Depth: 1,
			Root: &Sel{
				Field:   "shop",
				Scalars: []string{"id", "name", "description", "moneyFormat", "customerAccountUrl"},
			},
			RequiredPaths: []string{"shop.name"},
			// Everything but the name is negotiable: the two Shop types are
			// not field-for-field identical, and the negotiator settles the
			// intersection against the live schemas.
			Optional: []string{"id", "description", "moneyFormat", "customerAccountUrl"},
			Notes:    "Measures connection and dispatch overhead, not query cost. Do not read it as a query-engine comparison.",
		},
		{
			ID: "S2_product_detail", Title: "Product detail (scalars)",
			Group: GroupReadOnly, Kind: KindByProductHandle, Op: "query", Depth: 1,
			Root: &Sel{
				Field: "product", Args: []Arg{argHandle},
				Scalars: []string{"id", "title", "handle", "descriptionHtml",
					"productType", "vendor", "availableForSale", "createdAt", "updatedAt"},
			},
			RequiredPaths: []string{"product.id", "product.handle", "product.title"},
			EchoCheck:     [2]string{"product.handle", "handle"},
			Optional:      []string{"createdAt", "updatedAt"},
			Notes:         "descriptionHtml, not description: SHOPLINE has no plain-text description field.",
		},
		{
			ID: "S3_product_variants", Title: "Product variants (availability)",
			Group: GroupReadOnly, Kind: KindByProductHandle, Op: "query", Depth: 2,
			Root: &Sel{
				Field: "product", Args: []Arg{argHandle}, Scalars: []string{"id"},
				Children: []*Sel{{
					Field: "variants", Args: []Arg{argFirst}, Conn: true,
					Scalars: []string{"id", "title", "sku", "availableForSale", "quantityAvailable"},
				}},
			},
			RequiredPaths: []string{"product.id", "product.variants.nodes"},
			Optional:      []string{"quantityAvailable", "sku"},
			Notes:         "Price is excluded: Shopify returns MoneyV2{amount,currencyCode} while SHOPLINE returns a Money scalar, which breaks both the depth and the size symmetry.",
		},
		{
			ID: "S4_product_images", Title: "Product media",
			Group: GroupReadOnly, Kind: KindByProductHandle, Op: "query", Depth: 2,
			Root: &Sel{
				Field: "product", Args: []Arg{argHandle}, Scalars: []string{"id"},
				Children: []*Sel{{
					Field: "images", Args: []Arg{argFirst}, Conn: true,
					Scalars: []string{"id", "url", "width", "height", "altText"},
				}},
			},
			RequiredPaths: []string{"product.id", "product.images.nodes"},
			Optional:      []string{"id", "width", "height", "altText", "url"},
			Notes:         "SHOPLINE's Image object is not documented; the negotiator discovers its field set from the live schema. CDN hostname length usually dominates this scenario's size gap.",
		},
		{
			ID: "S5_collections", Title: "Collection list",
			Group: GroupReadOnly, Kind: KindStatic, Op: "query", Depth: 1,
			Root: &Sel{
				Field: "collections", Args: []Arg{argFirst}, Conn: true,
				Scalars: []string{"id", "title", "handle", "updatedAt"},
			},
			RequiredPaths: []string{"collections.nodes"},
			Optional:      []string{"updatedAt"},
		},
		{
			ID: "S6_collection_products", Title: "Collection detail + products",
			Group: GroupReadOnly, Kind: KindByCollectionHandle, Op: "query", Depth: 2,
			Root: &Sel{
				Field: "collection", Args: []Arg{argHandle},
				Scalars: []string{"id", "title", "handle"},
				Children: []*Sel{{
					Field: "products", Args: []Arg{argFirst}, Conn: true,
					Scalars: []string{"id", "title", "handle"},
				}},
			},
			RequiredPaths: []string{"collection.id", "collection.products.nodes"},
			EchoCheck:     [2]string{"collection.handle", "handle"},
		},
		{
			ID: "S7_product_search", Title: "Product search (structured filter)",
			Group: GroupReadOnly, Kind: KindSearch, Op: "query", Depth: 1,
			Root: &Sel{
				Field: "products", Args: []Arg{argFirst, argQuery}, Conn: true,
				Scalars: []string{"id", "title", "handle"},
			},
			RequiredPaths: []string{"products.nodes"},
			Notes:         "Uses a structured filter expression (title:'...'), not a free-text term: both platforms document the same filter grammar, while full-text relevance search is implemented differently and would not be comparable.",
		},
		{
			ID: "S1_customer_token", Title: "Customer access token",
			Group: GroupAuth, Kind: KindLogin, Op: "mutation", Depth: 2,
			Root: &Sel{
				Field: "customerAccessTokenCreate", Args: []Arg{argInput},
				Children: []*Sel{
					{Field: "customerAccessToken", Scalars: []string{"accessToken", "expiresAt"}},
					{Field: "customerUserErrors", Scalars: []string{"code", "field", "message"}},
				},
			},
			RequiredPaths: []string{"customerAccessTokenCreate.customerAccessToken.accessToken"},
			Notes:         "Shopify flags this mutation 'legacy customer accounts only'; the negotiator probes whether the store still accepts it. Reported separately, never in the aggregate score.",
		},
	}
}

func ByID(id string) (Scenario, bool) {
	for _, s := range All() {
		if s.ID == id {
			return s, true
		}
	}
	return Scenario{}, false
}

// IsOptional reports whether a canonical field may be negotiated away.
func (s Scenario) IsOptional(field string) bool {
	for _, o := range s.Optional {
		if o == field {
			return true
		}
	}
	return false
}

// OpName derives the GraphQL operation name from the scenario ID. It is
// derived rather than hand-written so both platforms send the same operation
// name -- it is part of the request bytes the size gate measures.
func (s Scenario) OpName() string {
	var b []rune
	upper := true
	for _, r := range s.ID {
		if r == '_' {
			upper = true
			continue
		}
		if upper {
			if r >= 'a' && r <= 'z' {
				r = r - 'a' + 'A'
			}
			upper = false
		}
		b = append(b, r)
	}
	return string(b)
}
