package main

import (
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/s6hib/llm-gateway/fake"
)

func TestParseKeys(t *testing.T) {
	keys, err := parseKeys("fineprint:aaaaaaaaaaaaaaaa, notebook : bbbbbbbbbbbbbbbb")
	if err != nil || len(keys) != 2 {
		t.Fatalf("keys %v, err %v", keys, err)
	}
	// Spaces around a name or key in the setting are not part of it.
	if name := keys[sha256.Sum256([]byte("bbbbbbbbbbbbbbbb"))]; name != "notebook" {
		t.Errorf("the key with spaces around it belongs to %q", name)
	}
	for _, bad := range []string{
		"",                                      // nothing set
		"fineprint",                             // no key
		":aaaaaaaaaaaaaaaa",                     // no name
		"fineprint:short",                       // key too short
		"a:aaaaaaaaaaaaaaaa,a:bbbbbbbbbbbbbbbb", // same name twice
		"a:aaaaaaaaaaaaaaaa,b:aaaaaaaaaaaaaaaa", // same key twice
		"a:aaaaaaaaaaaaaaaa,",                   // trailing comma
	} {
		if _, err := parseKeys(bad); err == nil {
			t.Errorf("parseKeys(%q) should fail", bad)
		}
	}
}

func TestRejectsMissingOrUnknownKey(t *testing.T) {
	var calls atomic.Int32
	url := start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))

	for _, headers := range []map[string]string{
		{"X-Api-Key": ""},
		{"X-Api-Key": "not-a-gateway-key-0000"},
		{"X-Api-Key": "", "Authorization": "Bearer not-a-gateway-key-0000"},
		{"X-Api-Key": "", "Authorization": testKey}, // no "Bearer " in front
		{"X-Api-Key": "upstream-key"},               // the upstream key is not a gateway key
	} {
		resp, body := post(t, url+"/v1/messages", requestBody, headers)
		if resp.StatusCode != http.StatusUnauthorized || errorType(t, body) != "authentication_error" {
			t.Errorf("%v: status %d, body %s", headers, resp.StatusCode, body)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("%d unauthenticated requests reached the upstream", calls.Load())
	}
}

func TestAcceptsKeyInEitherHeader(t *testing.T) {
	url := start(t, fake.Handler(0, 0))

	for _, headers := range []map[string]string{
		{"X-Api-Key": testKey},
		{"X-Api-Key": "", "Authorization": "Bearer " + testKey},
		{"X-Api-Key": "", "Authorization": "bearer " + testKey},
	} {
		resp, body := post(t, url+"/v1/messages", requestBody, headers)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%v: status %d, body %s", headers, resp.StatusCode, body)
		}
	}
}

func TestRateLimitIsPerKey(t *testing.T) {
	up := httptest.NewServer(fake.Handler(0, 0))
	defer up.Close()
	g := testGateway(t, up.URL, 2) // 2 requests a minute
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	g.limiter.now = func() time.Time { return clock }
	gw := httptest.NewServer(g.handler())
	defer gw.Close()
	send := func(key string) *http.Response {
		resp, _ := post(t, gw.URL+"/v1/messages", requestBody, map[string]string{"X-Api-Key": key})
		return resp
	}

	for i := 0; i < 2; i++ {
		if resp := send(testKey); resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status %d", i+1, resp.StatusCode)
		}
	}
	resp, body := post(t, gw.URL+"/v1/messages", requestBody, nil)
	if resp.StatusCode != http.StatusTooManyRequests || errorType(t, body) != "rate_limit_error" {
		t.Fatalf("third request: status %d, body %s", resp.StatusCode, body)
	}
	// At 2 a minute the next token is 30 seconds away.
	if got := resp.Header.Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After %q", got)
	}
	if resp := send("test-gateway-key-0002"); resp.StatusCode != http.StatusOK {
		t.Errorf("another key was limited too: status %d", resp.StatusCode)
	}

	clock = clock.Add(29 * time.Second)
	if resp := send(testKey); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("after 29s: status %d", resp.StatusCode)
	}
	clock = clock.Add(2 * time.Second)
	if resp := send(testKey); resp.StatusCode != http.StatusOK {
		t.Errorf("after 31s: status %d", resp.StatusCode)
	}
}

func TestRetryAfterIsNeverZero(t *testing.T) {
	up := httptest.NewServer(fake.Handler(0, 0))
	defer up.Close()
	g := testGateway(t, up.URL, 1)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	g.limiter.now = func() time.Time { return clock }
	gw := httptest.NewServer(g.handler())
	defer gw.Close()

	post(t, gw.URL+"/v1/messages", requestBody, nil) // takes the only token
	clock = clock.Add(time.Minute - time.Nanosecond) // the next token is a nanosecond away
	resp, _ := post(t, gw.URL+"/v1/messages", requestBody, nil)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "1" {
		t.Errorf("status %d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

func TestLimiterBucketNeverOverfills(t *testing.T) {
	l := newLimiter(3)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return clock }
	l.allow("a")
	clock = clock.Add(time.Hour) // a long quiet spell earns no more than a full bucket
	allowed := 0
	for i := 0; i < 10; i++ {
		if ok, _ := l.allow("a"); ok {
			allowed++
		}
	}
	if allowed != 3 {
		t.Errorf("allowed %d requests from a bucket of 3", allowed)
	}
}

func TestLimiterCountsRequestsThatArriveTogether(t *testing.T) {
	l := newLimiter(10)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return clock }
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := l.allow("a"); ok {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != 10 {
		t.Errorf("allowed %d of 100 requests with a limit of 10", allowed.Load())
	}
}
