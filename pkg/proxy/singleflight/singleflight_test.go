package singleflight

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrentSameKeyCollapses(t *testing.T) {
	t.Parallel()

	var g Group[int]
	const workers = 20

	var calls int64
	start := make(chan struct{})
	gate := make(chan struct{})
	started := make(chan struct{}, 1)

	// fn blocks until released, so all workers pile onto the same in-flight call.
	fn := func() (int, error) {
		atomic.AddInt64(&calls, 1)
		started <- struct{}{}
		<-gate
		return 42, nil
	}

	var wg sync.WaitGroup
	results := make([]int, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			v, err, _ := g.Do("key", fn)
			if err != nil {
				t.Errorf("Do() = %v", err)
			}
			results[i] = v
		}(i)
	}

	close(start) // unleash all workers onto the same key
	<-started    // the single in-flight fill has begun
	// Let any straggler workers reach Do and coalesce onto the in-flight call
	// before we release it; otherwise a late arrival could start a second fill.
	time.Sleep(100 * time.Millisecond)
	close(gate)
	wg.Wait()

	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("fill executed %d times, want exactly 1", got)
	}
	for i, v := range results {
		if v != 42 {
			t.Fatalf("results[%d] = %d, want 42", i, v)
		}
	}
}

func TestDifferentKeysRunSeparately(t *testing.T) {
	t.Parallel()

	var g Group[string]
	a, _, _ := g.Do("a", func() (string, error) { return "A", nil })
	b, _, _ := g.Do("b", func() (string, error) { return "B", nil })
	if a != "A" || b != "B" {
		t.Fatalf("got a=%q b=%q", a, b)
	}
}

func TestErrorPropagates(t *testing.T) {
	t.Parallel()

	var g Group[int]
	sentinel := errors.New("boom")
	v, err, _ := g.Do("k", func() (int, error) { return 0, sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("Do() err = %v, want %v", err, sentinel)
	}
	if v != 0 {
		t.Fatalf("Do() value = %d, want zero", v)
	}
}
