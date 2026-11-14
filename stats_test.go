package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/s6hib/llm-gateway/fake"
)

const adminKey = "test-admin-key-0001"

func withAdmin(g *gateway) {
	g.adminKey, g.hasAdmin = sha256.Sum256([]byte(adminKey)), true
}

func get(t *testing.T, url string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func readStats(t *testing.T, url string) map[string]callerReport {
	t.Helper()
	resp, body := get(t, url+"/stats", map[string]string{"X-Api-Key": adminKey})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /stats: status %d, body %s", resp.StatusCode, body)
	}
	var report struct {
		Callers map[string]callerReport `json:"callers"`
	}
	if err := json.Unmarshal([]byte(body), &report); err != nil {
		t.Fatalf("%v in %s", err, body)
	}
	return report.Callers
}

func TestStatsCountWhatHappened(t *testing.T) {
	var calls atomic.Int32
	_, url := streamGateway(t, numbered(&calls), withAdmin)

	post(t, url+"/v1/messages", requestBody, nil)                                // 200
	post(t, url+"/v1/messages", requestBody, cached("60"))                       // 200, a miss
	post(t, url+"/v1/messages", requestBody, cached("60"))                       // 200, a hit
	post(t, url+"/v1/messages", requestBody, cached("soon"))                     // 400
	post(t, url+"/v1/messages", requestBody, map[string]string{"X-Api-Key": ""}) // 401
	get(t, url+"/healthz", nil)                                                  // not counted

	callers := readStats(t, url)
	tester := callers["tester"]
	if tester.Requests != 4 || tester.ByStatus["200"] != 3 || tester.ByStatus["400"] != 1 || tester.CacheHits != 1 {
		t.Errorf("tester: %+v", tester)
	}
	if tester.LatencyMs.Count != 4 || tester.OverheadMs.Count != 4 {
		t.Errorf("latency covers %d requests, overhead %d", tester.LatencyMs.Count, tester.OverheadMs.Count)
	}
	// Two calls reached the upstream, each 10 in and 20 out on haiku: 11 millionths.
	if tester.Today != (todayReport{Requests: 2, InputTokens: 20, OutputTokens: 40, CostUSD: 0.000022}) {
		t.Errorf("today: %+v", tester.Today)
	}
	if noKey := callers["(no key)"]; noKey.Requests != 1 || noKey.ByStatus["401"] != 1 {
		t.Errorf("(no key): %+v", noKey)
	}
	if len(callers) != 2 {
		t.Errorf("callers: %v", callers)
	}
}

func TestStatsSeparateGatewayTimeFromUpstreamTime(t *testing.T) {
	_, url := streamGateway(t, fake.Handler(150*time.Millisecond, 0), withAdmin)

	for i := 0; i < 3; i++ {
		post(t, url+"/v1/messages", requestBody, nil)
	}

	tester := readStats(t, url)["tester"]
	// Each request waits 150 ms on the upstream. That wait is in the total
	// and must not be in the gateway's own share.
	if tester.LatencyMs.P50 < 150 || tester.OverheadMs.P50 >= 75 {
		t.Errorf("latency %+v, overhead %+v", tester.LatencyMs, tester.OverheadMs)
	}
}

func TestStatsCountRetries(t *testing.T) {
	up := &flaky{failures: 2, status: 529}
	_, url := streamGateway(t, up, withAdmin)

	post(t, url+"/v1/messages", requestBody, nil) // fails twice, then works
	post(t, url+"/v1/messages", requestBody, nil) // works first time

	if tester := readStats(t, url)["tester"]; tester.Retries != 2 || tester.ByStatus["200"] != 2 {
		t.Errorf("tester: %+v", tester)
	}
}

func TestStatsNeedTheAdminKey(t *testing.T) {
	_, noAdmin := streamGateway(t, fake.Handler(0, 0))
	if resp, _ := get(t, noAdmin+"/stats", map[string]string{"X-Api-Key": adminKey}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("with no admin key set: status %d, want 404", resp.StatusCode)
	}

	_, url := streamGateway(t, fake.Handler(0, 0), withAdmin)
	for what, headers := range map[string]map[string]string{
		"no key":         nil,
		"a wrong key":    {"X-Api-Key": "not-the-admin-key-00"},
		"a caller's key": {"X-Api-Key": testKey},
		"an empty key":   {"Authorization": "Bearer "},
	} {
		resp, body := get(t, url+"/stats", headers)
		if resp.StatusCode != http.StatusUnauthorized || strings.Contains(body, "callers") {
			t.Errorf("%s: status %d, body %s", what, resp.StatusCode, body)
		}
	}
	for _, headers := range []map[string]string{
		{"X-Api-Key": adminKey},
		{"Authorization": "Bearer " + adminKey},
	} {
		if resp, _ := get(t, url+"/stats", headers); resp.StatusCode != http.StatusOK {
			t.Errorf("%v: status %d", headers, resp.StatusCode)
		}
	}
	// The admin key is not a caller's key.
	if resp, _ := post(t, url+"/v1/messages", requestBody, map[string]string{"X-Api-Key": adminKey}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the admin key was accepted for a model call: status %d", resp.StatusCode)
	}
}

