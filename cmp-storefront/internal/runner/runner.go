package runner

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/shopline/cmp-storefront/internal/config"
	"github.com/shopline/cmp-storefront/internal/platform"
	"github.com/shopline/cmp-storefront/internal/scenario"
)

// Pair is one logical request executed on both platforms back to back.
// In baseline mode (one platform configured) B is nil.
type Pair struct {
	Scenario      string  `json:"scenario"`
	Key           string  `json:"key"`
	A             Sample  `json:"a"`
	B             *Sample `json:"b,omitempty"`
	Discarded     bool    `json:"discarded"`
	DiscardReason string  `json:"discard_reason,omitempty"`
	// SwappedOrder records which platform went first, so an order effect can
	// be tested for rather than assumed away.
	SwappedOrder bool `json:"swapped_order"`
}

type Runner struct {
	A   platform.Platform
	B   platform.Platform // nil => baseline mode
	Cfg *config.Config

	mu  sync.Mutex
	rng *rand.Rand
	// Throttlers are per platform because the two storefronts meter
	// differently: Shopify by request rate, SHOPLINE by compute time.
	throttle map[string]throttler
}

func New(a, b platform.Platform, cfg *config.Config) *Runner {
	r := &Runner{A: a, B: b, Cfg: cfg, rng: rand.New(rand.NewSource(cfg.Run.Seed)),
		throttle: map[string]throttler{}}
	for _, p := range r.platforms() {
		r.throttle[p.Name()] = newThrottlerFor(p.Name(), cfg)
	}
	return r
}

func newThrottlerFor(name string, cfg *config.Config) throttler {
	if t, ok := cfg.Run.Throttle[name]; ok {
		switch t.Mode {
		case "compute_budget":
			return newComputeThrottler(t.BudgetSecondsPerSecond, t.Safety)
		case "qps":
			return newQPSThrottler(t.QPS)
		}
	}
	// SHOPLINE documents a time-based limit (one second of compute per app per
	// IP per second), so pacing it by request count either wastes headroom on
	// cheap queries or throttles on expensive ones.
	if name == "shopline" {
		return newComputeThrottler(1.0, 0.6)
	}
	return newQPSThrottler(cfg.Run.QPSReadOnly)
}

func (r *Runner) waitFor(ctx context.Context, p platform.Platform) {
	if t, ok := r.throttle[p.Name()]; ok {
		t.wait(ctx)
	}
}

func (r *Runner) observed(p platform.Platform, s Sample) {
	if t, ok := r.throttle[p.Name()]; ok {
		t.observe(s.Timing.ServerTime(), s.FailKind == FailThrottled)
	}
}

func (r *Runner) intn(n int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rng.Intn(n)
}

// varsFor picks the inputs for one sample. Handles rotate through the pool so
// the run does not degenerate into repeatedly hitting one warm cache entry.
func (r *Runner) varsFor(sc scenario.Scenario, i int) (platform.Vars, string) {
	f := r.Cfg.Fixtures
	v := platform.Vars{PageSize: f.PageSize}
	switch sc.Kind {
	case scenario.KindByProductHandle:
		h := f.ProductHandles[i%len(f.ProductHandles)]
		v.ProductHandle = h
		return v, h
	case scenario.KindByCollectionHandle:
		if len(f.CollectionHandles) == 0 {
			return v, ""
		}
		h := f.CollectionHandles[i%len(f.CollectionHandles)]
		v.CollectionHandle = h
		return v, h
	case scenario.KindSearch:
		if len(f.SearchTerms) == 0 {
			return v, ""
		}
		t := f.SearchTerms[i%len(f.SearchTerms)]
		v.SearchTerm = t
		return v, t
	case scenario.KindLogin:
		if len(f.Accounts) == 0 {
			return v, ""
		}
		a := f.Accounts[i%len(f.Accounts)]
		v.Email, v.Password = a.Email, a.Password
		return v, a.Email
	}
	return v, fmt.Sprintf("static-%d", i)
}

// CheckDepth verifies every bound query stays inside the depth budget. This
// runs before any traffic: a query that drifted past the budget invalidates the
// comparison regardless of what the numbers say.
func (r *Runner) CheckDepth() []string {
	var problems []string
	v, _ := r.varsFor(scenario.Scenario{Kind: scenario.KindByProductHandle}, 0)
	v.CollectionHandle = first(r.Cfg.Fixtures.CollectionHandles)
	v.SearchTerm = first(r.Cfg.Fixtures.SearchTerms)
	v.PageSize = r.Cfg.Fixtures.PageSize
	for _, p := range r.platforms() {
		for _, sc := range r.selected() {
			b, err := p.Bind(sc, v)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s/%s: %v", p.Name(), sc.ID, err))
				continue
			}
			if d := scenario.SemanticDepth(b.Query); d > r.Cfg.Gates.MaxSemanticDepth {
				problems = append(problems, fmt.Sprintf(
					"%s/%s: semantic depth %d exceeds budget %d", p.Name(), sc.ID, d, r.Cfg.Gates.MaxSemanticDepth))
			}
		}
	}
	return problems
}

func first(xs []string) string {
	if len(xs) == 0 {
		return ""
	}
	return xs[0]
}

func (r *Runner) platforms() []platform.Platform {
	if r.B == nil {
		return []platform.Platform{r.A}
	}
	return []platform.Platform{r.A, r.B}
}

