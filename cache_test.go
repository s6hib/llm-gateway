package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/s6hib/llm-gateway/fake"
)

// numbered is an upstream whose every answer is different, so a test can tell
// a kept answer from a fresh one.
func numbered(calls *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-Id", fmt.Sprintf("req_%d", n))
		w.Header().Set("Anthropic-Organization-Id", "org_1")
		fmt.Fprintf(w, `{"id":"msg_%d","model":"claude-haiku-5-5","usage":{"input_tokens":10,"output_tokens":20}}`, n)
	})
}

func cached(seconds string) map[string]string {
	return map[string]string{cacheHeader: seconds}
}

func TestSecondIdenticalRequestComesFromTheCache(t *testing.T) {
	var calls atomic.Int32
	var seenHeader atomic.Value
	g, url := streamGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeader.Store(r.Header.Get(cacheHeader))
		numbered(&calls).ServeHTTP(w, r)
	}))

	first, firstBody := post(t, url+"/v1/messages", requestBody, cached("60"))
	second, secondBody := post(t, url+"/v1/messages", requestBody, cached("60"))

	if first.Header.Get(cacheResult) != "miss" || second.Header.Get(cacheResult) != "hit" {
		t.Errorf("X-Cache was %q then %q", first.Header.Get(cacheResult), second.Header.Get(cacheResult))
	}
	if calls.Load() != 1 || secondBody != firstBody || second.StatusCode != http.StatusOK {
		t.Errorf("%d upstream calls; first %s; second %d %s", calls.Load(), firstBody, second.StatusCode, secondBody)
	}
	if second.Header.Get("Content-Type") != "application/json" || second.Header.Get("Request-Id") != "req_1" {
		t.Errorf("the kept answer's headers: %v", second.Header)
	}
	if second.Header.Get("Anthropic-Organization-Id") != "" {
		t.Error("the kept answer carries the upstream account's header")
	}
	if got := seenHeader.Load(); got != "" {
		t.Errorf("the cache header reached the upstream: %q", got)
	}
	// The answer from the cache cost nothing, so one call is counted.
	if got := counted(t, g); got.Requests != 1 {
		t.Errorf("counted %+v", got)
	}
}

func TestNothingIsCachedUnlessAsked(t *testing.T) {
	var calls atomic.Int32
	_, url := streamGateway(t, numbered(&calls))

	post(t, url+"/v1/messages", requestBody, cached("60")) // one caller asks
	resp, _ := post(t, url+"/v1/messages", requestBody, nil)
	resp2, _ := post(t, url+"/v1/messages", requestBody, nil)

	if calls.Load() != 3 || resp.Header.Get(cacheResult) != "" || resp2.Header.Get(cacheResult) != "" {
		t.Errorf("%d upstream calls, X-Cache %q and %q", calls.Load(), resp.Header.Get(cacheResult), resp2.Header.Get(cacheResult))
	}
}

func TestAnythingThatChangesTheAnswerIsAMiss(t *testing.T) {
	var calls atomic.Int32
	_, url := streamGateway(t, numbered(&calls))
	with := func(extra map[string]string) map[string]string {
		headers := cached("60")
		for name, value := range extra {
			headers[name] = value
		}
		return headers
	}
	post(t, url+"/v1/messages", requestBody, cached("60"))

	for what, send := range map[string]func() *http.Response{
		"another caller": func() *http.Response {
			resp, _ := post(t, url+"/v1/messages", requestBody, with(map[string]string{"X-Api-Key": "test-gateway-key-0002"}))
			return resp
		},
		"another body": func() *http.Response {
			resp, _ := post(t, url+"/v1/messages", requestBody+" ", cached("60"))
			return resp
		},
		"another API version": func() *http.Response {
			resp, _ := post(t, url+"/v1/messages", requestBody, with(map[string]string{"Anthropic-Version": "2023-06-01"}))
			return resp
		},
		"a beta feature": func() *http.Response {
			resp, _ := post(t, url+"/v1/messages", requestBody, with(map[string]string{"Anthropic-Beta": "something-2026"}))
			return resp
		},
		"another content type": func() *http.Response {
			resp, _ := post(t, url+"/v1/messages", requestBody, with(map[string]string{"Content-Type": "text/plain"}))
			return resp
		},
		"another query string": func() *http.Response {
			resp, _ := post(t, url+"/v1/messages?beta=true", requestBody, cached("60"))
			return resp
		},
	} {
		if resp := send(); resp.Header.Get(cacheResult) != "miss" {
			t.Errorf("%s: X-Cache %q", what, resp.Header.Get(cacheResult))
		}
	}
	if calls.Load() != 7 {
		t.Errorf("%d upstream calls, want 7", calls.Load())
	}
	// The first request again, unchanged, is still a hit.
	if resp, _ := post(t, url+"/v1/messages", requestBody, cached("60")); resp.Header.Get(cacheResult) != "hit" {
		t.Errorf("the unchanged request: X-Cache %q", resp.Header.Get(cacheResult))
	}
}

