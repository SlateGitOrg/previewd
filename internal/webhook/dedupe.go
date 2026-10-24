package webhook

import (
	"container/list"
	"sync"
	"time"
)

// DeliveryCache remembers recently processed delivery IDs so redeliveries
// and replays of the same delivery are answered without touching the API
// server. It is bounded both in size (LRU eviction) and in time (entries
// older than ttl are forgotten).
//
// The cache is an optimisation, not the correctness mechanism: the intake
// is idempotent and orders events by updated_at, so a duplicate that slips
// through (after a restart, or on another replica) is harmless.
type DeliveryCache struct {
	mu    sync.Mutex
	cap   int
	ttl   time.Duration
	now   func() time.Time
	order *list.List // front = most recent
	items map[string]*list.Element
}

type deliveryEntry struct {
	id   string
	seen time.Time
}

// NewDeliveryCache returns a cache holding at most capacity IDs for ttl.
func NewDeliveryCache(capacity int, ttl time.Duration, now func() time.Time) *DeliveryCache {
	if capacity < 1 {
		capacity = 1
	}
	if now == nil {
		now = time.Now
	}
	return &DeliveryCache{cap: capacity, ttl: ttl, now: now, order: list.New(), items: map[string]*list.Element{}}
}

// Seen reports whether id was recorded within the TTL.
func (c *DeliveryCache) Seen(id string) bool {
	if id == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[id]
	if !ok {
		return false
	}
	if c.now().Sub(el.Value.(*deliveryEntry).seen) > c.ttl {
		c.order.Remove(el)
		delete(c.items, id)
		return false
	}
	return true
}

// Record marks id as processed. Callers record only after successful
// processing, so a delivery that failed can be redelivered and retried.
func (c *DeliveryCache) Record(id string) {
	if id == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[id]; ok {
		el.Value.(*deliveryEntry).seen = c.now()
		c.order.MoveToFront(el)
		return
	}
	c.items[id] = c.order.PushFront(&deliveryEntry{id: id, seen: c.now()})
	for c.order.Len() > c.cap {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*deliveryEntry).id)
	}
}

// Len returns the number of remembered deliveries.
func (c *DeliveryCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
