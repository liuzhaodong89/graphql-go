package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/shopline/cmp-storefront/internal/gql"
	"github.com/shopline/cmp-storefront/internal/platform"
)

// Fixtures are the handle pools a run draws from.
type Fixtures struct {
	ProductHandles    []string `json:"product_handles"`
	CollectionHandles []string `json:"collection_handles"`
	// ProductTitles feed the S7 search filter. Handles are NOT interchangeable
	// here: the filter is title:'...', and a handle ("red-jasper-bracelet")
	// never matches the title it was derived from ("Red Jasper Bracelet").
	ProductTitles []string `json:"-"`
}

const handleListQuery = `query FixtureHandles($n: Int!) {
  products(first: $n) { nodes { handle title } }
  collections(first: $n) { nodes { handle } }
}`

type handleResp struct {
	Products struct {
		Nodes []struct {
			Handle string
			Title  string
		}
	} `json:"products"`
	Collections struct{ Nodes []struct{ Handle string } } `json:"collections"`
}

// DiscoverFixtures pulls handle pools from each platform and returns the
// INTERSECTION.
//
// The intersection is the point: a handle present on only one store fails
// every pair it appears in, and those failures are not random -- they would
// concentrate wherever the two catalogues diverge. Seeding from the overlap
// keeps the discard rate near zero for reasons that have nothing to do with
// latency.
func DiscoverFixtures(ctx context.Context, ps []platform.Platform, want int) (Fixtures, []string, error) {
	var notes []string
	perPlatform := make([]handleResp, 0, len(ps))

	for _, p := range ps {
		resp := p.Client().Do(ctx, gql.Request{
			Query: handleListQuery, Operation: "FixtureHandles",
			Variables: map[string]any{"n": want},
		})
		if resp.TransportErr != nil {
			return Fixtures{}, notes, fmt.Errorf("%s: %w", p.Name(), resp.TransportErr)
		}
		env, err := resp.Parse()
		if err != nil {
			return Fixtures{}, notes, fmt.Errorf("%s: %w", p.Name(), err)
		}
		if len(env.Errors) > 0 {
			return Fixtures{}, notes, fmt.Errorf("%s: %s", p.Name(),
				p.Client().Redact(env.Errors[0].Message))
		}
		var hr handleResp
		if err := json.Unmarshal(env.Data, &hr); err != nil {
			return Fixtures{}, notes, fmt.Errorf("%s: %w", p.Name(), err)
		}
		notes = append(notes, fmt.Sprintf("%s: %d products, %d collections",
			p.Name(), len(hr.Products.Nodes), len(hr.Collections.Nodes)))
		perPlatform = append(perPlatform, hr)
	}

	prod := intersect(perPlatform, func(h handleResp) []string {
		out := make([]string, 0, len(h.Products.Nodes))
		for _, n := range h.Products.Nodes {
			out = append(out, n.Handle)
		}
		return out
	})
	coll := intersect(perPlatform, func(h handleResp) []string {
		out := make([]string, 0, len(h.Collections.Nodes))
		for _, n := range h.Collections.Nodes {
			out = append(out, n.Handle)
		}
		return out
	})

	titles := intersect(perPlatform, func(h handleResp) []string {
		out := make([]string, 0, len(h.Products.Nodes))
		for _, n := range h.Products.Nodes {
			if n.Title != "" {
				out = append(out, n.Title)
			}
		}
		return out
	})

	if len(ps) > 1 {
		notes = append(notes, fmt.Sprintf("shared: %d products, %d collections", len(prod), len(coll)))
		if len(prod) < 200 {
			notes = append(notes, fmt.Sprintf(
				"WARNING: only %d shared product handles. A small pool means samples repeatedly hit the same warm cache entries; seed both stores from one fixture set to widen it.", len(prod)))
		}
	}
	return Fixtures{ProductHandles: prod, CollectionHandles: coll, ProductTitles: titles}, notes, nil
}

// intersect returns handles present in every platform's list, sorted for a
// deterministic rotation order.
func intersect(rs []handleResp, pick func(handleResp) []string) []string {
	if len(rs) == 0 {
		return nil
	}
	counts := map[string]int{}
	for _, r := range rs {
		seen := map[string]bool{}
		for _, h := range pick(r) {
			if h == "" || seen[h] {
				continue
			}
			seen[h] = true
			counts[h]++
		}
	}
	var out []string
	for h, c := range counts {
		if c == len(rs) {
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out
}
