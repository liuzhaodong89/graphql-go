package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/shopline/cmp-storefront/internal/config"
	"github.com/shopline/cmp-storefront/internal/gql"
	"github.com/shopline/cmp-storefront/internal/scenario"
	"github.com/shopline/cmp-storefront/internal/stats"
)

type Verdict string

const (
	VerdictPass         Verdict = "PASS"
	VerdictSizeGap      Verdict = "INCOMPARABLE_SIZE"
	VerdictUnreliable   Verdict = "UNRELIABLE_ERROR_RATE"
	VerdictNoData       Verdict = "NO_VALID_PAIRS"
	VerdictBaselineOnly Verdict = "BASELINE_ONLY"
)

type ScenarioResult struct {
	Scenario string  `json:"scenario"`
	Title    string  `json:"title"`
	Group    string  `json:"group"`
	Verdict  Verdict `json:"verdict"`
	Notes    string  `json:"notes,omitempty"`

	Total     int `json:"total_pairs"`
	Valid     int `json:"valid_pairs"`
	Discarded int `json:"discarded_pairs"`

	DiscardByReason map[string]int `json:"discard_by_reason,omitempty"`
	ErrRateA        float64        `json:"error_rate_a"`
	ErrRateB        float64        `json:"error_rate_b"`

	DataBytesA int     `json:"data_bytes_a"`
	DataBytesB int     `json:"data_bytes_b"`
	SizeDelta  float64 `json:"size_delta"`
	WireBytesA int64   `json:"wire_bytes_a"`
	WireBytesB int64   `json:"wire_bytes_b"`
	WireDelta  float64 `json:"wire_delta"`
	ReqBytesA  int     `json:"req_bytes_a"`
	ReqBytesB  int     `json:"req_bytes_b"`
	ReqAbsGap  int     `json:"req_abs_gap"`

	ServerA stats.Summary `json:"server_a"`
	ServerB stats.Summary `json:"server_b"`
	TotalA  stats.Summary `json:"total_a"`
	TotalB  stats.Summary `json:"total_b"`

	PairedMedianMS float64 `json:"paired_median_delta_ms"`
	PairedCILo     float64 `json:"paired_ci_lo_ms"`
	PairedCIHi     float64 `json:"paired_ci_hi_ms"`
	P95DeltaMS     float64 `json:"p95_delta_ms"`
	WilcoxonZ      float64 `json:"wilcoxon_z"`
	WilcoxonP      float64 `json:"wilcoxon_p"`

	// WorstCaseP95 recomputes p95 with discarded samples charged at the
	// timeout ceiling. If it disagrees with the headline number, the discards
	// were not random and the headline is not trustworthy.
	WorstCaseP95A float64 `json:"worst_case_p95_a_ms"`
	WorstCaseP95B float64 `json:"worst_case_p95_b_ms"`

	CacheHitA float64 `json:"cache_hit_rate_a"`
	CacheHitB float64 `json:"cache_hit_rate_b"`

	Attribution []gql.Attribution `json:"attribution,omitempty"`
	Baseline    bool              `json:"baseline_only"`
}

type Report struct {
	PlatformA string           `json:"platform_a"`
	PlatformB string           `json:"platform_b,omitempty"`
	Gates     config.Gates     `json:"gates"`
	Baselines []NetBaseline    `json:"network_baselines,omitempty"`
	Results   []ScenarioResult `json:"results"`
}

