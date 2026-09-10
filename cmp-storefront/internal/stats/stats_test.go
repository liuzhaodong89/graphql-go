package stats

import (
	"math"
	"math/rand"
	"testing"
)

func TestQuantileInterpolates(t *testing.T) {
	xs := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if got := Quantile(xs, 0.5); math.Abs(got-5.5) > 1e-9 {
		t.Fatalf("p50 = %v, want 5.5", got)
	}
	if got := Quantile(xs, 0); got != 1 {
		t.Fatalf("p0 = %v, want 1", got)
	}
	if got := Quantile(xs, 1); got != 10 {
		t.Fatalf("p100 = %v, want 10", got)
	}
}

func TestDescribeOnEmptyIsSafe(t *testing.T) {
	s := Describe(nil)
	if s.N != 0 || s.Mean != 0 {
		t.Fatalf("unexpected summary for empty input: %+v", s)
	}
}

// A real difference must be detected, and no-difference must not be.
func TestWilcoxonDetectsAndRejects(t *testing.T) {
	rng := rand.New(rand.NewSource(42))

	var shifted, centred []float64
	for i := 0; i < 400; i++ {
		shifted = append(shifted, rng.NormFloat64()*5+12) // B is 12ms faster
		centred = append(centred, rng.NormFloat64()*5)
	}

	if _, p, _ := WilcoxonSignedRank(shifted); p > 0.001 {
		t.Errorf("a 12ms shift went undetected: p = %v", p)
	}
	if _, p, _ := WilcoxonSignedRank(centred); p < 0.01 {
		t.Errorf("no real difference but p = %v", p)
	}
}

// Below the sample size where the normal approximation holds, the test must
// report NaN rather than a confident-looking number.
func TestWilcoxonRefusesTinySamples(t *testing.T) {
	_, p, n := WilcoxonSignedRank([]float64{1, -2, 3})
	if !math.IsNaN(p) {
		t.Fatalf("p = %v, want NaN for n=%d", p, n)
	}
}

func TestWilcoxonHandlesZerosAndTies(t *testing.T) {
	d := make([]float64, 0, 40)
	for i := 0; i < 20; i++ {
		d = append(d, 0)
	}
	for i := 0; i < 20; i++ {
		d = append(d, 3) // all tied, all positive
	}
	z, p, n := WilcoxonSignedRank(d)
	if n != 20 {
		t.Fatalf("n = %d, want 20 after dropping zeros", n)
	}
	if math.IsNaN(z) || math.IsNaN(p) {
		t.Fatalf("tie correction produced NaN: z=%v p=%v", z, p)
	}
	if p > 0.001 {
		t.Fatalf("20 identical positive differences should be significant, p = %v", p)
	}
}

// The bootstrap interval must bracket the true median and shrink as evidence
// accumulates; a fixed seed keeps the report reproducible.
func TestBootstrapCIBracketsAndIsDeterministic(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	var d []float64
	for i := 0; i < 500; i++ {
		d = append(d, rng.NormFloat64()*4+10)
	}
	lo, hi := BootstrapCI(d, Median, 1000, 0.05, 99)
	if lo > 10 || hi < 10 {
		t.Fatalf("CI [%v, %v] does not bracket the true median 10", lo, hi)
	}
	lo2, hi2 := BootstrapCI(d, Median, 1000, 0.05, 99)
	if lo != lo2 || hi != hi2 {
		t.Fatal("same seed produced a different interval")
	}
}

func TestQuantileOfBuildsAStatFunc(t *testing.T) {
	f := QuantileOf(0.95)
	xs := make([]float64, 100)
	for i := range xs {
		xs[i] = float64(i + 1)
	}
	if got := f(xs); math.Abs(got-95.05) > 0.5 {
		t.Fatalf("p95 = %v, want ~95", got)
	}
}
