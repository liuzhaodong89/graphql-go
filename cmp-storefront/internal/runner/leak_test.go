package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shopline/cmp-storefront/internal/platform"
	"github.com/shopline/cmp-storefront/internal/scenario"
)

const secretToken = "shpat_THIS_MUST_NEVER_BE_WRITTEN_ANYWHERE"

// Reports get pasted into tickets, committed, and shared. Nothing the tool
// writes may carry the access token, including when the server echoes the
// request back in an error message.
func TestTokenNeverReachesAnyOutput(t *testing.T) {
	echoErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Worst realistic case: an upstream that reflects the request headers
		// into its error body.
		w.Header().Set("Content-Type", "application/json")
		msg, _ := json.Marshal("bad request from token " + r.Header.Get("X-Shopify-Storefront-Access-Token"))
		_, _ = w.Write([]byte(`{"errors":[{"message":` + string(msg) + `}],"data":null}`))
	}))
	defer echoErr.Close()

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"product":{"id":"gid://1","handle":"widget","title":"W"}}}`))
	}))
	defer ok.Close()

	for _, srv := range []*httptest.Server{ok, echoErr} {
		p := platform.NewShopify("s", "2026-07", secretToken, srv.URL, nil, 3*time.Second, true)
		cfg := testCfg()
		cfg.Run.Scenarios = []string{"S2_product_detail"}

		pairs, err := New(p, nil, cfg).Run(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		rep := Aggregate(pairs, cfg, p.Name(), "")

		dir := t.TempDir()
		out := filepath.Join(dir, "report.json")
		if err := rep.WriteJSON(out); err != nil {
			t.Fatal(err)
		}
		onDisk, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}

		for name, blob := range map[string]string{
			"report.json": string(onDisk),
			"markdown":    rep.Markdown(),
			"pairs":       mustJSON(t, pairs),
		} {
			if strings.Contains(blob, secretToken) {
				t.Errorf("%s leaked the access token", name)
			}
		}
	}
}

// The endpoint is printed by `probe`, so it must not carry credentials either.
func TestEndpointsCarryNoCredentials(t *testing.T) {
	for _, p := range []platform.Platform{
		platform.NewShopify("s", "2026-07", secretToken, "", nil, time.Second, true),
		platform.NewShopline("h", "v20250301", secretToken, "", nil, time.Second, true),
	} {
		if strings.Contains(p.Client().Endpoint, secretToken) {
			t.Errorf("%s puts the token in the URL", p.Name())
		}
	}
}

// Login fixtures are configuration too: a password must not survive into a
// sample, a discard reason, or the report.
func TestLoginPasswordNeverReachesOutput(t *testing.T) {
	const pw = "correct-horse-battery-staple"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errors":[{"message":"login failed"}],"data":null}`))
	}))
	defer srv.Close()

	p := platform.NewShopify("s", "2026-07", secretToken, srv.URL, nil, 3*time.Second, true)
	sc, _ := scenario.ByID("S1_customer_token")
	s := Exec(context.Background(), p, sc, platform.Vars{
		Email: "qa@example.com", Password: pw}, "qa@example.com", true)

	blob := mustJSON(t, s)
	if strings.Contains(blob, pw) {
		t.Error("sample serialisation leaked the login password")
	}
	if strings.Contains(s.FailMsg, pw) {
		t.Error("failure message leaked the login password")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
