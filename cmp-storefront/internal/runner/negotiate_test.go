package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopline/cmp-storefront/internal/platform"
	"github.com/shopline/cmp-storefront/internal/scenario"
)

// stub answers GraphQL requests, rejecting any field named in `reject` the way
// a real server would (HTTP 200 with an errors array) and returning null for
// any field named in `null`.
type stub struct {
	reject map[string]string // field -> error message template
	null   map[string]bool
	calls  int
}

func (s *stub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)
		s.calls++
		w.Header().Set("Content-Type", "application/json")

		for f, msg := range s.reject {
			if fieldSelected(req.Query, f) {
				_, _ = w.Write([]byte(`{"errors":[{"message":` + jsonString(msg) + `}],"data":null}`))
				return
			}
		}
		_, _ = w.Write([]byte(s.respond(req.Query, req.Variables)))
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func fieldSelected(query, field string) bool {
	for _, line := range strings.Split(query, "\n") {
		if strings.TrimSpace(line) == field {
			return true
		}
	}
	return false
}

// respond builds a product payload containing exactly the variant fields the
// query selected.
func (s *stub) respond(query string, vars map[string]any) string {
	handle, _ := vars["handle"].(string)
	node := map[string]any{}
	for _, f := range []string{"id", "title", "sku", "availableForSale", "quantityAvailable", "inventoryQuantity"} {
		if !fieldSelected(query, f) {
			continue
		}
		switch {
		case s.null[f]:
			node[f] = nil
		case f == "availableForSale":
			node[f] = true
		case f == "quantityAvailable" || f == "inventoryQuantity":
			node[f] = 7
		default:
			node[f] = "v-" + f
		}
	}
	conn := "variants"
	if strings.Contains(query, "variantsV2") {
		conn = "variantsV2"
	}
	payload := map[string]any{"data": map[string]any{
		"product": map[string]any{
			"id":     "gid://1",
			"handle": handle,
			"title":  "Widget",
			conn:     map[string]any{"nodes": []any{node}},
		},
	}}
	b, _ := json.Marshal(payload)
	return string(b)
}

func stubPlatforms(t *testing.T, a, b *stub) (platform.Platform, platform.Platform, func()) {
	t.Helper()
	sa := httptest.NewServer(a.handler())
	sb := httptest.NewServer(b.handler())
	pa := platform.NewShopify("s", "2026-07", "tok", sa.URL, nil, 3*time.Second, true)
	pb := platform.NewShopline("h", "v20250301", "tok", sb.URL, nil, 3*time.Second, true)
	return pa, pb, func() { sa.Close(); sb.Close() }
}

func find(negs []Negotiation, id string) Negotiation {
	for _, n := range negs {
		if n.Scenario == id {
			return n
		}
	}
	return Negotiation{}
}

// A field one platform does not implement must be dropped from BOTH, so the
// two sides keep requesting the same information.
func TestNegotiationDropsUnsupportedFieldOnBothPlatforms(t *testing.T) {
	a := &stub{}
	b := &stub{reject: map[string]string{
		"inventoryQuantity": `Cannot query field "inventoryQuantity" on type "ProductVariant"`,
	}}
	pa, pb, done := stubPlatforms(t, a, b)
	defer done()

	sc, _ := scenario.ByID("S3_product_variants")
	negs := Negotiate(context.Background(), []platform.Platform{pa, pb},
		[]scenario.Scenario{sc}, platform.Vars{ProductHandle: "widget", PageSize: 10})

	n := find(negs, sc.ID)
	if !n.Runnable {
		t.Fatalf("scenario blocked unexpectedly: %s", n.Blocked)
	}
	if len(n.Removed) != 1 || n.Removed[0] != "quantityAvailable" {
		t.Fatalf("removed = %v, want [quantityAvailable]", n.Removed)
	}
	// Critically: dropped on the platform that DID support it, too.
	qa, _ := pa.Bind(sc, platform.Vars{ProductHandle: "w", PageSize: 10})
	if strings.Contains(qa.Query, "quantityAvailable") {
		t.Error("field was rejected by shopline but is still requested from shopify")
	}
	qb, _ := pb.Bind(sc, platform.Vars{ProductHandle: "w", PageSize: 10})
	if strings.Contains(qb.Query, "inventoryQuantity") {
		t.Error("rejected field still requested from shopline")
	}
}

// A field that exists but resolves to null on only one platform is an
// unrequested asymmetry: it lands straight in the size delta. It must be
// dropped on both sides, which is what happens when a Shopify token lacks the
// inventory scope.
func TestNegotiationDropsOneSidedNullField(t *testing.T) {
	a := &stub{null: map[string]bool{"quantityAvailable": true}}
	b := &stub{}
	pa, pb, done := stubPlatforms(t, a, b)
	defer done()

	sc, _ := scenario.ByID("S3_product_variants")
	negs := Negotiate(context.Background(), []platform.Platform{pa, pb},
		[]scenario.Scenario{sc}, platform.Vars{ProductHandle: "widget", PageSize: 10})

	n := find(negs, sc.ID)
	if !n.Runnable {
		t.Fatalf("blocked: %s", n.Blocked)
	}
	if len(n.Removed) != 1 || n.Removed[0] != "quantityAvailable" {
		t.Fatalf("removed = %v, want [quantityAvailable]", n.Removed)
	}
	if !strings.Contains(n.Reasons["quantityAvailable"], "null on one platform") {
		t.Errorf("reason does not explain the asymmetry: %q", n.Reasons["quantityAvailable"])
	}
}

// Losing a MANDATORY field must block the scenario, not quietly shrink it into
// a different comparison.
func TestNegotiationBlocksOnMandatoryFieldLoss(t *testing.T) {
	a := &stub{}
	b := &stub{reject: map[string]string{
		"title": `Cannot query field "title" on type "ProductVariant"`,
	}}
	pa, pb, done := stubPlatforms(t, a, b)
	defer done()

	sc, _ := scenario.ByID("S3_product_variants")
	negs := Negotiate(context.Background(), []platform.Platform{pa, pb},
		[]scenario.Scenario{sc}, platform.Vars{ProductHandle: "widget", PageSize: 10})

	n := find(negs, sc.ID)
	if n.Runnable {
		t.Fatal("scenario should be blocked when a mandatory field is unavailable")
	}
	if !strings.Contains(n.Blocked, "title") {
		t.Errorf("blocked reason does not name the field: %q", n.Blocked)
	}
}

// Negotiation must terminate even when a platform rejects several fields.
func TestNegotiationTerminatesWithMultipleRejections(t *testing.T) {
	a := &stub{}
	b := &stub{reject: map[string]string{
		"inventoryQuantity": `Cannot query field "inventoryQuantity" on type "ProductVariant"`,
		"sku":               `Cannot query field "sku" on type "ProductVariant"`,
	}}
	pa, pb, done := stubPlatforms(t, a, b)
	defer done()

	sc, _ := scenario.ByID("S3_product_variants")
	negs := Negotiate(context.Background(), []platform.Platform{pa, pb},
		[]scenario.Scenario{sc}, platform.Vars{ProductHandle: "widget", PageSize: 10})

	n := find(negs, sc.ID)
	if !n.Runnable {
		t.Fatalf("blocked: %s", n.Blocked)
	}
	if len(n.Removed) != 2 {
		t.Fatalf("removed = %v, want both optional fields", n.Removed)
	}
}
