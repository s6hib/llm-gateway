package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/s6hib/llm-gateway/fake"
)

const streamBody = `{"model":"claude-haiku-5-5","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`

// sse writes one event the way the API does and sends it at once.
func sse(w http.ResponseWriter, event, data string) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	http.NewResponseController(w).Flush()
}

func startEvent(usage string) string {
	return `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-haiku-5-5","usage":` + usage + `}}`
}

func textDelta(text string) string {
	return `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + text + `"}}`
}

// streamGateway runs a gateway with a fixed clock in front of upstream.
// Each change is applied to the gateway before it starts.
func streamGateway(t *testing.T, upstream http.Handler, changes ...func(*gateway)) (*gateway, string) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	g := testGateway(t, up.URL, 1000)
	g.spend.now = func() time.Time { return time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC) }
	for _, change := range changes {
		change(g)
	}
	gw := httptest.NewServer(g.handler())
	t.Cleanup(gw.Close)
	return g, gw.URL
}

// tracing makes a gateway close a channel the first time a call passes each
// of the two points that the hang-up tests need to act on.
type tracing struct {
	answerStarted, callerGone chan struct{}
}

func newTracing() (*tracing, func(*gateway)) {
	tr := &tracing{answerStarted: make(chan struct{}), callerGone: make(chan struct{})}
	var started, gone sync.Once
	return tr, func(g *gateway) {
		g.trace = func(point string) {
			switch point {
			case "answer started":
				started.Do(func() { close(tr.answerStarted) })
			case "caller gone":
				gone.Do(func() { close(tr.callerGone) })
			}
		}
	}
}

// counted waits for the gateway to finish counting a stream, which happens
// just after the last byte has gone to the caller.
func counted(t *testing.T, g *gateway) tally {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		g.spend.mu.Lock()
		got := g.spend.days["2026-03-04"]["tester"]
		g.spend.mu.Unlock()
		if got != nil {
			return *got
		}
	}
	t.Fatal("the stream was never counted")
	return tally{}
}

func TestStreamReachesCallerUnchangedAndIsCounted(t *testing.T) {
	// What the fake upstream sends when called directly.
	direct := httptest.NewServer(fake.Handler(0, 0))
	defer direct.Close()
	_, want := post(t, direct.URL+"/v1/messages", streamBody, nil)

	g, url := streamGateway(t, fake.Handler(0, 0))
	resp, got := post(t, url+"/v1/messages", streamBody, nil)

	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d, Content-Type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if got != want || !strings.Contains(got, "event: message_stop") {
		t.Errorf("through the gateway:\n%s\ndirect:\n%s", got, want)
	}
	// 12 in and 6 out on haiku, the same as the plain reply: 4 millionths.
	if got := counted(t, g); got != (tally{Requests: 1, InputTokens: 12, OutputTokens: 6, CostMicroUSD: 4}) {
		t.Errorf("counted %+v", got)
	}
}

func TestStreamArrivesPieceByPiece(t *testing.T) {
	release := make(chan struct{})
	_, url := streamGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sse(w, "message_start", startEvent(`{"input_tokens":1,"output_tokens":1}`))
		select { // the rest is held back until the test has seen the first event
		case <-release:
		case <-r.Context().Done():
			return
		}
		sse(w, "message_stop", `{"type":"message_stop"}`)
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/messages", strings.NewReader(streamBody))
	req.Header.Set("X-Api-Key", testKey)
	first := make(chan string, 1)
	rest := make(chan string, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			first <- err.Error()
			return
		}
		defer resp.Body.Close()
		lines := bufio.NewReader(resp.Body)
		line, _ := lines.ReadString('\n')
		first <- line
		all, _ := io.ReadAll(lines)
		rest <- string(all)
	}()

	select {
	case line := <-first:
		if line != "event: message_start\n" {
			t.Fatalf("first line %q", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the first event did not arrive while the upstream was still sending: the gateway held it back")
	}
	close(release)
	if got := <-rest; !strings.Contains(got, "event: message_stop") {
		t.Errorf("the rest of the stream: %q", got)
	}
}

func TestStreamTokenCountsAreRunningTotals(t *testing.T) {
	g, url := streamGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sse(w, "message_start", startEvent(`{"input_tokens":100,"cache_read_input_tokens":50,"output_tokens":1}`))
		sse(w, "ping", `{"type":"ping"}`)
		sse(w, "something_new", `{"type":"something_new","usage":{"output_tokens":777777}}`)
		sse(w, "message_delta", `{"type":"message_delta","delta":{},"usage":{"output_tokens":40}}`)
		sse(w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":120,"cache_read_input_tokens":null,"output_tokens":90}}`)
		sse(w, "message_stop", `{"type":"message_stop"}`)
	}))

	post(t, url+"/v1/messages", streamBody, nil)

	// The last counts win: 120 fresh + 50 cached in, 90 out. Not 40 + 90.
	// On haiku: 120*0.10 + 50*0.10*0.1 + 90*0.50 = 12 + 0.5 + 45 = 57.5
	if got := counted(t, g); got != (tally{Requests: 1, InputTokens: 170, OutputTokens: 90, CostMicroUSD: 58}) {
		t.Errorf("counted %+v", got)
	}
}

