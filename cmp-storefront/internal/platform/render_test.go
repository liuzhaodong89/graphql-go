package platform

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/shopline/cmp-storefront/internal/scenario"
)

func adapters(t *testing.T) (*Shopify, *Shopline) {
	t.Helper()
	return NewShopify("shop", "2026-07", "tok", "", nil, time.Second, true),
		NewShopline("h", "v20250301", "tok", "", nil, time.Second, true)
}

// knownRenames are the only textual differences allowed between the two
// rendered queries. Anything else means the two platforms are being asked
// different questions.
var knownRenames = [][2]string{
	{"variantsV2", "variants"},
	{"imagesV2", "images"},
	{"inventoryQuantity", "quantityAvailable"},
}

func TestRenderedQueriesDifferOnlyByKnownRenames(t *testing.T) {
	sf, sl := adapters(t)
	for _, sc := range scenario.All() {
		a, err := sf.Bind(sc, testVars())
		if err != nil {
			t.Fatalf("%s: %v", sc.ID, err)
		}
		b, err := sl.Bind(sc, testVars())
		if err != nil {
			t.Fatalf("%s: %v", sc.ID, err)
		}
		folded := b.Query
		for _, r := range knownRenames {
			folded = strings.ReplaceAll(folded, r[0], r[1])
		}
		if folded != a.Query {
			t.Errorf("%s: queries differ beyond the known renames\n--- shopify ---\n%s\n--- shopline (folded) ---\n%s",
				sc.ID, a.Query, folded)
		}
	}
}

// The request-side size budget. The rendered pair differs only by the "V2"
// suffixes, so the gap must stay far inside the tolerance.
func TestRequestBytesStayWithinTolerance(t *testing.T) {
	const tolerance = 256
	sf, sl := adapters(t)
	for _, sc := range scenario.All() {
		a, _ := sf.Bind(sc, testVars())
		b, _ := sl.Bind(sc, testVars())
		ba, err := json.Marshal(map[string]any{
			"query": a.Query, "variables": a.Variables, "operationName": a.Operation})
		if err != nil {
			t.Fatal(err)
		}
		bb, err := json.Marshal(map[string]any{
			"query": b.Query, "variables": b.Variables, "operationName": b.Operation})
		if err != nil {
			t.Fatal(err)
		}
		gap := len(ba) - len(bb)
		if gap < 0 {
			gap = -gap
		}
		if gap > tolerance {
			t.Errorf("%s: request bytes %d vs %d (gap %d > %d)", sc.ID, len(ba), len(bb), gap, tolerance)
		}
		if a.Operation != b.Operation {
			t.Errorf("%s: operation names differ (%q vs %q); they are part of the request bytes",
				sc.ID, a.Operation, b.Operation)
		}
	}
}

// Both platforms render from one canonical tree, so the selected leaf set must
// be identical by construction -- including after negotiation removes a field.
func TestLeafSetsMatchBeforeAndAfterNegotiation(t *testing.T) {
	sf, sl := adapters(t)
	check := func(label string) {
		for _, sc := range scenario.All() {
			la := scenario.Leaves(sc.Root, sf.fm.mapper())
			lb := scenario.Leaves(sc.Root, sl.fm.mapper())
			if len(la) != len(lb) {
				t.Fatalf("%s %s: leaf counts %d vs %d", label, sc.ID, len(la), len(lb))
			}
			for i := range la {
				if la[i] != lb[i] {
					t.Fatalf("%s %s: leaf %d is %q vs %q", label, sc.ID, i, la[i], lb[i])
				}
			}
		}
	}
	check("before")

	// Negotiation always disables on every platform; simulate that.
	for _, f := range []string{"quantityAvailable", "altText", "updatedAt"} {
		sf.Disable(f)
		sl.Disable(f)
	}
	check("after")

	a, _ := sf.Bind(mustScenario(t, "S3_product_variants"), testVars())
	if strings.Contains(a.Query, "quantityAvailable") {
		t.Error("disabled field still rendered into the query")
	}
	b, _ := sl.Bind(mustScenario(t, "S3_product_variants"), testVars())
	if strings.Contains(b.Query, "inventoryQuantity") {
		t.Error("disabled field still rendered on the renaming platform")
	}
}

