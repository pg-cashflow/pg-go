package search

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCache_coalescesAndCaches(t *testing.T) {
	c := NewCache(time.Second, 10)
	var calls int32
	fn := func() ([]Result, bool, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(30 * time.Millisecond)
		return []Result{{ID: "x"}}, false, nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _, _ = c.Do("k", fn) }()
	}
	wg.Wait()
	_, _, _ = c.Do("k", fn)
	if calls != 1 {
		t.Fatalf("calls=%d want 1", calls)
	}
}

func TestCache_doesNotCachePartialOrScopes(t *testing.T) {
	c := NewCache(time.Second, 10)
	n := 0
	partial := func() ([]Result, bool, error) { n++; return nil, true, nil }
	_, _, _ = c.Do("k", partial)
	_, _, _ = c.Do("k", partial)
	if n != 2 {
		t.Fatalf("partial result was cached")
	}
	a, _, _ := c.Do("a", func() ([]Result, bool, error) { return []Result{{ID: "A"}}, false, nil })
	b, _, _ := c.Do("b", func() ([]Result, bool, error) { return []Result{{ID: "B"}}, false, nil })
	if a[0].ID == b[0].ID {
		t.Fatal("keys collided")
	}
}

func TestCache_boundedAndExpires(t *testing.T) {
	c := NewCache(20*time.Millisecond, 5)
	for i := 0; i < 50; i++ {
		k := string(rune('a'+i%26)) + string(rune('A'+i/26))
		_, _, _ = c.Do(k, func() ([]Result, bool, error) { return nil, false, nil })
	}
	if len(c.m) > 5 {
		t.Fatalf("size=%d", len(c.m))
	}
	time.Sleep(30 * time.Millisecond)
	calls := 0
	_, _, _ = c.Do("aA", func() ([]Result, bool, error) { calls++; return nil, false, nil })
	if calls != 1 {
		t.Fatalf("expired entry should recompute, calls=%d", calls)
	}
}
