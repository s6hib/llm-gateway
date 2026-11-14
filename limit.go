package main

import (
	"math"
	"sync"
	"time"
)

// limiter is a token bucket per caller. A bucket holds up to perMinute
// tokens and refills at perMinute tokens a minute. Each request takes one
// token, and a caller with an empty bucket is turned away. So a caller can
// send a short burst, but over time no more than perMinute requests a minute.
//
// The buckets live in memory: they reset when the gateway restarts, and two
// gateways would each count separately.
type limiter struct {
	mu        sync.Mutex // requests run at the same time and share the map
	perMinute float64
	now       func() time.Time // swapped for a fake clock in tests
	buckets   map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time // when tokens was last brought up to date
}

func newLimiter(perMinute int) *limiter {
	return &limiter{perMinute: float64(perMinute), now: time.Now, buckets: map[string]*bucket{}}
}

// allow takes one token from the caller's bucket. When the bucket is empty it
// returns false and how long until the next token arrives.
func (l *limiter) allow(name string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b := l.buckets[name]
	if b == nil {
		b = &bucket{tokens: l.perMinute, last: now}
		l.buckets[name] = b
	}
	// Add the tokens earned since the last request, up to a full bucket.
	b.tokens = math.Min(l.perMinute, b.tokens+now.Sub(b.last).Minutes()*l.perMinute)
	b.last = now

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	missing := 1 - b.tokens
	return false, time.Duration(missing / l.perMinute * float64(time.Minute))
}