func Aggregate(pairs []Pair, cfg *config.Config, nameA, nameB string) Report {
	byScen := map[string][]Pair{}
	var order []string
	for _, p := range pairs {
		if _, ok := byScen[p.Scenario]; !ok {
			order = append(order, p.Scenario)
		}
		byScen[p.Scenario] = append(byScen[p.Scenario], p)
	}

	rep := Report{PlatformA: nameA, PlatformB: nameB, Gates: cfg.Gates}
	timeoutMS := float64(cfg.TimeoutDuration()) / 1e6

	for _, id := range order {
		ps := byScen[id]
		sc, _ := scenario.ByID(id)
		r := ScenarioResult{
			Scenario: id, Title: sc.Title, Group: string(sc.Group),
			Notes: sc.Notes, Total: len(ps),
			DiscardByReason: map[string]int{},
			Baseline:        nameB == "",
		}

		var serverA, serverB, totalA, totalB, diffs []float64
		var failA, failB, hitA, hitB, nA, nB int
		var dataA, dataB, reqA, reqB []float64
		var wireA, wireB []float64
		var fieldsA, fieldsB map[string]int

		for _, p := range ps {
			if !p.A.OK {
				failA++
			}
			if p.B != nil && !p.B.OK {
				failB++
			}
			if p.Discarded {
				r.Discarded++
				r.DiscardByReason[p.DiscardReason]++
				continue
			}
			r.Valid++

			serverA = append(serverA, p.A.ServerMS())
			totalA = append(totalA, p.A.TotalMS())
			dataA = append(dataA, float64(p.A.DataBytes))
			wireA = append(wireA, float64(p.A.WireBytes))
			reqA = append(reqA, float64(p.A.ReqBytes))
			nA++
			if isHit(p.A.CacheStatus) {
				hitA++
			}
			if fieldsA == nil && p.A.Fields != nil {
				fieldsA = p.A.Fields
			}

			if p.B != nil {
				serverB = append(serverB, p.B.ServerMS())
				totalB = append(totalB, p.B.TotalMS())
				dataB = append(dataB, float64(p.B.DataBytes))
				wireB = append(wireB, float64(p.B.WireBytes))
				reqB = append(reqB, float64(p.B.ReqBytes))
				nB++
				if isHit(p.B.CacheStatus) {
					hitB++
				}
				if fieldsB == nil && p.B.Fields != nil {
					fieldsB = p.B.Fields
				}
				diffs = append(diffs, p.A.ServerMS()-p.B.ServerMS())
			}
		}

		if len(ps) > 0 {
			r.ErrRateA = float64(failA) / float64(len(ps))
			if nameB != "" {
				r.ErrRateB = float64(failB) / float64(len(ps))
			}
		}
		if nA > 0 {
			r.CacheHitA = float64(hitA) / float64(nA)
		}
		if nB > 0 {
			r.CacheHitB = float64(hitB) / float64(nB)
		}

		r.ServerA = stats.Describe(serverA)
		r.TotalA = stats.Describe(totalA)
		r.DataBytesA = int(stats.Median(dataA))
		r.WireBytesA = int64(stats.Median(wireA))
		r.ReqBytesA = int(stats.Median(reqA))
		r.WorstCaseP95A = worstCaseP95(serverA, r.Discarded, timeoutMS)

		if nameB != "" {
			r.ServerB = stats.Describe(serverB)
			r.TotalB = stats.Describe(totalB)
			r.DataBytesB = int(stats.Median(dataB))
			r.WireBytesB = int64(stats.Median(wireB))
			r.ReqBytesB = int(stats.Median(reqB))
			r.WorstCaseP95B = worstCaseP95(serverB, r.Discarded, timeoutMS)

			r.SizeDelta = gql.SizeDelta(r.DataBytesA, r.DataBytesB)
			r.WireDelta = gql.SizeDelta(int(r.WireBytesA), int(r.WireBytesB))
			r.ReqAbsGap = absInt(r.ReqBytesA - r.ReqBytesB)

			if len(diffs) > 0 {
				r.PairedMedianMS = stats.Median(diffs)
				r.PairedCILo, r.PairedCIHi = stats.BootstrapCI(
					diffs, stats.Median, cfg.Run.BootstrapIters, 0.05, cfg.Run.Seed)
				r.P95DeltaMS = r.ServerA.P95 - r.ServerB.P95
				r.WilcoxonZ, r.WilcoxonP, _ = stats.WilcoxonSignedRank(diffs)
			}
			if fieldsA != nil && fieldsB != nil {
				rows := gql.Attribute(fieldsA, fieldsB, nil)
				if len(rows) > 12 {
					rows = rows[:12]
				}
				r.Attribution = rows
			}
		}

		r.Verdict = verdict(r, cfg, nameB == "")
		rep.Results = append(rep.Results, r)
	}
	return rep
}

func verdict(r ScenarioResult, cfg *config.Config, baseline bool) Verdict {
	if r.Valid == 0 {
		return VerdictNoData
	}
	if r.ErrRateA > cfg.Gates.MaxErrorRate || r.ErrRateB > cfg.Gates.MaxErrorRate {
		return VerdictUnreliable
	}
	if baseline {
		return VerdictBaselineOnly
	}
	if r.SizeDelta > cfg.Gates.SizeDeltaMax {
		return VerdictSizeGap
	}
	return VerdictPass
}

// worstCaseP95 charges every discarded sample at the timeout ceiling. A large
// gap between this and the headline p95 means the discards were biased.
func worstCaseP95(xs []float64, discarded int, timeoutMS float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	aug := append([]float64(nil), xs...)
	for i := 0; i < discarded; i++ {
		aug = append(aug, timeoutMS)
	}
	sort.Float64s(aug)
	return stats.Quantile(aug, 0.95)
}

