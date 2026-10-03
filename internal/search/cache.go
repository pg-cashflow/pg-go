package search

import (
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Cache is a small in-process TTL cache with request coalescing. It absorbs
// typeahead bursts and repeated identical queries. Results are cached only when
// complete (not partial) and error-free. Keys must contain every RBAC input
// (role, property, tenant, limit, types, query) so entries are never shared
// across scopes.
type Cache struct {
	mu  sync.Mutex
	m   map[string]cacheEntry
	ttl time.Duration
	max int
	sf  singleflight.Group
}

type cacheEntry struct {
	res []Result
	exp time.Time
}

// NewCache returns a cache holding at most max entries for ttl each.
func NewCache(ttl time.Duration, max int) *Cache {
	if max <= 0 {
		max = 1000
	}
	return &Cache{m: make(map[string]cacheEntry, max), ttl: ttl, max: max}
}

// Do returns the cached value for key, or runs fn once for all concurrent callers.
func (c *Cache) Do(key string, fn func() ([]Result, bool, error)) ([]Result, bool, error) {
	now := time.Now()
	c.mu.Lock()
	if e, ok := c.m[key]; ok && now.Before(e.exp) {
		c.mu.Unlock()
		return e.res, false, nil
	}
	c.mu.Unlock()

	type out struct {
		res     []Result
		partial bool
	}
	v, err, _ := c.sf.Do(key, func() (any, error) {
		res, partial, err := fn()
		if err != nil {
			return nil, err
		}
		if !partial {
			c.put(key, res)
		}
		return out{res, partial}, nil
	})
	if err != nil {
		return nil, false, err
	}
	o := v.(out)
	return o.res, o.partial, nil
}

func (c *Cache) put(key string, res []Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if len(c.m) >= c.max {
		for k, e := range c.m { // drop expired first
			if now.After(e.exp) {
				delete(c.m, k)
			}
		}
		for k := range c.m { // still full: evict arbitrary entries down to 90%
			if len(c.m) < c.max*9/10 {
				break
			}
			delete(c.m, k)
		}
	}
	c.m[key] = cacheEntry{res: res, exp: now.Add(c.ttl)}
}
