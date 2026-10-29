// SPDX-License-Identifier: MIT

package ratelimit

import (
	"sync"
	"time"
)

type bucket struct {
	tokens  float64
	updated time.Time
}

// Limiter keeps a token bucket per key. The zero value is ready for use.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]bucket
	swept   time.Time
}

func (l *Limiter) Allow(key string, capacity int, period time.Duration, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil {
		l.buckets = make(map[string]bucket)
	}
	if now.Sub(l.swept) >= period {
		for k, b := range l.buckets {
			if now.Sub(b.updated) >= period {
				delete(l.buckets, k)
			}
		}
		l.swept = now
	}
	b, ok := l.buckets[key]
	if !ok {
		b = bucket{tokens: float64(capacity), updated: now}
	}
	b.tokens = min(float64(capacity), b.tokens+max(0, now.Sub(b.updated).Seconds())*float64(capacity)/period.Seconds())
	b.updated = now
	allowed := b.tokens >= 1
	if allowed {
		b.tokens--
	}
	l.buckets[key] = b
	return allowed
}
