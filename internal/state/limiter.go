package state

import (
	"lark-feishu-chatbot/internal/config"
	"sync"
	"time"
)

// attempts keeps at most the configured rolling-minute timestamps and one daily count.
type attempts struct {
	day    string
	count  int
	minute []time.Time
	last   time.Time
}

// Limiter enforces atomic UTC-day and rolling-minute attempt quotas.
type Limiter struct {
	mu    sync.Mutex
	items map[string]*attempts
	cfg   config.Rate
}

// NewLimiter uses zero quotas as explicitly disabled constraints.
func NewLimiter(cfg config.Rate) *Limiter {
	return &Limiter{items: make(map[string]*attempts), cfg: cfg}
}

// Allow reserves one attempt only when both quotas and user capacity permit it.
func (l *Limiter) Allow(user string, now time.Time) bool {
	if !l.cfg.Enabled || (l.cfg.PerMinute == 0 && l.cfg.PerDay == 0) {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	day := now.UTC().Format("2006-01-02")
	for key, item := range l.items {
		if (l.cfg.PerDay == 0 || item.day != day) && now.Sub(item.last) >= time.Minute {
			delete(l.items, key)
		}
	}
	item := l.items[user]
	if item == nil {
		if len(l.items) >= l.cfg.MaxUsers {
			return false
		}
		item = &attempts{day: day}
		l.items[user] = item
	}
	if item.day != day {
		item.day = day
		item.count = 0
	}
	first := 0
	for first < len(item.minute) && !item.minute[first].After(now.Add(-time.Minute)) {
		first++
	}
	item.minute = append([]time.Time(nil), item.minute[first:]...)
	if (l.cfg.PerDay > 0 && item.count >= l.cfg.PerDay) ||
		(l.cfg.PerMinute > 0 && len(item.minute) >= l.cfg.PerMinute) {
		return false
	}
	item.count++
	item.last = now
	if l.cfg.PerMinute > 0 {
		item.minute = append(item.minute, now)
	}
	return true
}
