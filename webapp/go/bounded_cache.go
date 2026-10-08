package main

import "sync"

// Shards discard their entries when full. Immutable values survive eviction
// while a reader uses them; retained key/value payloads have an explicit budget.
type boundedCache[K comparable, V any] struct {
	mu      sync.Mutex
	entries map[K]V
	bytes   int
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

type rowJSONKey struct {
	comment      LivecommentModel
	reaction     ReactionModel
	user, stream *referenceJSON
}