func isHit(cache string) bool {
	l := strings.ToLower(cache)
	return strings.Contains(l, "hit")
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func (rep Report) WriteJSON(path string) error {
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// Markdown renders the comparison table. Scenarios whose verdict is not PASS
// are rendered but must not be aggregated into an overall claim.
func (rep Report) Markdown() string {
	var b strings.Builder
	baseline := rep.PlatformB == ""
	nameB := rep.PlatformB
	if baseline {
		nameB = "(not configured)"
	}

	fmt.Fprintf(&b, "# Storefront API latency comparison\n\n")
	fmt.Fprintf(&b, "A = **%s**, B = **%s**\n\n", rep.PlatformA, nameB)
	fmt.Fprintf(&b, "Gates: size delta <= %.0f%%, semantic depth <= %d, error rate <= %.1f%%\n\n",
		rep.Gates.SizeDeltaMax*100, rep.Gates.MaxSemanticDepth, rep.Gates.MaxErrorRate*100)

	if baseline {
		b.WriteString("> Baseline mode: one platform only. No comparison verdict is issued.\n\n")
		b.WriteString("| Scenario | Valid | Discard | data B | wire B | req B | ServerTime p50/p95 | Cache hit |\n")
		b.WriteString("|---|---:|---:|---:|---:|---:|---|---:|\n")
		for _, r := range rep.Results {
			fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %d | %.1f / %.1f ms | %.0f%% |\n",
				r.Scenario, r.Valid, r.Discarded, r.DataBytesA, r.WireBytesA, r.ReqBytesA,
				r.ServerA.P50, r.ServerA.P95, r.CacheHitA*100)
		}
	} else {
		b.WriteString("| Scenario | Valid | Discard | δD | δW | A p50/p95 | B p50/p95 | paired median Δ [95% CI] | Wilcoxon p | Verdict |\n")
		b.WriteString("|---|---:|---:|---:|---:|---|---|---|---:|---|\n")
		for _, r := range rep.Results {
			fmt.Fprintf(&b, "| %s | %d | %d | %.1f%% | %.1f%% | %.1f / %.1f | %.1f / %.1f | %+.1f [%+.1f, %+.1f] ms | %.4f | %s |\n",
				r.Scenario, r.Valid, r.Discarded, r.SizeDelta*100, r.WireDelta*100,
				r.ServerA.P50, r.ServerA.P95, r.ServerB.P50, r.ServerB.P95,
				r.PairedMedianMS, r.PairedCILo, r.PairedCIHi, r.WilcoxonP, r.Verdict)
		}
	}

	if len(rep.Baselines) > 0 {
		b.WriteString("\n## Network floor\n\n")
		b.WriteString("Handshake cost to each edge, measured on fresh connections. Latency below\n")
		b.WriteString("this floor is impossible; a gap here is distance, not server speed.\n\n")
		b.WriteString("| Platform | Edge IP | TCP connect p50 | TCP min | TLS handshake p50 |\n|---|---|---:|---:|---:|\n")
		for _, nb := range rep.Baselines {
			fmt.Fprintf(&b, "| %s | %s | %.1f ms | %.1f ms | %.1f ms |\n",
				nb.Platform, nb.RemoteIP, nb.TCPMedianMS, nb.TCPMinMS, nb.TLSMedianMS)
		}
		if len(rep.Baselines) == 2 {
			gap := rep.Baselines[0].TCPMedianMS - rep.Baselines[1].TCPMedianMS
			fmt.Fprintf(&b, "\nRound-trip difference between the two edges: **%+.1f ms**. "+
				"Subtract roughly one RTT from each ServerTime to approximate actual server work.\n", gap)
		}
	}

	b.WriteString("\n## Discard reasons\n\n")
	anyDiscards := false
	for _, r := range rep.Results {
		if len(r.DiscardByReason) > 0 {
			anyDiscards = true
			break
		}
	}
	if !anyDiscards {
		b.WriteString("No pairs were discarded.\n")
	}
	for _, r := range rep.Results {
		if len(r.DiscardByReason) == 0 {
			continue
		}
		fmt.Fprintf(&b, "- **%s** (err A %.2f%%", r.Scenario, r.ErrRateA*100)
		if !baseline {
			fmt.Fprintf(&b, ", err B %.2f%%", r.ErrRateB*100)
		}
		b.WriteString("): ")
		var keys []string
		for k := range r.DiscardByReason {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", k, r.DiscardByReason[k]))
		}
		b.WriteString(strings.Join(parts, ", ") + "\n")
	}

	b.WriteString("\n## Sensitivity to discards\n\n")
	b.WriteString("Worst case charges every discarded sample at the timeout ceiling.\n\n")
	b.WriteString("| Scenario | A p95 | A worst-case p95 | B p95 | B worst-case p95 |\n|---|---:|---:|---:|---:|\n")
	for _, r := range rep.Results {
		fmt.Fprintf(&b, "| %s | %.1f | %.1f | %.1f | %.1f |\n",
			r.Scenario, r.ServerA.P95, r.WorstCaseP95A, r.ServerB.P95, r.WorstCaseP95B)
	}

	if !baseline {
		b.WriteString("\n## Per-field byte attribution\n\n")
		for _, r := range rep.Results {
			table := gql.FormatAttribution(r.Attribution, 12)
			// A table with only its header means every field matched exactly;
			// printing it adds noise without adding information.
			if len(r.Attribution) == 0 || strings.Count(table, "\n") < 2 {
				continue
			}
			fmt.Fprintf(&b, "### %s (δD = %.1f%%)\n\n```\n%s```\n\n",
				r.Scenario, r.SizeDelta*100, table)
		}
	}
	return b.String()
}
