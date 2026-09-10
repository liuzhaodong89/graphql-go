package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopline/cmp-storefront/internal/config"
	"github.com/shopline/cmp-storefront/internal/gql"
	"github.com/shopline/cmp-storefront/internal/platform"
)

func fakeAt(t *testing.T, name string, h http.HandlerFunc) (*fakePlatform, func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	return &fakePlatform{name: name, c: gql.NewClient(srv.URL, nil, 3*time.Second, true)}, srv.Close
}

func okBody(handle, title string) string {
	return `{"data":{"product":{"id":"gid://1","handle":"` + handle + `","title":"` + title + `"}}}`
}

func testCfg() *config.Config {
	c := &config.Config{}
	c.Shopify.Enabled = true
	c.Fixtures.ProductHandles = []string{"widget"}
	c.Run.SamplesPerScen = 6
	c.Run.Warmup = 0
	c.Run.Concurrency = 1
	c.Run.PairGapMS = 1
	c.Run.QPSReadOnly = 1000
	c.Run.Scenarios = []string{"S2_product_detail"}
	c.Run.Seed = 7
	c.Run.BootstrapIters = 200
	c.Run.Timeout = "3s"
	c.Gates.SizeDeltaMax = 0.10
	c.Gates.MaxErrorRate = 0.01
	c.Gates.MaxSemanticDepth = 2
	c.Gates.ReqSizeAbsTolerance = 256
	c.Fixtures.PageSize = 10
	return c
}

// One-sided failure must invalidate the whole pair. Keeping the healthy half
// would compare a real response against nothing and, because failures cluster
// on slow requests, would flatter the failing platform.
func TestOneSidedFailureDiscardsThePair(t *testing.T) {
	a, closeA := fakeAt(t, "a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody("widget", "Widget")))
	})
	defer closeA()

	var n int
	b, closeB := fakeAt(t, "b", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		n++
		if n%2 == 0 {
			// GraphQL-level failure behind a 200.
			_, _ = w.Write([]byte(`{"errors":[{"message":"boom"}],"data":null}`))
			return
		}
		_, _ = w.Write([]byte(okBody("widget", "Widget")))
	})
	defer closeB()

	cfg := testCfg()
	r := New(a, b, cfg)
	pairs, err := r.Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != cfg.Run.SamplesPerScen {
		t.Fatalf("got %d pairs, want %d", len(pairs), cfg.Run.SamplesPerScen)
	}

	var discarded int
	for _, p := range pairs {
		if p.Discarded {
			discarded++
			if p.B == nil || p.B.OK {
				t.Errorf("pair discarded but B looks healthy: %+v", p.DiscardReason)
			}
			if !strings.Contains(p.DiscardReason, "b:") {
				t.Errorf("discard reason %q does not attribute the failure to B", p.DiscardReason)
			}
		}
	}
	if discarded == 0 {
		t.Fatal("no pairs discarded despite B failing half the time")
	}

	rep := Aggregate(pairs, cfg, a.Name(), b.Name())
	res := rep.Results[0]
	if res.Valid+res.Discarded != res.Total {
		t.Errorf("valid(%d) + discarded(%d) != total(%d)", res.Valid, res.Discarded, res.Total)
	}
	if res.ErrRateB == 0 {
		t.Error("B error rate reported as zero despite induced failures")
	}
	// The error rate blew the gate, so the scenario must not be scored.
	if res.Verdict != VerdictUnreliable {
		t.Errorf("verdict = %s, want %s", res.Verdict, VerdictUnreliable)
	}
}

// Both platforms are hit for every pair, and the order is randomised so the
// second slot's warm-path advantage does not accrue to one platform.
func TestPairOrderIsRandomisedAndBothSidesRun(t *testing.T) {
	var hitsA, hitsB int
	a, closeA := fakeAt(t, "a", func(w http.ResponseWriter, r *http.Request) {
		hitsA++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody("widget", "Widget")))
	})
	defer closeA()
	b, closeB := fakeAt(t, "b", func(w http.ResponseWriter, r *http.Request) {
		hitsB++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody("widget", "Widget")))
	})
	defer closeB()

	cfg := testCfg()
	cfg.Run.SamplesPerScen = 40
	pairs, err := New(a, b, cfg).Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if hitsA != 40 || hitsB != 40 {
		t.Fatalf("hits A=%d B=%d, want 40 each", hitsA, hitsB)
	}

	var swapped int
	for _, p := range pairs {
		if p.SwappedOrder {
			swapped++
		}
		if p.A.Platform != "a" || p.B == nil || p.B.Platform != "b" {
			t.Fatalf("samples not restored to canonical A/B slots: %s / %v", p.A.Platform, p.B)
		}
	}
	if swapped == 0 || swapped == len(pairs) {
		t.Errorf("order swapped %d/%d times; expected a mix", swapped, len(pairs))
	}
}

// Baseline mode runs a single platform so the Shopify side can be validated
// before the second adapter is finished.
func TestBaselineModeRunsOnePlatform(t *testing.T) {
	a, closeA := fakeAt(t, "a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody("widget", "Widget")))
	})
	defer closeA()

	cfg := testCfg()
	pairs, err := New(a, nil, cfg).Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rep := Aggregate(pairs, cfg, "a", "")
	if len(rep.Results) != 1 {
		t.Fatalf("got %d results, want 1", len(rep.Results))
	}
	res := rep.Results[0]
	if res.Verdict != VerdictBaselineOnly {
		t.Errorf("verdict = %s, want %s", res.Verdict, VerdictBaselineOnly)
	}
	if res.DataBytesA == 0 {
		t.Error("baseline recorded no payload size")
	}
	if !strings.Contains(rep.Markdown(), "Baseline mode") {
		t.Error("baseline report does not announce that no comparison was made")
	}
}

// A size gap beyond the budget must block the scenario from being scored.
func TestSizeGateBlocksScoring(t *testing.T) {
	a, closeA := fakeAt(t, "a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody("widget", strings.Repeat("X", 400))))
	})
	defer closeA()
	b, closeB := fakeAt(t, "b", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody("widget", "Widget")))
	})
	defer closeB()

	cfg := testCfg()
	pairs, err := New(a, b, cfg).Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	res := Aggregate(pairs, cfg, "a", "b").Results[0]
	if res.Verdict != VerdictSizeGap {
		t.Fatalf("verdict = %s, want %s (delta was %.3f)", res.Verdict, VerdictSizeGap, res.SizeDelta)
	}
	if res.SizeDelta <= cfg.Gates.SizeDeltaMax {
		t.Fatalf("size delta %.3f should exceed the budget", res.SizeDelta)
	}
}

var _ = platform.Vars{}
