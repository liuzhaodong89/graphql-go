// Package stats provides the summary statistics the comparison report needs.
//
// Everything here works on PAIRED differences where possible. Comparing two
// independent latency distributions across a multi-hour run mostly measures
// when each sample happened; comparing the within-pair difference cancels the
// shared time-varying conditions out.
package stats

import (
	"math"
	"math/rand"
	"sort"
)

type Summary struct {
	N                            int     `json:"n"`
	Min, P50, P90, P95, P99, Max float64 `json:"-"`
	Mean                         float64 `json:"mean_ms"`
	StdDev                       float64 `json:"stddev_ms"`
}

// Quantile uses linear interpolation between order statistics.
func Quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	pos := q * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	frac := pos - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

func Describe(xs []float64) Summary {
	s := Summary{N: len(xs)}
	if len(xs) == 0 {
		return s
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	s.Min, s.Max = sorted[0], sorted[len(sorted)-1]
	s.P50 = Quantile(sorted, 0.50)
	s.P90 = Quantile(sorted, 0.90)
	s.P95 = Quantile(sorted, 0.95)
	s.P99 = Quantile(sorted, 0.99)
	var sum float64
	for _, x := range xs {
		sum += x
	}
	s.Mean = sum / float64(len(xs))
	var ss float64
	for _, x := range xs {
		ss += (x - s.Mean) * (x - s.Mean)
	}
	if len(xs) > 1 {
		s.StdDev = math.Sqrt(ss / float64(len(xs)-1))
	}
	return s
}

// BootstrapCI resamples the paired differences to give a distribution-free
// confidence interval for any statistic (median, p95 gap, ...). Latency is far
// from normal, so a t-interval on the mean would be misleading.
func BootstrapCI(diffs []float64, stat func([]float64) float64, iters int, alpha float64, seed int64) (lo, hi float64) {
	if len(diffs) == 0 {
		return math.NaN(), math.NaN()
	}
	rng := rand.New(rand.NewSource(seed))
	vals := make([]float64, 0, iters)
	buf := make([]float64, len(diffs))
	for i := 0; i < iters; i++ {
		for j := range buf {
			buf[j] = diffs[rng.Intn(len(diffs))]
		}
		vals = append(vals, stat(buf))
	}
	sort.Float64s(vals)
	return Quantile(vals, alpha/2), Quantile(vals, 1-alpha/2)
}

func Median(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return Quantile(s, 0.5)
}

func QuantileOf(q float64) func([]float64) float64 {
	return func(xs []float64) float64 {
		s := append([]float64(nil), xs...)
		sort.Float64s(s)
		return Quantile(s, q)
	}
}

// WilcoxonSignedRank returns the normal-approximation two-sided p-value for
// H0: the paired differences are symmetric about zero. Ties and zeros are
// handled the standard way (zeros dropped, tied ranks averaged).
func WilcoxonSignedRank(diffs []float64) (z, p float64, n int) {
	type absd struct {
		v float64
		s float64
	}
	var nz []absd
	for _, d := range diffs {
		if d == 0 {
			continue
		}
		sign := 1.0
		if d < 0 {
			sign = -1.0
		}
		nz = append(nz, absd{math.Abs(d), sign})
	}
	n = len(nz)
	if n < 10 {
		return math.NaN(), math.NaN(), n // normal approximation not trustworthy
	}
	sort.Slice(nz, func(i, j int) bool { return nz[i].v < nz[j].v })

	ranks := make([]float64, n)
	var tieCorrection float64
	for i := 0; i < n; {
		j := i
		for j+1 < n && nz[j+1].v == nz[i].v {
			j++
		}
		avg := float64(i+j)/2 + 1
		for k := i; k <= j; k++ {
			ranks[k] = avg
		}
		t := float64(j - i + 1)
		tieCorrection += t*t*t - t
		i = j + 1
	}

	var wPlus float64
	for i, r := range ranks {
		if nz[i].s > 0 {
			wPlus += r
		}
	}
	fn := float64(n)
	mean := fn * (fn + 1) / 4
	variance := fn*(fn+1)*(2*fn+1)/24 - tieCorrection/48
	if variance <= 0 {
		return math.NaN(), math.NaN(), n
	}
	// Continuity correction.
	diff := wPlus - mean
	cc := 0.5
	if diff < 0 {
		cc = -0.5
	}
	z = (diff - cc) / math.Sqrt(variance)
	p = 2 * (1 - normalCDF(math.Abs(z)))
	if p > 1 {
		p = 1
	}
	return z, p, n
}

func normalCDF(x float64) float64 { return 0.5 * math.Erfc(-x/math.Sqrt2) }
