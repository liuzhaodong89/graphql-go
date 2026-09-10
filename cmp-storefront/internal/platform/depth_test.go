package platform

import (
	"testing"
	"time"

	"github.com/shopline/cmp-storefront/internal/scenario"
)

func testVars() Vars {
	return Vars{
		ProductHandle:    "test-product",
		CollectionHandle: "test-collection",
		SearchTerm:       "shirt",
		PageSize:         10,
		Email:            "a@example.com",
		Password:         "x",
	}
}

// The depth budget is a hard requirement of the comparison design, so it is
// asserted against the queries the adapters actually send rather than against
// the numbers declared on the scenarios.
func TestBoundQueriesRespectDepthBudget(t *testing.T) {
	plats := []Platform{
		NewShopify("shop", "2026-07", "tok", "", nil, time.Second, true),
		NewShopline("handle", "v20250301", "tok", "", nil, time.Second, true),
	}
	for _, p := range plats {
		for _, sc := range scenario.All() {
			b, err := p.Bind(sc, testVars())
			if err != nil {
				t.Fatalf("%s/%s: bind: %v", p.Name(), sc.ID, err)
			}
			got := scenario.SemanticDepth(b.Query)
			if got > 2 {
				t.Errorf("%s/%s: semantic depth %d exceeds budget 2\n%s", p.Name(), sc.ID, got, b.Query)
			}
			if got != sc.Depth {
				t.Errorf("%s/%s: semantic depth %d, scenario declares %d", p.Name(), sc.ID, got, sc.Depth)
			}
		}
	}
}

// Both platforms must select the same number of leaf fields; otherwise the
// size gate compares different amounts of information and the latency numbers
// describe two different requests.
func TestBothPlatformsSelectSameLeafCount(t *testing.T) {
	sf := NewShopify("shop", "2026-07", "tok", "", nil, time.Second, true)
	sl := NewShopline("handle", "v20250301", "tok", "", nil, time.Second, true)

	for _, sc := range scenario.All() {
		a, err := sf.Bind(sc, testVars())
		if err != nil {
			t.Fatal(err)
		}
		b, err := sl.Bind(sc, testVars())
		if err != nil {
			t.Fatal(err)
		}
		na, nb := countLeaves(a.Query), countLeaves(b.Query)
		if na != nb {
			t.Errorf("%s: shopify selects %d leaves, shopline %d\n--- shopify ---\n%s\n--- shopline ---\n%s",
				sc.ID, na, nb, a.Query, b.Query)
		}
		if na == 0 {
			t.Errorf("%s: leaf counter produced 0", sc.ID)
		}
	}
}

// The inventory variant of S3 must also stay balanced: Shopify calls the field
// quantityAvailable and SHOPLINE calls it inventoryQuantity.
func TestInventoryVariantStaysBalanced(t *testing.T) {
	sf := NewShopify("shop", "2026-07", "tok", "", nil, time.Second, true)
	sl := NewShopline("handle", "v20250301", "tok", "", nil, time.Second, true)
	sc, _ := scenario.ByID("S3_product_variants")
	a, _ := sf.Bind(sc, testVars())
	b, _ := sl.Bind(sc, testVars())
	if countLeaves(a.Query) != countLeaves(b.Query) {
		t.Errorf("inventory variant unbalanced: %d vs %d", countLeaves(a.Query), countLeaves(b.Query))
	}
}

// SHOPLINE's V2 connection fields must fold onto the canonical names, or
// byte attribution would report every variant path as a pure gap.
func TestFieldRewriteFoldsV2Names(t *testing.T) {
	sl := NewShopline("handle", "v20250301", "tok", "", nil, time.Second, true)
	for _, tc := range []struct{ scenarioID, in, want string }{
		{"S3_product_variants", "product.variantsV2.nodes[].sku", "product.variants.nodes[].sku"},
		{"S4_product_images", "product.imagesV2.nodes[].url", "product.images.nodes[].url"},
		{"S2_product_detail", "product.title", "product.title"},
	} {
		sc, _ := scenario.ByID(tc.scenarioID)
		b, _ := sl.Bind(sc, testVars())
		if got := b.RewritePath(tc.in); got != tc.want {
			t.Errorf("%s: RewritePath(%q) = %q, want %q", tc.scenarioID, tc.in, got, tc.want)
		}
	}
}

func isIdentByte(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// countLeaves counts selected fields that have no selection set of their own.
// A field immediately followed by "{" is a parent and is discounted.
func countLeaves(q string) int {
	n, brace, paren := 0, 0, 0
	var ident []rune
	lastCounted := false

	emit := func() {
		if len(ident) == 0 {
			return
		}
		if brace >= 1 && paren == 0 {
			n++
			lastCounted = true
		}
		ident = nil
	}

	for _, r := range q {
		switch {
		case r == '(':
			emit()
			paren++
		case r == ')':
			paren--
		case paren > 0:
			// Argument values never contain selected fields.
		case isIdentByte(r):
			ident = append(ident, r)
		case r == '{':
			emit()
			if lastCounted {
				n-- // the identifier before "{" is a parent, not a leaf
			}
			brace++
			lastCounted = false
		case r == '}':
			emit()
			brace--
			lastCounted = false
		default:
			emit()
		}
	}
	return n
}
