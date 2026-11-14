package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func parseUsage(t *testing.T, raw string) usage {
	t.Helper()
	var u usage
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		t.Fatal(err)
	}
	return u
}

// Each want is worked out by hand from the pricing page's dollars per million
// tokens, in millionths of a dollar.
func TestCost(t *testing.T) {
	for _, c := range []struct {
		what, model, usage string
		want               int64
	}{
		{"haiku, 100,000 in and 1M out: $0.01 + $0.50", "claude-haiku-5-5",
			`{"input_tokens":100000,"output_tokens":1000000}`, 510_000},
		{"sonnet, 1M in and 1M out: $2 + $10", "claude-sonnet-5-5",
			`{"input_tokens":1000000,"output_tokens":1000000}`, 12_000_000},
		{"opus, 1M read from cache: $0.20", "claude-opus-5-5",
			`{"cache_read_input_tokens":1000000}`, 200_000},
		{"fable, 1M read from cache: $0.20", "claude-fable-5-2",
			`{"cache_read_input_tokens":1000000}`, 200_000},
		{"haiku, 100,000 read from cache: $0.001", "claude-haiku-5-5",
			`{"cache_read_input_tokens":100000}`, 1_000},
		{"opus, 1M written to the 5-minute cache: $5", "claude-opus-5-5",
			`{"cache_creation_input_tokens":1000000,"cache_creation":{"ephemeral_5m_input_tokens":1000000}}`, 5_000_000},
		{"opus, 1M written to the 1-hour cache: $8", "claude-opus-5-5",
			`{"cache_creation_input_tokens":1000000,"cache_creation":{"ephemeral_1h_input_tokens":1000000}}`, 8_000_000},
		{"cache writes with no split are charged as 5-minute writes", "claude-opus-5-5",
			`{"cache_creation_input_tokens":1000000}`, 5_000_000},
		{"cache writes the split leaves out are charged as 5-minute writes: 0.9M at $5 + 0.1M at $8", "claude-opus-5-5",
			`{"cache_creation_input_tokens":1000000,"cache_creation":{"ephemeral_1h_input_tokens":100000}}`, 4_500_000 + 800_000},
		{"a split bigger than the total is charged in full", "claude-opus-5-5",
			`{"cache_creation_input_tokens":5,"cache_creation":{"ephemeral_5m_input_tokens":1000000,"ephemeral_1h_input_tokens":1000000}}`, 5_000_000 + 8_000_000},
		{"haiku at exactly 100,000 prompt tokens pays the low price", "claude-haiku-5-5",
			`{"input_tokens":50000,"cache_read_input_tokens":50000}`, 5_000 + 500},
		{"haiku over 100,000 prompt tokens pays 5x on everything", "claude-haiku-5-5",
			`{"input_tokens":50001,"cache_read_input_tokens":50000,"output_tokens":1000}`, 25_001 + 2_500 + 2_500},
		{"fast mode doubles opus: $8 + $40", "claude-opus-5-5",
			`{"input_tokens":1000000,"output_tokens":1000000,"speed":"fast"}`, 48_000_000},
		{"US-only inference adds 10%", "claude-sonnet-5-5",
			`{"input_tokens":1000000,"inference_geo":"us"}`, 2_200_000},
		{"3 web searches at $10 per 1,000", "claude-sonnet-5-5",
			`{"server_tool_use":{"web_search_requests":3}}`, 30_000},
		{"a dated id uses its model's price", "claude-haiku-5-5-20261001",
			`{"input_tokens":100000}`, 10_000},
		{"an unknown model is charged as the most expensive", "claude-something-new",
			`{"input_tokens":1000000}`, 8_000_000},
		{"the fake upstream's reply: 12 in, 6 out on haiku", "claude-haiku-5-5",
			`{"input_tokens":12,"output_tokens":6}`, 4}, // 1.2 + 3 = 4.2, rounded
		{"negative counts are counted as 0", "claude-opus-5-5",
			`{"input_tokens":-1000000,"output_tokens":-5,"cache_read_input_tokens":-1,"cache_creation_input_tokens":-1,"cache_creation":{"ephemeral_5m_input_tokens":-9,"ephemeral_1h_input_tokens":-9},"server_tool_use":{"web_search_requests":-3}}`, 0},
		{"an absurd count stops at the most one call can cost", "claude-fable-5-2",
			`{"output_tokens":9000000000000000000,"server_tool_use":{"web_search_requests":9000000000000000000}}`, 1_000_000_000_000_000},
	} {
		if got := cost(c.model, parseUsage(t, c.usage)); got != c.want {
			t.Errorf("%s: got %d, want %d", c.what, got, c.want)
		}
	}
}

