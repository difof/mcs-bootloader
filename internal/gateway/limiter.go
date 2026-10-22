package gateway

import (
	"sync"
	"time"
)

type bucket struct {
	tokens  float64
	updated time.Time
}

// wakeLimiter is deliberately small and bounded. Repeated clicks still receive
// the boot message, but cannot create unbounded controller work or IP records.
type wakeLimiter struct {
	mu     sync.Mutex
	global bucket
	byIP   map[string]bucket
}

func (l *wakeLimiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byIP == nil {
		l.byIP = make(map[string]bucket)
	}
	global := refill(l.global, now, 8, 2*time.Second)
	ipBucket, found := l.byIP[ip]
	if !found && len(l.byIP) >= 1024 {
		// No growth beyond the cap. Evict one old entry, rather than permanently
		// banning new addresses after a scan exhausts the table.
		var oldestIP string
		var oldest time.Time
		for key, value := range l.byIP {
			if oldestIP == "" || value.updated.Before(oldest) {
				oldestIP, oldest = key, value.updated
			}
		}
		delete(l.byIP, oldestIP)
	}
	ipBucket = refill(ipBucket, now, 3, 5*time.Second)
	allowed := global.tokens >= 1 && ipBucket.tokens >= 1
	if allowed {
		global.tokens--
		ipBucket.tokens--
	}
	l.global, l.byIP[ip] = global, ipBucket
	return allowed
}

func refill(b bucket, now time.Time, capacity float64, interval time.Duration) bucket {
	if b.updated.IsZero() {
		return bucket{tokens: capacity, updated: now}
	}
	elapsed := now.Sub(b.updated)
	if elapsed > 0 {
		b.tokens += float64(elapsed) / float64(interval)
		if b.tokens > capacity {
			b.tokens = capacity
		}
		b.updated = now
	}
	return b
}
