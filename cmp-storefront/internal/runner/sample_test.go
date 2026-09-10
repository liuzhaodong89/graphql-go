package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shopline/cmp-storefront/internal/gql"
	"github.com/shopline/cmp-storefront/internal/platform"
	"github.com/shopline/cmp-storefront/internal/scenario"
)

// fakePlatform points the real client at a test server so the status gate can
// be exercised against the exact response shapes both storefronts produce.
type fakePlatform struct {
	name     string
	c        *gql.Client
	alias    map[string]string
	disabled []string
}

func (f *fakePlatform) Name() string                                          { return f.name }
func (f *fakePlatform) Client() *gql.Client                                   { return f.c }
func (f *fakePlatform) TypeProbe() (string, []string)                         { return "", nil }
func (f *fakePlatform) Disable(canonical string)                              { f.disabled = append(f.disabled, canonical) }
func (f *fakePlatform) DisabledFields() []string                              { return f.disabled }
func (f *fakePlatform) NativeNames(c map[string][]string) map[string][]string { return c }
func (f *fakePlatform) Canonical(native string) string                        { return native }
func (f *fakePlatform) Bind(sc scenario.Scenario, v platform.Vars) (platform.Bound, error) {
	return platform.Bound{
		Query:     "query Q($handle: String!) { product(handle: $handle) { id handle title } }",
		Operation: "Q",
		Variables: map[string]any{"handle": v.ProductHandle},
		PathAlias: f.alias,
	}, nil
}

func newFake(t *testing.T, status int, body string) (*fakePlatform, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	c := gql.NewClient(srv.URL, nil, 3*time.Second, true)
	return &fakePlatform{name: "fake", c: c}, srv.Close
}

func TestStatusGate(t *testing.T) {
	sc, ok := scenario.ByID("S2_product_detail")
	if !ok {
		t.Fatal("scenario missing")
	}
	vars := platform.Vars{ProductHandle: "widget", PageSize: 10}

	cases := []struct {
		name   string
		status int
		body   string
		wantOK bool
		want   FailKind
	}{
		{
			name: "success", status: 200,
			body:   `{"data":{"product":{"id":"gid://1","handle":"widget","title":"Widget"}}}`,
			wantOK: true,
		},
		{
			// The critical case: HTTP 200 carrying a GraphQL errors array. A
			// harness that only checks the status code times an error path on
			// one platform against a real response on the other.
			name: "http_200_with_graphql_errors", status: 200,
			body: `{"errors":[{"message":"Field 'foo' doesn't exist"}],"data":null}`,
			want: FailGraphQL,
		},
		{
			name: "throttled_via_extension_code", status: 200,
			body: `{"errors":[{"message":"Too many requests","extensions":{"code":"THROTTLED"}}]}`,
			want: FailThrottled,
		},
		{
			name: "http_429", status: 429, body: `{}`,
			want: FailThrottled,
		},
		{
			name: "null_data", status: 200, body: `{"data":null}`,
			want: FailNullData,
		},
		{
			// A missing product returns 200 with data.product = null. Counting
			// it as success measures the not-found path, which is fast and
			// meaningless.
			name: "null_product_is_not_success", status: 200,
			body: `{"data":{"product":null}}`,
			want: FailMissing,
		},
		{
			name: "missing_required_field", status: 200,
			body: `{"data":{"product":{"id":"gid://1"}}}`,
			want: FailMissing,
		},
		{
			name: "echo_mismatch_catches_wrong_entity", status: 200,
			body: `{"data":{"product":{"id":"gid://1","handle":"other-widget","title":"Other"}}}`,
			want: FailEcho,
		},
		{
			name: "non_json_body", status: 200, body: `<html>503 from the edge</html>`,
			want: FailNotJSON,
		},
		{
			name: "http_500", status: 500, body: `{}`,
			want: FailHTTP,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, closeSrv := newFake(t, tc.status, tc.body)
			defer closeSrv()

			s := Exec(context.Background(), p, sc, vars, "widget", true)
			if s.OK != tc.wantOK {
				t.Fatalf("OK = %v (%s: %s), want %v", s.OK, s.FailKind, s.FailMsg, tc.wantOK)
			}
			if !tc.wantOK && s.FailKind != tc.want {
				t.Fatalf("FailKind = %q (%s), want %q", s.FailKind, s.FailMsg, tc.want)
			}
			if tc.wantOK {
				if s.DataBytes == 0 {
					t.Error("successful sample recorded zero data bytes")
				}
				if s.Timing.Total == 0 {
					t.Error("successful sample recorded zero total time")
				}
				if len(s.Fields) == 0 {
					t.Error("field attribution was requested but not collected")
				}
			}
		})
	}
}

