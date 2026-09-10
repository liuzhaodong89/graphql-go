package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopline/cmp-storefront/internal/platform"
)

func handleServer(products, collections []string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mk := func(hs []string) any {
			nodes := make([]any, 0, len(hs))
			for _, h := range hs {
				nodes = append(nodes, map[string]any{"handle": h})
			}
			return map[string]any{"nodes": nodes}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"products": mk(products), "collections": mk(collections)}})
	}))
}

// Only handles present on BOTH stores may be used: a handle missing on one
// side fails every pair it appears in, and those failures cluster wherever the
// catalogues diverge rather than falling randomly.
func TestDiscoverFixturesKeepsOnlySharedHandles(t *testing.T) {
	a := handleServer([]string{"shared-1", "shared-2", "shopify-only"}, []string{"col-a", "col-shopify"})
	defer a.Close()
	b := handleServer([]string{"shared-2", "shared-1", "shopline-only"}, []string{"col-a"})
	defer b.Close()

	ps := []platform.Platform{
		platform.NewShopify("s", "2026-07", "tok", a.URL, nil, 3*time.Second, true),
		platform.NewShopline("h", "v20250301", "tok", b.URL, nil, 3*time.Second, true),
	}
	fx, notes, err := DiscoverFixtures(context.Background(), ps, 50)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fx.ProductHandles, ","); got != "shared-1,shared-2" {
		t.Fatalf("product handles = %q, want the sorted intersection", got)
	}
	if got := strings.Join(fx.CollectionHandles, ","); got != "col-a" {
		t.Fatalf("collection handles = %q", got)
	}
	// A small shared pool means samples hit the same warm cache entries, so it
	// must be surfaced rather than silently accepted.
	if !strings.Contains(strings.Join(notes, "\n"), "WARNING") {
		t.Error("a 2-handle pool produced no warning about cache concentration")
	}
}

func TestDiscoverFixturesSingleStoreUsesEverything(t *testing.T) {
	a := handleServer([]string{"p1", "p2"}, []string{"c1"})
	defer a.Close()
	ps := []platform.Platform{platform.NewShopify("s", "2026-07", "tok", a.URL, nil, 3*time.Second, true)}
	fx, notes, err := DiscoverFixtures(context.Background(), ps, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(fx.ProductHandles) != 2 {
		t.Fatalf("got %v", fx.ProductHandles)
	}
	// The shared-pool warning is meaningless with one store.
	if strings.Contains(strings.Join(notes, "\n"), "WARNING") {
		t.Error("single-store discovery should not warn about a shared pool")
	}
}

func TestDiscoverFixturesReportsGraphQLErrorRedacted(t *testing.T) {
	const tok = "shpat_secret_value_here"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		msg, _ := json.Marshal("denied for " + r.Header.Get("X-Shopify-Storefront-Access-Token"))
		_, _ = w.Write([]byte(`{"errors":[{"message":` + string(msg) + `}]}`))
	}))
	defer srv.Close()

	ps := []platform.Platform{platform.NewShopify("s", "2026-07", tok, srv.URL, nil, 3*time.Second, true)}
	_, _, err := DiscoverFixtures(context.Background(), ps, 10)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), tok) {
		t.Fatalf("fixture discovery leaked the token: %v", err)
	}
}