func TestKeptAnswerExpires(t *testing.T) {
	var calls atomic.Int32
	clock := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	advance := func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }
	_, url := streamGateway(t, numbered(&calls), func(g *gateway) { g.cache.now = now })

	post(t, url+"/v1/messages", requestBody, cached("60"))
	advance(59 * time.Second)
	if resp, _ := post(t, url+"/v1/messages", requestBody, cached("60")); resp.Header.Get(cacheResult) != "hit" {
		t.Errorf("after 59s: X-Cache %q", resp.Header.Get(cacheResult))
	}
	advance(time.Second)
	if resp, _ := post(t, url+"/v1/messages", requestBody, cached("60")); resp.Header.Get(cacheResult) != "miss" {
		t.Errorf("after 60s: X-Cache %q", resp.Header.Get(cacheResult))
	}
	if calls.Load() != 2 {
		t.Errorf("%d upstream calls, want 2", calls.Load())
	}
}

func TestErrorsAndStreamsAreNotKept(t *testing.T) {
	var calls atomic.Int32
	_, url := streamGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"no"}}`)
			return
		}
		fake.Handler(0, 0).ServeHTTP(w, r)
	}))

	if resp, _ := post(t, url+"/v1/messages", requestBody, cached("60")); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("first call: status %d", resp.StatusCode)
	}
	if resp, _ := post(t, url+"/v1/messages", requestBody, cached("60")); resp.StatusCode != http.StatusOK || resp.Header.Get(cacheResult) != "miss" {
		t.Errorf("after an error: status %d, X-Cache %q", resp.StatusCode, resp.Header.Get(cacheResult))
	}

	post(t, url+"/v1/messages", streamBody, cached("60"))
	if resp, _ := post(t, url+"/v1/messages", streamBody, cached("60")); resp.Header.Get(cacheResult) != "miss" {
		t.Errorf("a streamed answer was kept: X-Cache %q", resp.Header.Get(cacheResult))
	}
	if calls.Load() != 4 {
		t.Errorf("%d upstream calls, want 4", calls.Load())
	}
}

func TestBadCacheHeaderIsRefused(t *testing.T) {
	var calls atomic.Int32
	_, url := streamGateway(t, numbered(&calls))

	for _, value := range []string{"0", "-5", "soon", "1.5", "3601", "60s", ""} {
		resp, body := post(t, url+"/v1/messages", requestBody, cached(value))
		if resp.StatusCode != http.StatusBadRequest || errorType(t, body) != "invalid_request_error" {
			t.Errorf("%s %q: status %d, body %s", cacheHeader, value, resp.StatusCode, body)
		}
	}
	// The header sent twice.
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/messages", strings.NewReader(requestBody))
	req.Header.Set("X-Api-Key", testKey)
	req.Header.Add(cacheHeader, "60")
	req.Header.Add(cacheHeader, "60")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("%s sent twice: status %d", cacheHeader, resp.StatusCode)
	}
	if calls.Load() != 0 {
		t.Errorf("%d refused requests reached the upstream", calls.Load())
	}
}

func TestCacheHitsStillCountAgainstTheRateLimit(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(numbered(&calls))
	defer up.Close()
	g := testGateway(t, up.URL, 2) // 2 requests a minute
	g.limiter.now = func() time.Time { return time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC) }
	gw := httptest.NewServer(g.handler())
	defer gw.Close()

	post(t, gw.URL+"/v1/messages", requestBody, cached("60"))
	post(t, gw.URL+"/v1/messages", requestBody, cached("60"))
	if resp, _ := post(t, gw.URL+"/v1/messages", requestBody, cached("60")); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("third request: status %d, want 429", resp.StatusCode)
	}
}

func TestCacheHitIsRefusedOverTheDailyCap(t *testing.T) {
	var calls atomic.Int32
	// 10 in and 20 out on haiku is 11 millionths, and the cap is 11.
	g, url := streamGateway(t, numbered(&calls), func(g *gateway) { g.dailyCap = 11 })

	post(t, url+"/v1/messages", requestBody, cached("60"))
	counted(t, g)
	if resp, _ := post(t, url+"/v1/messages", requestBody, cached("60")); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("a kept answer was handed to a caller over its cap: status %d", resp.StatusCode)
	}
}

func TestKeptAnswerGetsAFreshDate(t *testing.T) {
	var calls atomic.Int32
	_, url := streamGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", "Mon, 02 Jan 2006 15:04:05 GMT")
		numbered(&calls).ServeHTTP(w, r)
	}))

	post(t, url+"/v1/messages", requestBody, cached("60"))
	resp, _ := post(t, url+"/v1/messages", requestBody, cached("60"))

	if resp.Header.Get(cacheResult) != "hit" || resp.Header.Get("Date") == "Mon, 02 Jan 2006 15:04:05 GMT" {
		t.Errorf("X-Cache %q, Date %q", resp.Header.Get(cacheResult), resp.Header.Get("Date"))
	}
}

func TestCacheDropsExpiredThenSoonestToExpire(t *testing.T) {
	c := newCache()
	c.max = 3
	clock := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return clock }
	key := func(n byte) [32]byte { return [32]byte{n} }
	has := func(n byte) bool { _, ok := c.get(key(n)); return ok }

	c.put(key(1), nil, []byte("a"), 10*time.Second)
	c.put(key(2), nil, []byte("b"), 300*time.Second)
	c.put(key(3), nil, []byte("c"), 200*time.Second)

	// Full, nothing expired: the one closest to expiring (1) makes room.
	c.put(key(4), nil, []byte("d"), 100*time.Second)
	if has(1) || !has(2) || !has(3) || !has(4) {
		t.Errorf("after the 4th: %v %v %v %v", has(1), has(2), has(3), has(4))
	}

	// Full, and 4 has expired: only 4 is dropped.
	clock = clock.Add(150 * time.Second)
	c.put(key(5), nil, []byte("e"), 500*time.Second)
	if has(4) || !has(2) || !has(3) || !has(5) {
		t.Errorf("after the 5th: %v %v %v %v", has(4), has(2), has(3), has(5))
	}

	// Putting a key that is already there never pushes another one out.
	c.put(key(5), nil, []byte("e2"), 500*time.Second)
	if !has(2) || !has(3) || len(c.entries) != 3 {
		t.Errorf("after replacing: %d entries", len(c.entries))
	}

	c.put(key(6), nil, make([]byte, maxCacheEntry+1), time.Minute)
	if has(6) {
		t.Error("an answer over the size limit was kept")
	}
}

func TestCacheKeyPartsDoNotRunTogether(t *testing.T) {
	a, _ := http.NewRequest(http.MethodPost, "/v1/messages?x=1", nil)
	b, _ := http.NewRequest(http.MethodPost, "/v1/messages?x=", nil)
	// Without lengths, name "ab" + query "x=1" and name "a" + query ... could
	// be made to line up. Here the same letters are split differently.
	if cacheKey("tester", a, []byte("body")) == cacheKey("tester", b, []byte("1body")) {
		t.Error("two different requests share a key")
	}
	if cacheKey("ab", a, nil) == cacheKey("a", a, []byte("b")) {
		t.Error("the name and the body ran together")
	}
}

func TestCacheSurvivesManyRequestsAtOnce(t *testing.T) {
	var calls atomic.Int32
	_, url := streamGateway(t, numbered(&calls))

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := fmt.Sprintf(`{"model":"claude-haiku-5-5","messages":[],"n":%d}`, i%4)
			if resp, _ := post(t, url+"/v1/messages", body, cached("60")); resp.StatusCode != http.StatusOK {
				t.Errorf("status %d", resp.StatusCode)
			}
		}()
	}
	wg.Wait()

	// At least the 4 different bodies went upstream, and no more than all 40.
	if n := calls.Load(); n < 4 || n > 40 {
		t.Errorf("%d upstream calls", n)
	}
	for i := 0; i < 4; i++ {
		body := fmt.Sprintf(`{"model":"claude-haiku-5-5","messages":[],"n":%d}`, i)
		if resp, _ := post(t, url+"/v1/messages", body, cached("60")); resp.Header.Get(cacheResult) != "hit" {
			t.Errorf("body %d afterwards: X-Cache %q", i, resp.Header.Get(cacheResult))
		}
	}
}