// hangUpMidStream starts a streamed call, reads past the first answer text
// and then hangs up.
func hangUpMidStream(t *testing.T, url string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/messages", strings.NewReader(streamBody))
	req.Header.Set("X-Api-Key", testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewReader(resp.Body)
	for i := 0; i < 5; i++ { // message_start, a blank line, the delta
		lines.ReadString('\n')
	}
	cancel()
	resp.Body.Close()
}

func TestCallerHangingUpMidStreamPaysForTheWholeAnswer(t *testing.T) {
	tr, trace := newTracing()
	g, url := streamGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sse(w, "message_start", startEvent(`{"input_tokens":10,"output_tokens":1}`))
		sse(w, "content_block_delta", textDelta("the start of a long answer"))
		select {
		case <-tr.callerGone: // the rest is sent only once the gateway knows the caller has left
		case <-r.Context().Done():
			return // the gateway dropped the call: the test will see no final count
		}
		// What only the end of a stream says: the real output, with thinking
		// the caller never saw, and the web searches.
		sse(w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":500,"server_tool_use":{"web_search_requests":2}}}`)
		sse(w, "message_stop", `{"type":"message_stop"}`)
	}), trace)

	hangUpMidStream(t, url)

	// 10*0.10 + 500*0.50 + 2 searches at 10,000 = 1 + 250 + 20,000
	if got := counted(t, g); got != (tally{Requests: 1, InputTokens: 10, OutputTokens: 500, CostMicroUSD: 20_251}) {
		t.Errorf("counted %+v", got)
	}
}

