package runner

import (
	"context"
	"sync"
	"time"
)

// throttler paces requests to one platform. The two storefronts meter
// differently, so they cannot share a pacing model:
//
//   - Shopify rate-limits Storefront calls per IP by request rate.
//   - SHOPLINE rate-limits by COMPUTE TIME: roughly one second of server time
//     per app per IP per second. A slow query therefore consumes budget far
//     faster than a fast one, and a fixed QPS that is safe for cheap scenarios
//     will throttle on expensive ones.
//
// Both implementations back off multiplicatively when a throttle is observed,
// because a throttled sample is a discarded pair, and discards that cluster on
// slow requests bias the whole comparison.
type throttler interface {
	wait(ctx context.Context)
	observe(serverTime time.Duration, throttled bool)
}

func sleepUntil(ctx context.Context, at time.Time) {
	d := time.Until(at)
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// qpsThrottler paces by request rate with multiplicative backoff.
type qpsThrottler struct {
	mu      sync.Mutex
	next    time.Time
	gap     time.Duration
	baseGap time.Duration
}

func newQPSThrottler(qps float64) *qpsThrottler {
	if qps <= 0 {
		return &qpsThrottler{}
	}
	g := time.Duration(float64(time.Second) / qps)
	return &qpsThrottler{gap: g, baseGap: g}
}

func (q *qpsThrottler) wait(ctx context.Context) {
	q.mu.Lock()
	if q.gap == 0 {
		q.mu.Unlock()
		return
	}
	now := time.Now()
	if q.next.Before(now) {
		q.next = now
	}
	at := q.next
	q.next = q.next.Add(q.gap)
	q.mu.Unlock()
	sleepUntil(ctx, at)
}

func (q *qpsThrottler) observe(_ time.Duration, throttled bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.baseGap == 0 {
		return
	}
	if throttled {
		q.gap *= 2
		if max := 30 * q.baseGap; q.gap > max {
			q.gap = max
		}
		return
	}
	// Recover gradually toward the configured rate.
	if q.gap > q.baseGap {
		q.gap = time.Duration(float64(q.gap) * 0.95)
		if q.gap < q.baseGap {
			q.gap = q.baseGap
		}
	}
}

// computeThrottler models a compute-time budget: at most `budget` seconds of
// observed server time may be spent per second. It keeps a sliding window of
// what recent requests cost and waits until enough has aged out.
type computeThrottler struct {
	mu      sync.Mutex
	window  time.Duration
	budget  float64 // seconds of server time allowed per second of wall time
	safety  float64
	spent   []spend
	penalty time.Duration
}

type spend struct {
	at   time.Time
	cost time.Duration
}

// computeWindow matches SHOPLINE's documented 60-second bucket. Using a
// one-second window instead would forbid the bursting the real limiter allows,
// pacing the run far below what the platform actually permits.
const computeWindow = 60 * time.Second

func newComputeThrottler(budgetPerSecond, safety float64) *computeThrottler {
	if budgetPerSecond <= 0 {
		budgetPerSecond = 1.0
	}
	if safety <= 0 || safety > 1 {
		safety = 0.6
	}
	return &computeThrottler{window: computeWindow, budget: budgetPerSecond, safety: safety}
}

func (c *computeThrottler) wait(ctx context.Context) {
	for {
		c.mu.Lock()
		now := time.Now()
		cut := now.Add(-c.window)
		keep := c.spent[:0]
		var used time.Duration
		for _, s := range c.spent {
			if s.at.After(cut) {
				keep = append(keep, s)
				used += s.cost
			}
		}
		c.spent = keep
		// Budget is expressed per second; scale it to the window actually used.
		allowed := time.Duration(c.budget * c.safety * float64(c.window))
		penalty := c.penalty
		oldest := time.Time{}
		if len(c.spent) > 0 {
			oldest = c.spent[0].at
		}
		c.mu.Unlock()

		if penalty > 0 {
			sleepUntil(ctx, now.Add(penalty))
			c.mu.Lock()
			c.penalty = 0
			c.mu.Unlock()
			continue
		}
		if used < allowed || oldest.IsZero() {
			return
		}
		// Wait for the oldest sample to leave the window.
		sleepUntil(ctx, oldest.Add(c.window))
		if ctx.Err() != nil {
			return
		}
	}
}

func (c *computeThrottler) observe(serverTime time.Duration, throttled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if serverTime > 0 {
		c.spent = append(c.spent, spend{at: time.Now(), cost: serverTime})
	}
	if throttled {
		// Give the bucket a full window to drain before trying again.
		c.penalty = c.window
		c.safety *= 0.7
		if c.safety < 0.05 {
			c.safety = 0.05
		}
	}
}
