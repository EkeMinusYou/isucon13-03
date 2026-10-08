package main

import (
	"sync"
	"sync/atomic"
)

// Shards discard their entries when full. Immutable values survive eviction
// while a reader uses them; retained key/value payloads have an explicit budget.
type boundedCache[K comparable, V any] struct {
	mu           sync.Mutex
	entries      map[K]V
	bytes        int
	hits, misses atomic.Uint64
}

func (c *boundedCache[K, V]) get(key K) (V, bool) {
	return c.getValid(key, nil)
}

func (c *boundedCache[K, V]) getValid(key K, valid func(V) bool) (V, bool) {
	c.mu.Lock()
	value, ok := c.entries[key]
	c.mu.Unlock()
	if ok && valid != nil {
		ok = valid(value)
	}
	if ok {
		c.hits.Add(1)
	} else {
		c.misses.Add(1)
	}
	return value, ok
}

func (c *boundedCache[K, V]) put(key K, value V, cost, budget int) {
	if cost > budget {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; exists {
		return
	}
	if c.entries == nil || c.bytes+cost > budget {
		c.entries = make(map[K]V)
		c.bytes = 0
	}
	c.entries[key] = value
	c.bytes += cost
}

func (c *boundedCache[K, V]) stats() (uint64, uint64, int) {
	c.mu.Lock()
	bytes := c.bytes
	c.mu.Unlock()
	return c.hits.Load(), c.misses.Load(), bytes
}

// Replacing a stale value also releases its accounted payload. This keeps
// mutable dependencies from accumulating versions under a stable request key.
func (c *boundedCache[K, V]) replace(key K, value V, cost, budget int, retainedCost func(V) int) {
	if cost > budget {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if previous, ok := c.entries[key]; ok {
		c.bytes -= retainedCost(previous)
	}
	if c.entries == nil || c.bytes+cost > budget {
		c.entries = make(map[K]V)
		c.bytes = 0
	}
	c.entries[key] = value
	c.bytes += cost
}

func addCacheMetrics[K comparable, V any](metrics map[string]any, prefix string, shards *[16]boundedCache[K, V]) {
	var hits, misses uint64
	var bytes int
	for i := range shards {
		h, m, b := shards[i].stats()
		hits += h
		misses += m
		bytes += b
	}
	metrics[prefix+"_hits_total"] = hits
	metrics[prefix+"_misses_total"] = misses
	metrics[prefix+"_budget_bytes"] = bytes
}

type rowJSONKey struct {
	comment      LivecommentModel
	reaction     ReactionModel
	user, stream *referenceJSON
}