func (r *Runner) selected() []scenario.Scenario {
	all := scenario.All()
	if len(r.Cfg.Run.Scenarios) == 0 {
		return all
	}
	want := map[string]bool{}
	for _, id := range r.Cfg.Run.Scenarios {
		want[id] = true
	}
	var out []scenario.Scenario
	for _, s := range all {
		if want[s.ID] {
			out = append(out, s)
		}
	}
	return out
}

// runPair executes one logical request on both platforms in randomised order
// with a short gap. Randomising the order cancels the systematic advantage the
// second call would otherwise get from a warmed local path; the short gap keeps
// both calls inside the same network conditions.
func (r *Runner) runPair(ctx context.Context, sc scenario.Scenario, v platform.Vars, key string, collectFields bool) Pair {
	p := Pair{Scenario: sc.ID, Key: key}

	if r.B == nil {
		r.waitFor(ctx, r.A)
		p.A = Exec(ctx, r.A, sc, v, key, collectFields)
		r.observed(r.A, p.A)
		if !p.A.OK {
			p.Discarded = true
			p.DiscardReason = fmt.Sprintf("%s:%s", r.A.Name(), p.A.FailKind)
		}
		return p
	}

	first, second := r.A, r.B
	swapped := r.intn(2) == 1
	if swapped {
		first, second = second, first
	}
	p.SwappedOrder = swapped

	r.waitFor(ctx, first)
	s1 := Exec(ctx, first, sc, v, key, collectFields)
	r.observed(first, s1)
	select {
	case <-ctx.Done():
		p.Discarded, p.DiscardReason = true, "cancelled"
		return p
	case <-time.After(time.Duration(r.Cfg.Run.PairGapMS) * time.Millisecond):
	}
	r.waitFor(ctx, second)
	s2 := Exec(ctx, second, sc, v, key, collectFields)
	r.observed(second, s2)

	if swapped {
		s1, s2 = s2, s1
	}
	p.A, p.B = s1, &s2

	// Either side failing invalidates the whole pair. Keeping the successful
	// half would compare a real response against nothing.
	if !p.A.OK || !p.B.OK {
		p.Discarded = true
		switch {
		case !p.A.OK && !p.B.OK:
			p.DiscardReason = fmt.Sprintf("both:%s/%s", p.A.FailKind, p.B.FailKind)
		case !p.A.OK:
			p.DiscardReason = fmt.Sprintf("%s:%s", p.A.Platform, p.A.FailKind)
		default:
			p.DiscardReason = fmt.Sprintf("%s:%s", p.B.Platform, p.B.FailKind)
		}
	}
	return p
}

// Run performs warmup then the measured pass for every selected scenario.
func (r *Runner) Run(ctx context.Context, progress func(string)) ([]Pair, error) {
	var all []Pair
	for _, sc := range r.selected() {
		if skip, why := r.skipReason(sc); skip {
			if progress != nil {
				progress(fmt.Sprintf("skip %s: %s", sc.ID, why))
			}
			continue
		}
		conc := r.Cfg.Run.Concurrency
		authLimit := (*qpsThrottler)(nil)
		if sc.Group == scenario.GroupAuth {
			// Login endpoints are brute-force protected on both platforms and
			// their cost profile is unrelated to query latency.
			conc = 1
			authLimit = newQPSThrottler(r.Cfg.Run.QPSAuth)
		}

		// Warmup fills connection pools and any first-request caches. It is
		// discarded: including it would report TLS handshakes as query latency.
		for i := 0; i < r.Cfg.Run.Warmup; i++ {
			v, key := r.varsFor(sc, i)
			if authLimit != nil {
				authLimit.wait(ctx)
			}
			r.runPair(ctx, sc, v, key, false)
			if ctx.Err() != nil {
				return all, ctx.Err()
			}
		}

		if progress != nil {
			progress(fmt.Sprintf("running %s (%d samples, conc=%d)", sc.ID, r.Cfg.Run.SamplesPerScen, conc))
		}

		out := make(chan Pair, r.Cfg.Run.SamplesPerScen)
		var wg sync.WaitGroup
		idx := make(chan int, r.Cfg.Run.SamplesPerScen)
		for i := 0; i < r.Cfg.Run.SamplesPerScen; i++ {
			idx <- i
		}
		close(idx)

		for w := 0; w < conc; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range idx {
					if ctx.Err() != nil {
						return
					}
					v, key := r.varsFor(sc, i)
					if authLimit != nil {
						authLimit.wait(ctx)
					}
					// Field-level byte attribution is only needed on a sample
					// of requests; collecting it every time is pure overhead.
					out <- r.runPair(ctx, sc, v, key, i < 20)
				}
			}()
		}
		wg.Wait()
		close(out)
		for p := range out {
			all = append(all, p)
		}
		if ctx.Err() != nil {
			return all, ctx.Err()
		}
	}
	return all, nil
}

func (r *Runner) skipReason(sc scenario.Scenario) (bool, string) {
	f := r.Cfg.Fixtures
	switch sc.Kind {
	case scenario.KindByCollectionHandle:
		if len(f.CollectionHandles) == 0 {
			return true, "no collection handles configured"
		}
	case scenario.KindSearch:
		if len(f.SearchTerms) == 0 {
			return true, "no search terms configured"
		}
	case scenario.KindLogin:
		if len(f.Accounts) == 0 {
			return true, "no test accounts configured"
		}
	}
	return false, ""
}