func TestStreamThatNeverEndsIsDroppedAndEstimated(t *testing.T) {
	dropped := make(chan struct{})
	g, url := streamGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sse(w, "message_start", startEvent(`{"input_tokens":10,"output_tokens":1}`))
		sse(w, "content_block_delta", textDelta(strings.Repeat("x", 2400)))
		sse(w, "content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":"`+strings.Repeat("s", 600)+`"}}`)
		<-r.Context().Done() // never finishes; only the gateway giving up ends it
		close(dropped)
	}), func(g *gateway) { g.drainLimit = 100 * time.Millisecond })

	hangUpMidStream(t, url)

	select {
	case <-dropped:
	case <-time.After(3 * time.Second):
		t.Fatal("the gateway kept the upstream call open past its drain limit")
	}
	// The stream only reported 1 output token. 3,000 bytes of answer and
	// signature went by, which is counted as 1,000 tokens.
	if got := counted(t, g); got.OutputTokens != 1000 || got.InputTokens != 10 {
		t.Errorf("counted %+v", got)
	}
}

func TestCallerThatStopsReadingIsStillCounted(t *testing.T) {
	g, url := streamGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sse(w, "message_start", startEvent(`{"input_tokens":10,"output_tokens":1}`))
		big := textDelta(strings.Repeat("x", 64<<10))
		for i := 0; i < 512; i++ { // 32 MB, far more than the connection will hold unread
			sse(w, "content_block_delta", big)
		}
		sse(w, "message_delta", `{"type":"message_delta","delta":{},"usage":{"output_tokens":777}}`)
		sse(w, "message_stop", `{"type":"message_stop"}`)
	}), func(g *gateway) { g.writeTimeout = 100 * time.Millisecond })

	// A caller that sends its request and then never reads the answer.
	conn, err := net.Dial("tcp", strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST /v1/messages HTTP/1.1\r\nHost: gateway\r\nX-Api-Key: %s\r\nContent-Length: %d\r\n\r\n%s", testKey, len(streamBody), streamBody)

	if got := counted(t, g); got.OutputTokens != 777 {
		t.Errorf("counted %+v", got)
	}
}

func TestCallerHangingUpDuringPlainAnswerIsStillCounted(t *testing.T) {
	tr, trace := newTracing()
	g, url := streamGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		http.NewResponseController(w).Flush() // the answer has started
		select {
		case <-tr.callerGone: // the body is sent only once the gateway knows the caller has left
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, `{"model":"claude-haiku-5-5","usage":{"input_tokens":10,"output_tokens":20}}`)
	}), trace)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/messages", strings.NewReader(requestBody))
	req.Header.Set("X-Api-Key", testKey)
	failed := make(chan error, 1)
	go func() {
		_, err := http.DefaultClient.Do(req)
		failed <- err
	}()
	<-tr.answerStarted // hanging up any earlier would cancel the call
	cancel()
	<-failed

	if got := counted(t, g); got.InputTokens != 10 || got.OutputTokens != 20 {
		t.Errorf("counted %+v", got)
	}
}

func TestErrorInsideStreamReachesCallerAndIsCounted(t *testing.T) {
	const overloadedEvent = `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`
	g, url := streamGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sse(w, "message_start", startEvent(`{"input_tokens":10,"output_tokens":1}`))
		sse(w, "error", overloadedEvent)
	}))

	resp, body := post(t, url+"/v1/messages", streamBody, nil)

	if resp.StatusCode != http.StatusOK || !strings.Contains(body, overloadedEvent) {
		t.Errorf("status %d, body %s", resp.StatusCode, body)
	}
	if got := counted(t, g); got.Requests != 1 || got.InputTokens != 10 {
		t.Errorf("counted %+v", got)
	}
}

func TestStreamedCallsCountTowardsTheCap(t *testing.T) {
	g, url := streamGateway(t, fake.Handler(0, 0))
	g.dailyCap = 4 // one streamed call from the fake costs 4 millionths

	if resp, _ := post(t, url+"/v1/messages", streamBody, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("first call: status %d", resp.StatusCode)
	}
	counted(t, g)
	if resp, _ := post(t, url+"/v1/messages", streamBody, nil); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("second call: status %d, want 429", resp.StatusCode)
	}
}

// feed gives a stream to a counter in pieces of the given size.
func feed(stream string, size int) (string, usage, bool) {
	var c streamCounter
	for len(stream) > 0 {
		n := min(size, len(stream))
		c.read([]byte(stream[:n]))
		stream = stream[n:]
	}
	return c.result()
}

func TestStreamCounterHandlesAnySplit(t *testing.T) {
	stream := "event: message_start\ndata: " + startEvent(`{"input_tokens":25,"output_tokens":1}`) + "\n\n" +
		"event: ping\ndata: {\"type\": \"ping\"}\n\n" +
		": a comment line\n" +
		"data: not json\n\n" +
		"event: content_block_delta\ndata:" + textDelta("Hello") + "\r\n\r\n" + // no space after the colon, CRLF
		"event: message_delta\ndata: {\"type\": \"message_delta\", \"usage\": {\"output_tokens\": 15}}\n\n" +
		"event: message_stop\ndata: {\"type\": \"message_stop\"}\n\n"

	for _, size := range []int{1, 2, 3, 7, 64, len(stream)} {
		model, u, ok := feed(stream, size)
		if !ok || model != "claude-haiku-5-5" || u.InputTokens != 25 || u.OutputTokens != 15 {
			t.Errorf("in pieces of %d: model %q, usage %+v, ok %v", size, model, u, ok)
		}
	}
}

func TestLaterCountsDoNotLoseCacheWrites(t *testing.T) {
	// The start says 248 tokens were written to the cache, 100 of them to the
	// 1-hour cache. The end updates the total to 300, with a null split.
	stream := "data: " + startEvent(`{"cache_creation_input_tokens":248,"cache_creation":{"ephemeral_5m_input_tokens":148,"ephemeral_1h_input_tokens":100}}`) + "\n\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"cache_creation_input_tokens\":300,\"cache_creation\":null,\"output_tokens\":1}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	_, u, _ := feed(stream, 9)

	if fiveMinute, oneHour := u.cacheWrites(); fiveMinute != 200 || oneHour != 100 {
		t.Errorf("cache writes: %d for 5 minutes, %d for 1 hour", fiveMinute, oneHour)
	}
}

func TestStreamCounterSkipsAnOverlongLine(t *testing.T) {
	stream := "data: " + startEvent(`{"input_tokens":25,"output_tokens":1}`) + "\n\n" +
		"data: " + textDelta(strings.Repeat("x", maxEventLine)) + "\n\n" + // too long to hold
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":15}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	for _, size := range []int{4096, len(stream)} {
		_, u, ok := feed(stream, size)
		if !ok || u.OutputTokens != 15 {
			t.Errorf("in pieces of %d: usage %+v, ok %v", size, u, ok)
		}
	}
}

func TestStreamWithNothingToCount(t *testing.T) {
	if _, _, ok := feed("event: ping\ndata: {\"type\": \"ping\"}\n\n", 5); ok {
		t.Error("a stream with no message_start was counted")
	}
	g, url := streamGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sse(w, "ping", `{"type":"ping"}`)
	}))
	if resp, body := post(t, url+"/v1/messages", streamBody, nil); resp.StatusCode != http.StatusOK || !strings.Contains(body, "ping") {
		t.Errorf("status %d, body %s", resp.StatusCode, body)
	}
	if spent, _ := g.spend.spentToday("tester"); spent != 0 {
		t.Errorf("spent %d", spent)
	}
}
