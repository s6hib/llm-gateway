package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/s6hib/llm-gateway/fake"
)

const overloaded = `{"type":"error","error":{"type":"overloaded_error","message":"busy"},"request_id":"req_1"}`

// flaky answers with status for the first failures calls and then hands over
// to the fake upstream. It counts every call and keeps each request body.
type flaky struct {
	failures   int32
	status     int
	retryAfter string

	calls  atomic.Int32
	mu     sync.Mutex
	bodies []string
}

func (f *flaky) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, string(raw))
	f.mu.Unlock()
	if f.calls.Add(1) <= f.failures {
		if f.retryAfter != "" {
			w.Header().Set("Retry-After", f.retryAfter)
		}
		w.WriteHeader(f.status)
		io.WriteString(w, overloaded)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	fake.Handler(0, 0).ServeHTTP(w, r)
}

// startRecording is start, but it also returns the waits the gateway asked for.
func startRecording(t *testing.T, upstream http.Handler, maxRetries int) (string, *[]time.Duration) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	g := testGateway(t, up.URL, 1000)
	g.maxRetries = maxRetries
	var mu sync.Mutex
	waits := &[]time.Duration{}
	g.sleep = func(_ context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		*waits = append(*waits, d)
		return nil
	}
	gw := httptest.NewServer(g.handler())
	t.Cleanup(gw.Close)
	return gw.URL, waits
}

func TestRetriesUntilUpstreamRecovers(t *testing.T) {
	up := &flaky{failures: 2, status: 529}
	url, waits := startRecording(t, up, 2)

	resp, body := post(t, url+"/v1/messages", requestBody, nil)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body %s", resp.StatusCode, body)
	}
	if up.calls.Load() != 3 {
		t.Errorf("upstream was called %d times, want 3", up.calls.Load())
	}
	for i, got := range up.bodies {
		if got != requestBody {
			t.Errorf("try %d sent body %q", i+1, got)
		}
	}
	// The first wait is 250 to 500 ms and the second 500 ms to 1 s.
	if len(*waits) != 2 || (*waits)[0] < 250*time.Millisecond || (*waits)[0] > 500*time.Millisecond ||
		(*waits)[1] < 500*time.Millisecond || (*waits)[1] > time.Second {
		t.Errorf("waits %v", *waits)
	}
}

func TestGivesUpAfterMaxRetries(t *testing.T) {
	up := &flaky{failures: 100, status: 500}
	url, _ := startRecording(t, up, 2)

	resp, body := post(t, url+"/v1/messages", requestBody, nil)

	// The caller gets the upstream's last answer as it was.
	if resp.StatusCode != http.StatusInternalServerError || body != overloaded {
		t.Errorf("status %d, body %s", resp.StatusCode, body)
	}
	if up.calls.Load() != 3 {
		t.Errorf("upstream was called %d times, want 3", up.calls.Load())
	}
}

func TestMaxRetriesZeroMeansOneTry(t *testing.T) {
	up := &flaky{failures: 100, status: 529}
	url, waits := startRecording(t, up, 0)

	resp, _ := post(t, url+"/v1/messages", requestBody, nil)

	if resp.StatusCode != 529 || up.calls.Load() != 1 || len(*waits) != 0 {
		t.Errorf("status %d, %d calls, waits %v", resp.StatusCode, up.calls.Load(), *waits)
	}
}

func TestDoesNotRetryCallerMistakes(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 413} {
		up := &flaky{failures: 100, status: status}
		url, _ := startRecording(t, up, 2)

		resp, _ := post(t, url+"/v1/messages", requestBody, nil)

		if resp.StatusCode != status || up.calls.Load() != 1 {
			t.Errorf("a %d was answered with %d after %d calls", status, resp.StatusCode, up.calls.Load())
		}
	}
}

func TestWaitsAsLongAsUpstreamAsks(t *testing.T) {
	up := &flaky{failures: 1, status: 429, retryAfter: "3"}
	url, waits := startRecording(t, up, 2)

	resp, _ := post(t, url+"/v1/messages", requestBody, nil)

	if resp.StatusCode != http.StatusOK || len(*waits) != 1 || (*waits)[0] != 3*time.Second {
		t.Errorf("status %d, waits %v", resp.StatusCode, *waits)
	}
}

