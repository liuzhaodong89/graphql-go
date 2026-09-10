package gql

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCanonicalizeIsOrderAndWhitespaceInvariant(t *testing.T) {
	// The size gate must not be swayed by how a server happens to order keys
	// or pad its JSON, or two identical payloads would score differently.
	a := json.RawMessage(`{"b":1,  "a":  [1,2,3], "c":{"y":"z","x":"w"}}`)
	b := json.RawMessage(`{"a":[1,2,3],"c":{"x":"w","y":"z"},"b":1}`)
	ca, err := Canonicalize(a)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := Canonicalize(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(ca) != string(cb) {
		t.Fatalf("canonical forms differ:\n%s\n%s", ca, cb)
	}
	if SizeDelta(len(ca), len(cb)) != 0 {
		t.Fatal("identical payloads produced a nonzero size delta")
	}
}

func TestCanonicalizePreservesNumberPrecision(t *testing.T) {
	// Prices and inventory counts must not be re-rendered through float64.
	in := json.RawMessage(`{"price":"19.99","qty":10000000000000001}`)
	out, err := Canonicalize(in)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"price":"19.99","qty":10000000000000001}`
	if string(out) != want {
		t.Fatalf("got %s, want %s", out, want)
	}
}

func TestFieldBytesCollapsesListIndices(t *testing.T) {
	raw := json.RawMessage(`{"product":{"variants":{"nodes":[{"sku":"A"},{"sku":"BB"}]}}}`)
	m, err := FieldBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m["product.variants.nodes[].sku"]; !ok {
		t.Fatalf("expected collapsed list path, got %v", m)
	}
	if m["product.variants.nodes[].sku"] == 0 {
		t.Fatal("collapsed path carries no bytes")
	}
}

func TestAttributeRanksTheDominantGapFirst(t *testing.T) {
	// This is the diagnostic that turns "delta is 18%" into "the URL field is
	// responsible", so the ordering is the whole point.
	a := map[string]int{
		"product.images.nodes[].url":     900,
		"product.images.nodes[].altText": 40,
		"product.id":                     30,
	}
	b := map[string]int{
		"product.images.nodes[].url":     300,
		"product.images.nodes[].altText": 38,
		"product.id":                     30,
	}
	rows := Attribute(a, b, nil)
	if len(rows) == 0 {
		t.Fatal("no attribution rows")
	}
	if rows[0].Path != "product.images.nodes[].url" {
		t.Fatalf("top gap is %q, want the url field", rows[0].Path)
	}
	if rows[0].Delta != 600 {
		t.Fatalf("delta = %d, want 600", rows[0].Delta)
	}
	if rows[0].ShareOfGap < 0.9 {
		t.Fatalf("share of gap = %.2f, want the url field to dominate", rows[0].ShareOfGap)
	}
}

func TestAttributeJoinsThroughAliasMap(t *testing.T) {
	a := map[string]int{"product.variants.nodes[].sku": 100}
	b := map[string]int{"product.variantsV2.nodes[].sku": 100}
	rows := Attribute(a, b, map[string]string{
		"product.variantsV2.nodes[].sku": "product.variants.nodes[].sku",
	})
	for _, r := range rows {
		if r.Delta != 0 {
			t.Fatalf("aliased paths should cancel, got %+v", r)
		}
	}
}

func TestSizeDeltaIsRelativeToTheLarger(t *testing.T) {
	if got := SizeDelta(100, 90); got < 0.099 || got > 0.101 {
		t.Fatalf("SizeDelta(100,90) = %v, want 0.10", got)
	}
	if got := SizeDelta(0, 0); got != 0 {
		t.Fatalf("SizeDelta(0,0) = %v, want 0", got)
	}
}

// The two storefronts do not agree on the GraphQL error envelope. SHOPLINE was
// observed returning {"errors":"Required head missing or invalid."} — a bare
// string where the spec says array-of-objects. Decoding strictly would misfile
// an auth failure as a malformed body, so every shape must survive.
func TestErrorEnvelopeShapesAllDecode(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantN   int
		wantMsg string
	}{
		{"spec_array", `{"errors":[{"message":"boom","extensions":{"code":"X"}}]}`, 1, "boom"},
		{"shopline_bare_string", `{"errors":"Required head missing or invalid."}`, 1, "Required head missing or invalid."},
		{"single_object", `{"errors":{"message":"solo"}}`, 1, "solo"},
		{"null", `{"errors":null,"data":{}}`, 0, ""},
		{"absent", `{"data":{}}`, 0, ""},
		{"empty_array", `{"errors":[],"data":{}}`, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Response{Body: []byte(tc.body), Status: 200}
			env, err := r.Parse()
			if err != nil {
				t.Fatalf("Parse failed on a real-world envelope: %v", err)
			}
			if len(env.Errors) != tc.wantN {
				t.Fatalf("got %d errors, want %d (%+v)", len(env.Errors), tc.wantN, env.Errors)
			}
			if tc.wantN > 0 && env.Errors[0].Message != tc.wantMsg {
				t.Fatalf("message = %q, want %q", env.Errors[0].Message, tc.wantMsg)
			}
		})
	}
}

// Extensions must still be reachable on the array form: throttle detection
// reads extensions.code.
func TestErrorExtensionsSurviveDecoding(t *testing.T) {
	r := Response{Body: []byte(`{"errors":[{"message":"t","extensions":{"code":"THROTTLED"}}]}`), Status: 200}
	env, err := r.Parse()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := env.Errors[0].Extensions["code"].(string); got != "THROTTLED" {
		t.Fatalf("extensions.code = %q, want THROTTLED", got)
	}
}

// The harness must identify itself rather than ship whatever the runtime's
// default agent happens to be: SHOPLINE denylists some defaults outright
// (Python-urllib receives HTTP 436), so an unset agent is a latent outage.
func TestRequestsCarryAnExplicitUserAgent(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, map[string]string{"X-Auth": "t"}, 3*time.Second, true)
	if r := c.Do(context.Background(), Request{Query: "{a}"}); r.TransportErr != nil {
		t.Fatal(r.TransportErr)
	}
	if got != UserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, UserAgent)
	}
	if strings.HasPrefix(got, "Go-http-client") {
		t.Fatal("shipped the runtime default agent")
	}
}
