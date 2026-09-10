package runner

import (
	"context"
	"fmt"
	"sort"

	"github.com/shopline/cmp-storefront/internal/gql"
	"github.com/shopline/cmp-storefront/internal/platform"
	"github.com/shopline/cmp-storefront/internal/scenario"
	"github.com/shopline/cmp-storefront/internal/stats"
)

// SizeCalibration records how a scenario was brought inside the payload budget.
type SizeCalibration struct {
	Scenario   string    `json:"scenario"`
	StartDelta float64   `json:"start_delta"`
	FinalDelta float64   `json:"final_delta"`
	Passed     bool      `json:"passed"`
	Dropped    []DropRec `json:"dropped_fields,omitempty"`
	BytesA     int       `json:"bytes_a"`
	BytesB     int       `json:"bytes_b"`
}

type DropRec struct {
	Field      string  `json:"field"`
	GapBytes   int     `json:"gap_bytes"`
	ShareOfGap float64 `json:"share_of_gap"`
	DeltaAfter float64 `json:"delta_after"`
}

// CalibrateSize brings a scenario inside the payload-size budget by removing
// OPTIONAL fields whose encodings differ structurally between the platforms --
// an ID rendered as a 38-byte base64 gid on one side and a 20-byte integer on
// the other carries the same information at very different cost, and leaving it
// in makes the run a comparison of ID encodings rather than of latency.
//
// Two rules keep this from becoming a way to game the gate:
//   - only fields the scenario declares Optional may go, and a field is always
//     removed from BOTH platforms, so the two sides still ask the same thing;
//   - every removal is recorded with the byte gap that motivated it, and the
//     pre-calibration delta is reported alongside the final one.
//
// A scenario that cannot be brought inside the budget this way stays over
// budget and is reported as incomparable rather than quietly trimmed further.
func CalibrateSize(ctx context.Context, r *Runner, sc scenario.Scenario, samples int, budget float64) (SizeCalibration, error) {
	cal := SizeCalibration{Scenario: sc.ID}
	if r.B == nil {
		cal.Passed = true
		return cal, nil
	}

	measure := func() (int, int, []gql.Attribution, error) {
		var da, db []float64
		var fa, fb map[string]int
		for i := 0; i < samples; i++ {
			v, key := r.varsFor(sc, i)
			p := r.runPair(ctx, sc, v, key, true)
			if p.Discarded || p.B == nil {
				continue
			}
			da = append(da, float64(p.A.DataBytes))
			db = append(db, float64(p.B.DataBytes))
			if fa == nil && p.A.Fields != nil {
				fa, fb = p.A.Fields, p.B.Fields
			}
		}
		if len(da) == 0 {
			return 0, 0, nil, fmt.Errorf("no valid samples during size calibration")
		}
		return int(stats.Median(da)), int(stats.Median(db)), gql.Attribute(fa, fb, nil), nil
	}

	a, b, attr, err := measure()
	if err != nil {
		return cal, err
	}
	cal.StartDelta = gql.SizeDelta(a, b)
	cal.BytesA, cal.BytesB = a, b

	// Bounded by the number of optional fields; each round removes one.
	for round := 0; round < len(sc.Optional)+1; round++ {
		d := gql.SizeDelta(a, b)
		cal.FinalDelta, cal.BytesA, cal.BytesB = d, a, b
		if d <= budget {
			cal.Passed = true
			return cal, nil
		}

		victim, gap, share := largestOptionalGap(attr, sc, r.A)
		if victim == "" {
			return cal, nil // over budget, nothing removable: report as incomparable
		}
		for _, p := range r.platforms() {
			p.Disable(victim)
		}
		if a, b, attr, err = measure(); err != nil {
			return cal, err
		}
		cal.Dropped = append(cal.Dropped, DropRec{
			Field: victim, GapBytes: gap, ShareOfGap: share,
			DeltaAfter: gql.SizeDelta(a, b),
		})
	}
	cal.FinalDelta, cal.BytesA, cal.BytesB = gql.SizeDelta(a, b), a, b
	cal.Passed = cal.FinalDelta <= budget
	return cal, nil
}

// largestOptionalGap picks the removable leaf contributing most to the byte gap.
func largestOptionalGap(attr []gql.Attribution, sc scenario.Scenario, p platform.Platform) (string, int, float64) {
	sort.Slice(attr, func(i, j int) bool { return absI(attr[i].Delta) > absI(attr[j].Delta) })
	for _, row := range attr {
		leaf := lastSegment(row.Path)
		if leaf == "" || !sc.IsOptional(leaf) {
			continue
		}
		return leaf, absI(row.Delta), row.ShareOfGap
	}
	return "", 0, 0
}

func lastSegment(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '.' {
			seg := path[i+1:]
			if seg == "" || seg == "nodes" || seg == "nodes[]" {
				return ""
			}
			return seg
		}
	}
	return path
}

func absI(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