// A required path resolved through an alias must follow the platform's real
// response shape, which is how SHOPLINE's variantsV2 is checked.
func TestRequiredPathResolvesThroughAlias(t *testing.T) {
	sc := scenario.Scenario{
		ID: "aliased", Kind: scenario.KindByProductHandle,
		RequiredPaths: []string{"product.variants.nodes"},
	}
	p, closeSrv := newFake(t, 200, `{"data":{"product":{"variantsV2":{"nodes":[{"id":"1"}]}}}}`)
	defer closeSrv()
	p.alias = map[string]string{"product.variants.nodes": "product.variantsV2.nodes"}

	s := Exec(context.Background(), p, sc, platform.Vars{ProductHandle: "w"}, "w", false)
	if !s.OK {
		t.Fatalf("alias not applied: %s %s", s.FailKind, s.FailMsg)
	}
}

// An empty list satisfies "present and non-null" but carries no data, so it
// must not count as a successful sample.
func TestEmptyListIsRejected(t *testing.T) {
	sc := scenario.Scenario{
		ID: "empty", Kind: scenario.KindStatic,
		RequiredPaths: []string{"collections.nodes"},
	}
	p, closeSrv := newFake(t, 200, `{"data":{"collections":{"nodes":[]}}}`)
	defer closeSrv()

	s := Exec(context.Background(), p, sc, platform.Vars{}, "static", false)
	if s.OK {
		t.Fatal("empty result list was accepted as a valid sample")
	}
	if s.FailKind != FailMissing {
		t.Fatalf("FailKind = %q, want %q", s.FailKind, FailMissing)
	}
}

// Both platforms must be recognised as throttled. A missed throttle code is
// filed as a generic GraphQL error, so the adaptive throttler never backs off
// and the run keeps hammering a limit it cannot see.
func TestThrottleDetectionCoversBothPlatforms(t *testing.T) {
	sc, _ := scenario.ByID("S2_product_detail")
	vars := platform.Vars{ProductHandle: "widget", PageSize: 10}

	cases := []struct {
		name string
		body string
	}{
		{"shopify_THROTTLED", `{"errors":[{"message":"Throttled","extensions":{"code":"THROTTLED"}}]}`},
		{"shopline_REQUEST_FREQUENTLY", `{"errors":[{"message":"too frequent","extensions":{"code":"REQUEST_FREQUENTLY"}}]}`},
		{"shopline_i18nCode", `{"errors":[{"message":"x","extensions":{"i18nCode":"REQUEST_FREQUENTLY"}}]}`},
		{"generic_too_many_requests", `{"errors":[{"message":"Too Many Requests"}]}`},
		{"rate_limit_prose", `{"errors":[{"message":"You have exceeded the rate limit"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, closeSrv := newFake(t, 200, tc.body)
			defer closeSrv()
			s := Exec(context.Background(), p, sc, vars, "widget", false)
			if s.FailKind != FailThrottled {
				t.Fatalf("FailKind = %q, want %q (msg %q)", s.FailKind, FailThrottled, s.FailMsg)
			}
		})
	}

	// A plain schema error must NOT be misread as throttling, or negotiation
	// would back off instead of dropping the offending field.
	p, closeSrv := newFake(t, 200,
		`{"errors":[{"message":"Cannot query field \"foo\" on type \"Product\""}]}`)
	defer closeSrv()
	if s := Exec(context.Background(), p, sc, vars, "widget", false); s.FailKind != FailGraphQL {
		t.Fatalf("schema error classified as %q, want %q", s.FailKind, FailGraphQL)
	}
}