// Removing a mandatory field must fail loudly rather than silently shrinking
// the scenario into something that is no longer what was designed.
func TestDisablingAMandatoryRootFieldFailsBind(t *testing.T) {
	sf, _ := adapters(t)
	sf.Disable("product")
	if _, err := sf.Bind(mustScenario(t, "S2_product_detail"), testVars()); err == nil {
		t.Fatal("expected bind to fail when the root field is unavailable")
	}
}

func TestPathAliasCoversRenamedSubtree(t *testing.T) {
	_, sl := adapters(t)
	b, _ := sl.Bind(mustScenario(t, "S3_product_variants"), testVars())
	for canonical, want := range map[string]string{
		"product.variants":                         "product.variantsV2",
		"product.variants.nodes":                   "product.variantsV2.nodes",
		"product.variants.nodes.quantityAvailable": "product.variantsV2.nodes.inventoryQuantity",
	} {
		if got := b.Resolve(canonical); got != want {
			t.Errorf("Resolve(%q) = %q, want %q", canonical, got, want)
		}
	}
	// The reverse direction folds attribution paths back onto canonical names.
	if got := b.RewritePath("product.variantsV2.nodes.inventoryQuantity"); got != "product.variants.nodes.quantityAvailable" {
		t.Errorf("RewritePath gave %q", got)
	}
}

// The structured search filter must be identical on both sides; both platforms
// document the same grammar for the products `query` argument.
func TestSearchFilterIsStructuredAndShared(t *testing.T) {
	sf, sl := adapters(t)
	sc := mustScenario(t, "S7_product_search")
	a, _ := sf.Bind(sc, testVars())
	b, _ := sl.Bind(sc, testVars())
	qa, _ := a.Variables["q"].(string)
	qb, _ := b.Variables["q"].(string)
	if qa != qb {
		t.Fatalf("search filters differ: %q vs %q", qa, qb)
	}
	if !strings.HasPrefix(qa, "title:") {
		t.Fatalf("expected a structured filter, got %q", qa)
	}
}

func mustScenario(t *testing.T, id string) scenario.Scenario {
	t.Helper()
	sc, ok := scenario.ByID(id)
	if !ok {
		t.Fatalf("scenario %s not found", id)
	}
	return sc
}

// Extra headers exist so an undocumented platform requirement can be satisfied
// from config. They must never be able to shadow the auth header: doing so
// would silently turn every request anonymous and report the 401s as latency.
func TestExtraHeadersCannotShadowAuth(t *testing.T) {
	sf := NewShopify("s", "2026-07", "real-token", "", map[string]string{
		"X-Shopify-Storefront-Access-Token": "hijacked",
		"X-Channel-Handle":                  "main",
	}, time.Second, true)
	h := sf.Client().Headers
	if h["X-Shopify-Storefront-Access-Token"] != "real-token" {
		t.Errorf("auth header was overridden by an extra: %q", h["X-Shopify-Storefront-Access-Token"])
	}
	if h["X-Channel-Handle"] != "main" {
		t.Errorf("extra header not applied: %q", h["X-Channel-Handle"])
	}

	sl := NewShopline("h", "v20250301", "real-token", "", map[string]string{
		"Authorization":    "Bearer hijacked",
		"X-Channel-Handle": "main",
	}, time.Second, true)
	hs := sl.Client().Headers
	if hs["Authorization"] != "Bearer real-token" {
		t.Errorf("auth header was overridden by an extra: %q", hs["Authorization"])
	}
	if hs["X-Channel-Handle"] != "main" {
		t.Errorf("extra header not applied: %q", hs["X-Channel-Handle"])
	}
}

func TestNilExtraHeadersIsSafe(t *testing.T) {
	sl := NewShopline("h", "v20250301", "tok", "", nil, time.Second, true)
	if sl.Client().Headers["Authorization"] != "Bearer tok" {
		t.Fatal("auth header lost when extras are nil")
	}
}
