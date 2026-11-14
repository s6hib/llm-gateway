package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	cacheHeader   = "X-Cache-Ttl" // a request sets this, in seconds, to ask for caching
	maxCacheTTL   = time.Hour     // the longest an answer may be kept
	maxCacheEntry = 1 << 20       // answers over 1 MB are not kept
	maxCacheSize  = 1000          // answers kept at once
	cacheResult   = "X-Cache"     // the answer says "hit" or "miss" here
)

// cache keeps answers in memory so that an identical request can be answered
// without calling the upstream. It is off unless a request asks for it: a
// model can answer the same prompt differently each time, and a caller that
// sends one prompt several times to compare the answers must get fresh ones.
//
// The cache is emptied by a restart and is not shared between machines.
type cache struct {
	mu      sync.Mutex
	now     func() time.Time // swapped for a fake clock in tests
	max     int
	entries map[[32]byte]cacheEntry
}

type cacheEntry struct {
	header  http.Header
	body    []byte
	expires time.Time
}

func newCache() *cache {
	return &cache{now: time.Now, max: maxCacheSize, entries: map[[32]byte]cacheEntry{}}
}

// cacheTTL reads how long the request wants its answer kept. wanted is false
// when the request did not ask for caching.
func cacheTTL(r *http.Request) (ttl time.Duration, wanted bool, err error) {
	values := r.Header.Values(cacheHeader)
	if len(values) == 0 {
		return 0, false, nil
	}
	seconds, err := strconv.Atoi(values[0])
	// A header that is there but empty, or sent twice, is refused too, so
	// that a mistake is never read as "no caching wanted".
	if err != nil || len(values) > 1 || seconds < 1 || seconds > int(maxCacheTTL/time.Second) {
		return 0, false, errors.New(cacheHeader + " must be a whole number of seconds from 1 to 3600")
	}
	return time.Duration(seconds) * time.Second, true, nil
}

// cacheKey is a hash of everything that decides the answer: who is asking,
// the API version and beta features asked for, the body's content type, the
// query string and the body.
// The caller's name is part of it so that one caller is never handed an
// answer that was made for another.
func cacheKey(name string, r *http.Request, body []byte) [32]byte {
	hash := sha256.New()
	for _, part := range []string{
		name,
		r.URL.RawQuery,
		strings.Join(r.Header.Values("Anthropic-Version"), ","),
		strings.Join(r.Header.Values("Anthropic-Beta"), ","),
		r.Header.Get("Content-Type"),
		string(body),
	} {
		// Each part goes in with its length in front, so that two different
		// sets of parts can never run together into the same bytes.
		fmt.Fprintf(hash, "%d:%s", len(part), part)
	}
	return [32]byte(hash.Sum(nil))
}

// get returns the kept answer for key, if there is one that has not expired.
func (c *cache) get(key [32]byte) (cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return cacheEntry{}, false
	}
	if !c.now().Before(entry.expires) {
		delete(c.entries, key)
		return cacheEntry{}, false
	}
	return entry, true
}

// put keeps an answer for ttl. When the cache is full it first drops what has
// expired and then, if that was not enough, the answer closest to expiring.
func (c *cache) put(key [32]byte, header http.Header, body []byte, ttl time.Duration) {
	if len(body) > maxCacheEntry {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if _, replacing := c.entries[key]; !replacing && len(c.entries) >= c.max {
		var soonest [32]byte
		var soonestExpiry time.Time
		for k, entry := range c.entries {
			if !now.Before(entry.expires) {
				delete(c.entries, k)
			} else if soonestExpiry.IsZero() || entry.expires.Before(soonestExpiry) {
				soonest, soonestExpiry = k, entry.expires
			}
		}
		if len(c.entries) >= c.max {
			delete(c.entries, soonest)
		}
	}
	c.entries[key] = cacheEntry{header: header, body: body, expires: now.Add(ttl)}
}