func TestLongRetryAfterGoesBackToCaller(t *testing.T) {
	for _, asked := range []string{"60", "10000000000"} {
		up := &flaky{failures: 100, status: 429, retryAfter: asked}
		url, waits := startRecording(t, up, 2)

		resp, body := post(t, url+"/v1/messages", requestBody, nil)

		if resp.StatusCode != http.StatusTooManyRequests || body != overloaded || resp.Header.Get("Retry-After") != asked {
			t.Errorf("status %d, Retry-After %q, body %s", resp.StatusCode, resp.Header.Get("Retry-After"), body)
		}
		if up.calls.Load() != 1 || len(*waits) != 0 {
			t.Errorf("Retry-After %s: %d calls, waits %v", asked, up.calls.Load(), *waits)
		}
	}
}

func TestRetryAfterZeroStillBacksOff(t *testing.T) {
	up := &flaky{failures: 1, status: 429, retryAfter: "0"}
	url, waits := startRecording(t, up, 2)

	post(t, url+"/v1/messages", requestBody, nil)

	if len(*waits) != 1 || (*waits)[0] < 250*time.Millisecond {
		t.Errorf("waits %v", *waits)
	}
}

func TestRetriesBrokenConnection(t *testing.T) {
	var calls atomic.Int32
	url, _ := startRecording(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			// Cut the connection without answering.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		fake.Handler(0, 0).ServeHTTP(w, r)
	}), 2)

	resp, body := post(t, url+"/v1/messages", requestBody, nil)

	if resp.StatusCode != http.StatusOK || calls.Load() != 2 {
		t.Errorf("status %d after %d calls, body %s", resp.StatusCode, calls.Load(), body)
	}
}

func TestSlowUpstreamTimesOut(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.Copy(io.Discard, r.Body) // with the body read, Go notices the gateway hanging up
		select {                    // never answers in time
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer up.Close()
	g := testGateway(t, up.URL, 1000)
	g.client = newClient(150 * time.Millisecond)
	gw := httptest.NewServer(g.handler())
	defer gw.Close()

	resp, body := post(t, gw.URL+"/v1/messages", requestBody, nil)

	if resp.StatusCode != http.StatusGatewayTimeout || errorType(t, body) != "timeout_error" {
		t.Errorf("status %d, body %s", resp.StatusCode, body)
	}
	if calls.Load() != 3 {
		t.Errorf("upstream was called %d times, want 3", calls.Load())
	}
}

func TestCallerHangingUpStopsRetries(t *testing.T) {
	up := &flaky{failures: 100, status: 529}
	upServer := httptest.NewServer(up)
	defer upServer.Close()
	g := testGateway(t, upServer.URL, 1000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan error, 1)
	g.sleep = func(serverSide context.Context, _ time.Duration) error {
		cancel()                              // the caller hangs up as the wait begins
		err := sleep(serverSide, time.Minute) // a real wait that only the hang-up ends early
		stopped <- err
		return err
	}
	gw := httptest.NewServer(g.handler())
	defer gw.Close()

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, gw.URL+"/v1/messages", bytes.NewReader([]byte(requestBody)))
	req.Header.Set("X-Api-Key", testKey)
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("the caller's request should have been cancelled")
	}

	select {
	case err := <-stopped:
		if err == nil {
			t.Error("the wait ran to the end though the caller had hung up")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the gateway kept waiting after the caller hung up")
	}
	if up.calls.Load() != 1 {
		t.Errorf("upstream was called %d times, want 1", up.calls.Load())
	}
}

func TestBackoffDoublesUpToTheCap(t *testing.T) {
	for try, top := range []time.Duration{
		500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second,
		8 * time.Second, 8 * time.Second, 8 * time.Second,
	} {
		for i := 0; i < 200; i++ {
			if got := backoff(try); got < top/2 || got > top {
				t.Fatalf("backoff(%d) = %s, want %s to %s", try, got, top/2, top)
			}
		}
	}
}

func TestRetryAfterHeader(t *testing.T) {
	for value, want := range map[string]time.Duration{"0": 0, "7": 7 * time.Second, "10000000000": time.Hour} {
		if got, ok := retryAfter(http.Header{"Retry-After": {value}}); !ok || got != want {
			t.Errorf("retryAfter(%q) = %s, %v", value, got, ok)
		}
	}
	for _, value := range []string{"", "-1", "soon", "Fri, 13 Nov 2026 16:00:00 GMT"} {
		if _, ok := retryAfter(http.Header{"Retry-After": {value}}); ok {
			t.Errorf("retryAfter(%q) should not be read as seconds", value)
		}
	}
}
