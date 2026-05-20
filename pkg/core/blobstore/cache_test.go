package blobstore

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a manually-advanced time source for TTL tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestMemoCacheHitMissAndCompute(t *testing.T) {
	t.Parallel()

	clk := &fakeClock{t: time.Unix(0, 0)}
	c := newMemoCache[string](8, time.Minute, clk.now)

	var calls atomic.Int64
	compute := func() (string, error) {
		calls.Add(1)
		return "value", nil
	}

	// First call computes; second is served from cache.
	for i := 0; i < 5; i++ {
		got, err := c.getOrCompute("k", compute)
		if err != nil {
			t.Fatalf("getOrCompute: %v", err)
		}
		if got != "value" {
			t.Fatalf("got %q, want %q", got, "value")
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("compute called %d times, want 1", n)
	}
}

func TestMemoCacheCachesZeroValue(t *testing.T) {
	t.Parallel()

	clk := &fakeClock{t: time.Unix(0, 0)}
	c := newMemoCache[string](8, time.Minute, clk.now)

	var calls atomic.Int64
	miss := func() (string, error) {
		calls.Add(1)
		return "", nil // an empty result is a cacheable miss
	}

	for i := 0; i < 3; i++ {
		got, err := c.getOrCompute("k", miss)
		if err != nil {
			t.Fatalf("getOrCompute: %v", err)
		}
		if got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("miss recomputed %d times, want 1 (empty result must be cached)", n)
	}
}

func TestMemoCacheDoesNotCacheErrors(t *testing.T) {
	t.Parallel()

	clk := &fakeClock{t: time.Unix(0, 0)}
	c := newMemoCache[string](8, time.Minute, clk.now)

	var calls atomic.Int64
	fail := func() (string, error) {
		calls.Add(1)
		return "", fmt.Errorf("boom")
	}

	for i := 0; i < 3; i++ {
		if _, err := c.getOrCompute("k", fail); err == nil {
			t.Fatal("expected error, got nil")
		}
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("errored compute called %d times, want 3 (errors must not be cached)", n)
	}
}

func TestMemoCacheTTLExpiry(t *testing.T) {
	t.Parallel()

	clk := &fakeClock{t: time.Unix(0, 0)}
	c := newMemoCache[string](8, time.Minute, clk.now)

	var calls atomic.Int64
	compute := func() (string, error) {
		calls.Add(1)
		return "v", nil
	}

	if _, err := c.getOrCompute("k", compute); err != nil {
		t.Fatalf("getOrCompute: %v", err)
	}
	clk.advance(2 * time.Minute) // entry is now stale
	if _, err := c.getOrCompute("k", compute); err != nil {
		t.Fatalf("getOrCompute after expiry: %v", err)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("compute called %d times, want 2 (expired entry must recompute)", n)
	}
}

func TestMemoCacheLRUEviction(t *testing.T) {
	t.Parallel()

	clk := &fakeClock{t: time.Unix(0, 0)}
	c := newMemoCache[string](2, time.Hour, clk.now)

	var calls atomic.Int64
	val := func(k string) func() (string, error) {
		return func() (string, error) {
			calls.Add(1)
			return k, nil
		}
	}

	mustGet := func(k string) {
		if _, err := c.getOrCompute(k, val(k)); err != nil {
			t.Fatalf("getOrCompute(%q): %v", k, err)
		}
	}

	mustGet("a")
	mustGet("b")
	mustGet("a") // touch a so b is the LRU victim
	mustGet("c") // capacity 2 -> evicts b
	mustGet("a") // still cached
	if n := calls.Load(); n != 3 {
		t.Fatalf("computes = %d, want 3 (a,b,c each once; a served from cache)", n)
	}
	mustGet("b") // b was evicted -> recompute
	if n := calls.Load(); n != 4 {
		t.Fatalf("computes = %d, want 4 (b recomputed after eviction)", n)
	}
}

func TestClampCacheTTL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"default 19m capped at 1m margin", 19 * time.Minute, 18 * time.Minute},
		{"long ttl capped at 1m margin", 24 * time.Hour, 24*time.Hour - time.Minute},
		{"sub-10m ttl shaves 10%", 5 * time.Minute, 5*time.Minute - 30*time.Second},
		{"tiny ttl shaves 10%", 10 * time.Second, 9 * time.Second},
		{"zero passes through", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := clampCacheTTL(tc.in); got != tc.want {
				t.Errorf("clampCacheTTL(%v) = %v, want %v", tc.in, got, tc.want)
			}
			if tc.in > 0 && clampCacheTTL(tc.in) >= tc.in {
				t.Errorf("clampCacheTTL(%v) must be strictly less than input", tc.in)
			}
		})
	}
}

func TestFlightGroupCollapsesConcurrentCalls(t *testing.T) {
	t.Parallel()

	var g flightGroup[int]
	var calls atomic.Int64
	release := make(chan struct{})
	const n = 50

	fn := func() (int, error) {
		calls.Add(1)
		<-release // hold the flight open until every caller has joined
		return 42, nil
	}

	var wg sync.WaitGroup
	results := make([]int, n)
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, _ := g.Do("k", fn)
			results[i] = v
		}()
	}
	// Give the goroutines time to coalesce on the in-flight call, then release.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("fn invoked %d times, want 1 (singleflight must collapse)", got)
	}
	for i, v := range results {
		if v != 42 {
			t.Fatalf("result[%d] = %d, want 42", i, v)
		}
	}
}
