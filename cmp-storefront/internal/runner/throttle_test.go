package runner

import (
	"context"
	"testing"
	"time"
)

// SHOPLINE meters by compute time, so an expensive query must consume budget
// faster than a cheap one and the pacer must slow down accordingly.
func TestComputeThrottlerPacesByObservedServerTime(t *testing.T) {
	ctx := context.Background()

	// A 100ms window admits 100ms of server time. Three 40ms requests exceed
	// it, so the third must be made to wait for the window to roll.
	ct := newComputeThrottler(1.0, 1.0)
	ct.window = 100 * time.Millisecond

	start := time.Now()
	for i := 0; i < 4; i++ {
		ct.wait(ctx)
		ct.observe(40*time.Millisecond, false)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("no pacing applied: %v elapsed for 160ms of compute against a 100ms budget", elapsed)
	}

	// The same request count with cheap queries must not be paced at all:
	// pacing tracks cost, not request count.
	cheap := newComputeThrottler(1.0, 1.0)
	cheap.window = 100 * time.Millisecond
	start = time.Now()
	for i := 0; i < 20; i++ {
		cheap.wait(ctx)
		cheap.observe(time.Millisecond, false)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("cheap requests were paced unnecessarily: %v", elapsed)
	}
}

// A throttle response must tighten the budget, because a throttled sample is a
// discarded pair and discards that cluster on slow requests bias the result.
func TestComputeThrottlerTightensAfterThrottle(t *testing.T) {
	ct := newComputeThrottler(1.0, 1.0)
	before := ct.safety
	ct.observe(10*time.Millisecond, true)
	if ct.safety >= before {
		t.Fatalf("safety factor did not tighten: %v -> %v", before, ct.safety)
	}
	if ct.penalty == 0 {
		t.Error("no cool-off applied after a throttle")
	}
}

func TestQPSThrottlerBacksOffAndRecovers(t *testing.T) {
	q := newQPSThrottler(100) // 10ms base gap
	base := q.gap
	q.observe(0, true)
	if q.gap <= base {
		t.Fatalf("gap did not grow after throttle: %v -> %v", base, q.gap)
	}
	backed := q.gap
	for i := 0; i < 200; i++ {
		q.observe(0, false)
	}
	if q.gap != base {
		t.Fatalf("gap did not recover to %v (backed off to %v, now %v)", base, backed, q.gap)
	}
}

func TestQPSThrottlerBackoffIsBounded(t *testing.T) {
	q := newQPSThrottler(100)
	for i := 0; i < 50; i++ {
		q.observe(0, true)
	}
	if q.gap > 30*q.baseGap {
		t.Fatalf("backoff unbounded: gap %v exceeds 30x base %v", q.gap, q.baseGap)
	}
}

func TestThrottlersRespectContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ct := newComputeThrottler(0.001, 1.0) // tiny budget: always wants to wait
	ct.window = time.Hour
	ct.observe(time.Second, false)
	cancel()
	done := make(chan struct{})
	go func() { ct.wait(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("compute throttler ignored context cancellation")
	}
}