// answering returns an upstream that replies with the given usage and counts
// how many calls reached it.
func answering(calls *atomic.Int32, usageJSON string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[],"usage":%s}`, usageJSON)
	})
}

func readSpendFile(t *testing.T, path string) map[string]map[string]tally {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var days map[string]map[string]tally
	if err := json.Unmarshal(raw, &days); err != nil {
		t.Fatalf("%v in %s", err, raw)
	}
	return days
}

func TestCountsSpendAndKeepsItAcrossRestarts(t *testing.T) {
	var calls atomic.Int32
	// 1,000 prompt tokens in three parts and 2,000 out on haiku:
	// 700*0.10 + 200*0.10*1.25 + 100*0.10*0.1 + 2000*0.50 = 70 + 25 + 1 + 1000
	up := httptest.NewServer(answering(&calls, `{"input_tokens":700,"cache_creation_input_tokens":200,"cache_read_input_tokens":100,"output_tokens":2000}`))
	defer up.Close()
	g := testGateway(t, up.URL, 1000)
	clock := time.Date(2026, 3, 4, 23, 0, 0, 0, time.UTC)
	g.spend.now = func() time.Time { return clock }
	gw := httptest.NewServer(g.handler())
	defer gw.Close()

	for i := 0; i < 2; i++ {
		if resp, body := post(t, gw.URL+"/v1/messages", requestBody, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d, body %s", resp.StatusCode, body)
		}
	}

	want := tally{Requests: 2, InputTokens: 2000, OutputTokens: 4000, CostMicroUSD: 2 * 1096}
	if got := readSpendFile(t, g.spend.path)["2026-03-04"]["tester"]; got != want {
		t.Errorf("spend file has %+v, want %+v", got, want)
	}

	// A gateway started later on the same file carries on from the same totals.
	reopened, err := openSpend(g.spend.path)
	if err != nil {
		t.Fatal(err)
	}
	reopened.now = g.spend.now
	if got, _ := reopened.spentToday("tester"); got != 2*1096 {
		t.Errorf("after a restart spentToday is %d", got)
	}
	if got, _ := reopened.spentToday("other"); got != 0 {
		t.Errorf("a caller that sent nothing has spent %d", got)
	}
}

func TestDailyCapStopsACallerUntilTomorrow(t *testing.T) {
	var calls atomic.Int32
	// 12,000 output tokens on haiku is $0.006 a call.
	up := httptest.NewServer(answering(&calls, `{"output_tokens":12000}`))
	defer up.Close()
	g := testGateway(t, up.URL, 1000)
	g.dailyCap = 10_000 // $0.01
	clock := time.Date(2026, 3, 4, 22, 0, 0, 0, time.UTC)
	g.spend.now = func() time.Time { return clock }
	gw := httptest.NewServer(g.handler())
	defer gw.Close()
	send := func(key string) (*http.Response, string) {
		return post(t, gw.URL+"/v1/messages", requestBody, map[string]string{"X-Api-Key": key})
	}

	// $0 and then $0.006 spent are both under the cap. The second call takes
	// the total to $0.012, so the third is refused.
	for i := 0; i < 2; i++ {
		if resp, body := send(testKey); resp.StatusCode != http.StatusOK {
			t.Fatalf("call %d: status %d, body %s", i+1, resp.StatusCode, body)
		}
	}
	resp, body := send(testKey)
	if resp.StatusCode != http.StatusTooManyRequests || errorType(t, body) != "rate_limit_error" {
		t.Fatalf("over the cap: status %d, body %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Retry-After"); got != "7200" { // 22:00 to midnight UTC
		t.Errorf("Retry-After %q", got)
	}
	if calls.Load() != 2 {
		t.Errorf("%d calls reached the upstream, want 2", calls.Load())
	}

	if resp, _ := send("test-gateway-key-0002"); resp.StatusCode != http.StatusOK {
		t.Errorf("another caller was stopped too: status %d", resp.StatusCode)
	}

	clock = clock.Add(2 * time.Hour) // midnight: a new day
	if resp, _ := send(testKey); resp.StatusCode != http.StatusOK {
		t.Errorf("the next day: status %d", resp.StatusCode)
	}
}

func TestFailedCallsCostNothing(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"no"},"usage":{"output_tokens":999999}}`)
	}))
	defer up.Close()
	g := testGateway(t, up.URL, 1000)
	gw := httptest.NewServer(g.handler())
	defer gw.Close()

	post(t, gw.URL+"/v1/messages", requestBody, nil)

	if got, _ := g.spend.spentToday("tester"); got != 0 {
		t.Errorf("a failed call cost %d", got)
	}
	if _, err := os.Stat(g.spend.path); err == nil {
		t.Error("a spend file was written for a call that cost nothing")
	}
}

