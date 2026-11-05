package state

import (
	"sync"
	"time"
)

// Dedup atomically reserves bounded message IDs for an explicit retention interval.
type Dedup struct {
	mu      sync.Mutex
	items   map[string]time.Time
	ttl     time.Duration
	maximum int
}

// NewDedup constructs an empty deduplication store.
func NewDedup(ttl time.Duration, maximum int) *Dedup {
	return &Dedup{items: make(map[string]time.Time), ttl: ttl, maximum: maximum}
}

// Claim returns false for an existing live identifier and an error when capacity is full.
func (d *Dedup) Claim(id string, now time.Time) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for key, expiry := range d.items {
		if !expiry.After(now) {
			delete(d.items, key)
		}
	}
	if _, ok := d.items[id]; ok {
		return false, nil
	}
	if len(d.items) >= d.maximum {
		return false, ErrCapacity
	}
	d.items[id] = now.Add(d.ttl)
	return true, nil
}

// Release allows retry only when a delivery could not enter background processing.
func (d *Dedup) Release(id string) { d.mu.Lock(); defer d.mu.Unlock(); delete(d.items, id) }
