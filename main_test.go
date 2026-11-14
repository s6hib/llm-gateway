package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/s6hib/llm-gateway/fake"
)

const requestBody = `{"model":"claude-haiku-5-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

// testKey is the gateway key the tests call with. post sends it unless the
// test sets its own X-Api-Key header.
const testKey = "test-gateway-key-0001"

func testGateway(t *testing.T, upstreamURL string, perMinute int) *gateway {
	t.Helper()
	keys, err := parseKeys("tester:" + testKey + ",other:test-gateway-key-0002")
	if err != nil {
		t.Fatal(err)
	}
	spend, err := openSpend(filepath.Join(t.TempDir(), "spend.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &gateway{
		spend: spend, dailyCap: 1000_000_000, // $1,000, out of the way unless a test lowers it
		upstream: upstreamURL, apiKey: "upstream-key", client: newClient(time.Minute),
		keys: keys, limiter: newLimiter(perMinute), maxRetries: 2,
		writeTimeout: 30 * time.Second, drainLimit: time.Minute, cache: newCache(),
		// Tests do not wait between tries.
		sleep: func(context.Context, time.Duration) error { return nil },
	}
}

// start runs a gateway in front of the given upstream and returns the
// gateway's address.
func start(t *testing.T, upstream http.Handler) string {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	gw := httptest.NewServer(testGateway(t, up.URL, 1000).handler())
	t.Cleanup(gw.Close)
	return gw.URL
}

func post(t *testing.T, url, body string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", testKey)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	text, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(text)
}

func errorType(t *testing.T, body string) string {
	t.Helper()
	var parsed struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil || parsed.Type != "error" {
		t.Fatalf("not an error body: %q", body)
	}
	return parsed.Error.Type
}

func TestForwardsRequestAndAnswer(t *testing.T) {
	var seen *http.Request
	var seenBody string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		seen, seenBody = r, string(raw)
		r.Body = io.NopCloser(strings.NewReader(seenBody))
		fake.Handler(0, 0).ServeHTTP(w, r)
	})
	url := start(t, upstream)

	resp, body := post(t, url+"/v1/messages?beta=true", requestBody, map[string]string{
		"X-Api-Key":              testKey,
		"Authorization":          "Bearer " + testKey,
		"Anthropic-Version":      "2026-10-21",
		"Content-Type":           "application/json",
		"Connection":             "X-Hop",
		"X-Hop":                  "stays on this hop",
		"Cookie":                 "session=caller",
		"Anthropic-Workspace-Id": "wrkspc_callers_pick",
		"Expect":                 "100-continue",
	})
	for _, name := range []string{"Cookie", "Anthropic-Workspace-Id", "Expect"} {
		if got := seen.Header.Get(name); got != "" {
			t.Errorf("the caller's %s header reached the upstream: %q", name, got)
		}
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body %s", resp.StatusCode, body)
	}
	var message struct {
		Type  string `json:"type"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(body), &message); err != nil || message.Type != "message" || message.Model != "claude-haiku-5-5" {
		t.Errorf("unexpected answer: %s", body)
	}
	if got := resp.Header.Get("Request-Id"); got != "req_fake" {
		t.Errorf("upstream's Request-Id header did not reach the caller: %q", got)
	}

	if seenBody != requestBody {
		t.Errorf("upstream got body %q", seenBody)
	}
	if got := seen.Header.Get("Authorization"); got != "Bearer upstream-key" {
		t.Errorf("upstream got Authorization %q", got)
	}
	if got := seen.Header.Get("X-Api-Key"); got != "" {
		t.Errorf("the caller's key reached the upstream: %q", got)
	}
	if got := seen.Header.Get("Anthropic-Version"); got != "2026-10-21" {
		t.Errorf("upstream got Anthropic-Version %q", got)
	}
	if got := seen.Header.Get("X-Hop"); got != "" {
		t.Errorf("a hop-by-hop header reached the upstream: %q", got)
	}
	if seen.URL.RawQuery != "beta=true" {
		t.Errorf("upstream got query %q", seen.URL.RawQuery)
	}
}

func TestPassesUpstreamErrorThrough(t *testing.T) {
	const rateLimited = `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"},"request_id":"req_1"}`
	url := start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, rateLimited)
	}))

	resp, body := post(t, url+"/v1/messages", requestBody, nil)

	if resp.StatusCode != http.StatusTooManyRequests || body != rateLimited {
		t.Errorf("status %d, body %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Retry-After"); got != "7" {
		t.Errorf("Retry-After %q", got)
	}
}

func TestHidesAccountHeadersFromCaller(t *testing.T) {
	url := start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Organization-Id", "org_1")
		w.Header().Set("Anthropic-Workspace-Id", "wrkspc_1")
		w.Header().Set("Anthropic-Ratelimit-Requests-Remaining", "49")
		w.Header().Set("Set-Cookie", "a=b")
		w.Header().Set("Request-Id", "req_1")
		io.WriteString(w, `{"model":"claude-haiku-5-5","usage":{"output_tokens":1}}`)
	}))

	resp, _ := post(t, url+"/v1/messages", requestBody, nil)

	for _, name := range []string{"Anthropic-Organization-Id", "Anthropic-Workspace-Id", "Anthropic-Ratelimit-Requests-Remaining", "Set-Cookie"} {
		if got := resp.Header.Get(name); got != "" {
			t.Errorf("the upstream's %s header reached the caller: %q", name, got)
		}
	}
	if got := resp.Header.Get("Request-Id"); got != "req_1" {
		t.Errorf("Request-Id %q", got)
	}
}

func TestUpstreamDown(t *testing.T) {
	up := httptest.NewServer(http.NotFoundHandler())
	up.Close() // nothing is listening at up.URL any more
	gw := httptest.NewServer(testGateway(t, up.URL, 1000).handler())
	defer gw.Close()

	resp, body := post(t, gw.URL+"/v1/messages", requestBody, nil)

	if resp.StatusCode != http.StatusBadGateway || errorType(t, body) != "api_error" {
		t.Errorf("status %d, body %s", resp.StatusCode, body)
	}
}

func TestRejectsOversizedBody(t *testing.T) {
	called := false
	url := start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))

	resp, body := post(t, url+"/v1/messages", strings.Repeat("x", maxBody+1), nil)

	if resp.StatusCode != http.StatusRequestEntityTooLarge || errorType(t, body) != "request_too_large" {
		t.Errorf("status %d, body %s", resp.StatusCode, body)
	}
	if called {
		t.Error("the oversized request was sent upstream")
	}
}

func TestOtherPathsAreNotFound(t *testing.T) {
	url := start(t, fake.Handler(0, 0))

	for _, try := range []struct{ method, path string }{
		{http.MethodPost, "/v1/models"},
		{http.MethodGet, "/v1/messages"},
	} {
		req, _ := http.NewRequest(try.method, url+try.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound || errorType(t, string(raw)) != "not_found_error" {
			t.Errorf("%s %s: status %d, body %s", try.method, try.path, resp.StatusCode, raw)
		}
	}
}