func TestAnswerThatCannotBeCountedIsNotPassedOn(t *testing.T) {
	for _, answer := range []string{
		`{"id":"msg_1","type":"message","content":[{"type":"text","text":"free"}]}`, // no usage
		"event: message_start\ndata: {}\n\n",                                        // a stream
		``,
	} {
		url := start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, answer) }))

		resp, body := post(t, url+"/v1/messages", requestBody, nil)

		if resp.StatusCode != http.StatusBadGateway || errorType(t, body) != "api_error" {
			t.Errorf("%q: status %d, body %s", answer, resp.StatusCode, body)
		}
	}
}

func TestParseCap(t *testing.T) {
	for dollars, want := range map[string]int64{"1": 1_000_000, "0.10": 100_000, "0.000001": 1, "1000000": 1_000_000_000_000} {
		if got, err := parseCap(dollars); err != nil || got != want {
			t.Errorf("parseCap(%q) = %d, %v", dollars, got, err)
		}
	}
	for _, bad := range []string{"", "0", "-1", "NaN", "Inf", "1e-9", "1000001", "a dollar"} {
		if got, err := parseCap(bad); err == nil {
			t.Errorf("parseCap(%q) = %d, should fail", bad, got)
		}
	}
}

func TestSpendFileHoldingNullStillCounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spend.json")
	os.WriteFile(path, []byte(`null`), 0o600)
	s, err := openSpend(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.add("tester", usage{OutputTokens: 1}, 5); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.spentToday("tester"); got != 5 {
		t.Errorf("spentToday is %d", got)
	}
}

func TestSpendCountsCallsThatArriveTogether(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(answering(&calls, `{"input_tokens":10,"output_tokens":10}`))
	defer up.Close()
	g := testGateway(t, up.URL, 1000)
	g.spend.now = func() time.Time { return time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC) }
	gw := httptest.NewServer(g.handler())
	defer gw.Close()

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			post(t, gw.URL+"/v1/messages", requestBody, nil)
		}()
	}
	wg.Wait()

	if got := readSpendFile(t, g.spend.path)["2026-03-04"]["tester"]; got.Requests != 40 || got.CostMicroUSD != 40*6 {
		t.Errorf("after 40 calls at once the file has %+v", got)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(g.spend.path), ".spend-*"))
	if len(leftovers) != 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

func TestOpenSpendRefusesABrokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spend.json")
	os.WriteFile(path, []byte(`{"2026-03-04": {"tester": `), 0o600)
	if _, err := openSpend(path); err == nil {
		t.Error("a cut-off spend file was accepted")
	}
}