func TestHealthNeedsNoKey(t *testing.T) {
	_, url := streamGateway(t, fake.Handler(0, 0))
	if resp, body := get(t, url+"/healthz", nil); resp.StatusCode != http.StatusOK || !strings.Contains(body, "ok") {
		t.Errorf("status %d, body %s", resp.StatusCode, body)
	}
}

func TestLandingPageIsOnlyAtTheBareAddress(t *testing.T) {
	_, url := streamGateway(t, fake.Handler(0, 0))

	resp, body := get(t, url+"/", nil)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") ||
		!strings.Contains(body, "https://github.com/s6hib/llm-gateway") {
		t.Errorf("GET /: status %d, type %q, body %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	// Every other path still gets the API's not-found error, and the page
	// does not answer a POST.
	if resp, body := get(t, url+"/anything", nil); resp.StatusCode != http.StatusNotFound || errorType(t, body) != "not_found_error" {
		t.Errorf("GET /anything: status %d, body %s", resp.StatusCode, body)
	}
	if resp, body := post(t, url+"/", requestBody, nil); resp.StatusCode != http.StatusNotFound || errorType(t, body) != "not_found_error" {
		t.Errorf("POST /: status %d, body %s", resp.StatusCode, body)
	}
}

func TestCallerLeavingBeforeAnAnswerIsRecordedAs499(t *testing.T) {
	g, url := streamGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done() // never answers
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/messages", strings.NewReader(requestBody))
	req.Header.Set("X-Api-Key", testKey)
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("the request should have been cancelled")
	}

	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		g.stats.mu.Lock()
		cs := g.stats.callers["tester"]
		seen := cs != nil && cs.byStatus[clientClosed] == 1
		g.stats.mu.Unlock()
		if seen {
			return
		}
	}
	t.Error("the abandoned request was never recorded as 499")
}

func TestSpreadOfRecentDurations(t *testing.T) {
	var r recent
	if got := r.spread(); got != (spread{}) {
		t.Errorf("empty: %+v", got)
	}
	r.add(7 * time.Millisecond)
	if got := r.spread(); got != (spread{Count: 1, P50: 7, P95: 7, P99: 7}) {
		t.Errorf("one value: %+v", got)
	}

	r = recent{}
	for ms := 100; ms >= 1; ms-- { // 1 to 100 ms, added out of order
		r.add(time.Duration(ms) * time.Millisecond)
	}
	if got := r.spread(); got != (spread{Count: 100, P50: 50, P95: 95, P99: 99}) {
		t.Errorf("1 to 100 ms: %+v", got)
	}

	// Past keepLast the oldest are overwritten: after 1,000 fast requests
	// and then 1,000 slow ones, only the slow ones are left.
	r = recent{}
	for i := 0; i < keepLast; i++ {
		r.add(time.Millisecond)
	}
	for i := 0; i < keepLast; i++ {
		r.add(time.Second)
	}
	if got := r.spread(); got.Count != keepLast || got.P50 != 1000 || len(r.values) != keepLast {
		t.Errorf("after wrapping: %+v, %d kept", got, len(r.values))
	}
	// Durations under a hundredth of a millisecond are not lost to rounding up.
	r = recent{}
	r.add(250 * time.Microsecond)
	if got := r.spread(); got.P50 != 0.25 {
		t.Errorf("250 microseconds: %+v", got)
	}
}

func TestStatsSurviveManyRequestsAtOnce(t *testing.T) {
	var calls atomic.Int32
	_, url := streamGateway(t, numbered(&calls), withAdmin)

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			post(t, url+"/v1/messages", requestBody, nil)
			readStats(t, url)
		}()
	}
	wg.Wait()

	if tester := readStats(t, url)["tester"]; tester.Requests != 30 || tester.ByStatus["200"] != 30 || tester.Today.Requests != 30 {
		t.Errorf("tester: %+v", tester)
	}
}
