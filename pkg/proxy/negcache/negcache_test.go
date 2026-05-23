package negcache

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMarkAndHas(t *testing.T) {
	t.Parallel()

	c := New(time.Minute)
	if c.Has("ns", "pypi", "requests") {
		t.Fatal("Has() = true before Mark")
	}
	c.Mark("ns", "pypi", "requests")
	if !c.Has("ns", "pypi", "requests") {
		t.Fatal("Has() = false after Mark")
	}
}

func TestKeyIsolation(t *testing.T) {
	t.Parallel()

	c := New(time.Minute)
	c.Mark("ns", "pypi", "requests")
	if c.Has("other", "pypi", "requests") {
		t.Fatal("namespace did not isolate")
	}
	if c.Has("ns", "npm", "requests") {
		t.Fatal("format did not isolate")
	}
	if c.Has("ns", "pypi", "flask") {
		t.Fatal("key did not isolate")
	}
}

func TestTTLExpiry(t *testing.T) {
	t.Parallel()

	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	c := New(30*time.Second, withClock(clock))

	c.Mark("ns", "pypi", "requests")
	if !c.Has("ns", "pypi", "requests") {
		t.Fatal("Has() = false immediately after Mark")
	}

	now = now.Add(29 * time.Second)
	if !c.Has("ns", "pypi", "requests") {
		t.Fatal("Has() = false before TTL")
	}

	now = now.Add(2 * time.Second) // 31s total, past 30s TTL
	if c.Has("ns", "pypi", "requests") {
		t.Fatal("Has() = true after TTL")
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()

	c := New(time.Minute)
	c.Mark("ns", "pypi", "requests")
	c.Delete("ns", "pypi", "requests")
	if c.Has("ns", "pypi", "requests") {
		t.Fatal("Has() = true after Delete")
	}
}

func TestDefaultTTL(t *testing.T) {
	t.Parallel()
	if got := New(0).ttl; got != DefaultTTL {
		t.Fatalf("New(0).ttl = %v, want %v", got, DefaultTTL)
	}
	if got := New(-time.Second).ttl; got != DefaultTTL {
		t.Fatalf("New(-1s).ttl = %v, want %v", got, DefaultTTL)
	}
}

func TestConcurrentAccess(t *testing.T) {
	t.Parallel()

	c := New(time.Minute)
	const workers = 50
	var wg sync.WaitGroup
	var hits int64

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("pkg-%d", i%5)
			c.Mark("ns", "pypi", key)
			if c.Has("ns", "pypi", key) {
				atomic.AddInt64(&hits, 1)
			}
			c.Delete("ns", "pypi", key)
		}(i)
	}
	wg.Wait()

	if hits == 0 {
		t.Fatal("expected at least some hits under concurrency")
	}
}
