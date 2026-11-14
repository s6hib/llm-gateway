// llm-gateway forwards Messages API requests to an upstream model API.
//
// Settings come from the environment:
//
//	ANTHROPIC_API_KEY      the upstream key (required)
//	GATEWAY_KEYS           who may call, as name:key,name:key (required)
//	RATE_LIMIT_PER_MINUTE  requests a minute allowed per caller (default 60)
//	UPSTREAM_TIMEOUT       how long to wait for the upstream to start answering (default 60s)
//	MAX_RETRIES            extra tries after a failed upstream call (default 2)
//	DAILY_CAP_USD          dollars each caller may spend per UTC day (default 1)
//	SPEND_FILE             where the per-caller totals are kept (default spend.json)
//	ADMIN_KEY              the key that may read GET /stats (optional; no key, no stats page)
//	UPSTREAM_URL           where requests are forwarded (default https://api.anthropic.com)
//	ADDR                   where the gateway listens (default 127.0.0.1:8080)
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// maxBody is the largest request body the gateway will read. The whole body
// is held in memory, so this is kept far below what the upstream accepts.
const maxBody = 10 << 20 // 10 MB

// maxAnswer is the largest plain upstream answer the gateway will read. A
// plain answer is held in memory too, so that its token counts can be read
// before it is sent on. A streamed answer is passed on piece by piece and has
// no such limit.
const maxAnswer = 32 << 20 // 32 MB

// hopHeaders describe one connection between two machines. A proxy does not
// pass them on, because its two connections are separate (RFC 9110, 7.6.1).
var hopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

type gateway struct {
	upstream string // base address with no trailing slash
	apiKey   string // sent upstream in place of whatever the caller sent
	client   *http.Client
	keys     map[[32]byte]string // hash of a gateway key -> the caller's name
	limiter  *limiter

	maxRetries int
	sleep      func(context.Context, time.Duration) error // swapped out in tests

	// Tests shorten these two.
	writeTimeout time.Duration // how long one write of an answer may take to reach the caller
	drainLimit   time.Duration // how long to keep reading an answer after the caller has gone

	// trace, when set, is told as a call passes certain points. Only tests
	// set it, to act at an exact moment without guessing at timing.
	trace func(point string)

	spend    *spendStore
	dailyCap int64 // millionths of a dollar, per caller per UTC day

	cache *cache

	stats    *stats
	adminKey [32]byte // hash of ADMIN_KEY
	hasAdmin bool
}

// newClient returns an HTTP client that gives up on a call when the upstream
// has not started its answer within timeout. The limit is on the wait for the
// first byte of the answer, so an answer that has started is never cut off.
func newClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = timeout
	return &http.Client{Transport: transport}
}

func (g *gateway) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", g.messages)
	mux.HandleFunc("GET /stats", g.statsPage)
	mux.HandleFunc("GET /healthz", health)
	mux.HandleFunc("GET /{$}", landing) // {$} matches the bare address only, not every path under it
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found_error", "the gateway only serves POST /v1/messages")
	})
	return mux
}

// messages sends the caller's request upstream and copies the answer back.
func (g *gateway) messages(w http.ResponseWriter, r *http.Request) {
	c := &call{start: time.Now()}
	noted := &statusWriter{ResponseWriter: w}
	w = noted
	defer func() { g.finish(c, noted.status) }()

	name, ok := g.caller(r)
	c.name = name
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication_error", "missing or unknown gateway key")
		return
	}
	if ok, wait := g.limiter.allow(name); !ok {
		// Whole seconds, rounded up, and never 0: that would tell the caller
		// to retry at once.
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(wait.Seconds())))))
		writeError(w, http.StatusTooManyRequests, "rate_limit_error", "this gateway key has used up its requests for now")
		return
	}
	// The cap is checked before the call and the cost is only known after
	// it, so a caller can end the day over the cap by the calls that were
	// already under way when it was reached.
	if spent, resets := g.spend.spentToday(name); spent >= g.dailyCap {
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(resets.Seconds())))))
		writeError(w, http.StatusTooManyRequests, "rate_limit_error", "this gateway key has reached its daily spending cap")
		return
	}

	// MaxBytesReader is given the real writer, not the wrapper: on a body
	// that is too big it tells that writer to close the connection, so the
	// gateway does not go on reading the rest of the oversized body.
	body, err := io.ReadAll(http.MaxBytesReader(noted.ResponseWriter, r.Body, maxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "the request body is larger than the gateway accepts")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request_error", "the gateway could not read the request body")
		return
	}

	ttl, wantsCache, err := cacheTTL(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var key [32]byte
	if wantsCache {
		key = cacheKey(name, r, body)
		if kept, ok := g.cache.get(key); ok {
			c.cache = "hit"
			copyHeaders(w.Header(), kept.header)
			w.Header().Set(cacheResult, "hit")
			http.NewResponseController(w).SetWriteDeadline(time.Now().Add(g.writeTimeout))
			w.Write(kept.body)
			return
		}
		c.cache = "miss"
		w.Header().Set(cacheResult, "miss")
	}

	// The upstream call gets a context of its own, which the gateway cancels.
	// Until an answer starts, the caller hanging up cancels it at once, so no
	// one waits or retries on behalf of a caller who has left.
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	stop := context.AfterFunc(r.Context(), cancel)

	sent := time.Now()
	resp, err := g.send(ctx, r, body, c)
	c.upstream = time.Since(sent)
	if err != nil {
		if ctx.Err() != nil {
			return // the caller hung up, so there is no one to answer
		}
		log.Printf("upstream call failed: %v", err)
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			writeError(w, http.StatusGatewayTimeout, "timeout_error", "the upstream took too long to answer")
			return
		}
		writeError(w, http.StatusBadGateway, "api_error", "the gateway could not reach the upstream")
		return
	}
	defer resp.Body.Close()

	// Once an answer has started, the caller hanging up no longer cancels
	// the call. The gateway reads the answer to the end, for up to
	// drainLimit, because the token counts it charges for come at the end.
	// A caller who leaves early pays for the whole answer.
	if stop() {
		stopDrain := context.AfterFunc(r.Context(), func() {
			time.AfterFunc(g.drainLimit, cancel)
			g.tell("caller gone")
		})
		defer stopDrain()
	}
	g.tell("answer started")

	// The answer says whether it is a stream. Going by the answer, not by
	// what the request asked for, means no wording of a request can get a
	// stream handled as a plain answer or the other way round.
	if resp.StatusCode == http.StatusOK && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		g.stream(w, resp, c, cancel)
		c.upstream = time.Since(sent)
		return
	}

	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	c.upstream = time.Since(sent)
	if err != nil || len(answer) > maxAnswer {
		if ctx.Err() != nil {
			log.Printf("caller=%s left and the answer did not finish in time, so it was not counted", name)
			return
		}
		log.Printf("reading the upstream answer failed: %v (%d bytes read)", err, len(answer))
		writeError(w, http.StatusBadGateway, "api_error", "the gateway could not read the upstream's answer")
		return
	}
	// A successful answer that cannot be counted is not passed on. Otherwise
	// any answer the gateway fails to understand would be free to the caller.
	if resp.StatusCode == http.StatusOK && !g.count(c, answer) {
		writeError(w, http.StatusBadGateway, "api_error", "the gateway could not read the token counts in the upstream's answer")
		return
	}

	// A caller that is connected but not reading must not hold the answer,
	// and this request, in memory for good.
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(g.writeTimeout))
	startAnswer(w, resp)
	// Only a successful, counted answer is kept. An error is never kept, so
	// a failure is not repeated to the next identical request.
	if wantsCache && resp.StatusCode == http.StatusOK {
		header := w.Header().Clone()
		header.Del(cacheResult)
		header.Del("Date") // a hit gets the time it is sent, not the time it was made
		g.cache.put(key, header, answer, ttl)
	}
	w.Write(answer)
}

func (g *gateway) tell(point string) {
	if g.trace != nil {
		g.trace(point)
	}
}

// startAnswer sends the upstream's status and headers to the caller.
func startAnswer(w http.ResponseWriter, resp *http.Response) {
	copyHeaders(w.Header(), resp.Header)
	// These describe the account behind the upstream key. Callers are not
	// shown them.
	for name := range w.Header() {
		if name == "Set-Cookie" || name == "Anthropic-Organization-Id" || name == "Anthropic-Workspace-Id" ||
			strings.HasPrefix(name, "Anthropic-Ratelimit-") {
			w.Header().Del(name)
		}
	}
	w.WriteHeader(resp.StatusCode)
}

// count reads the model and token counts out of a successful answer and adds
// their cost to the caller's total for the day. It returns false when the
// answer has no token counts to read.
func (g *gateway) count(c *call, answer []byte) bool {
	var message struct {
		Model string `json:"model"`
		Usage *usage `json:"usage"`
	}
	if err := json.Unmarshal(answer, &message); err != nil || message.Usage == nil {
		log.Printf("caller=%s answer has no usage to count", c.name)
		return false
	}
	g.record(c, message.Model, *message.Usage)
	return true
}

// record adds one call's cost to the caller's total for the day.
func (g *gateway) record(c *call, model string, u usage) {
	if _, known := priceFor(model); !known {
		log.Printf("no price for model %q, charging the highest list price", model)
	}
	c.model, c.prompt, c.output, c.micro = model, u.promptTokens(), u.OutputTokens, cost(model, u)
	if err := g.spend.add(c.name, u, c.micro); err != nil {
		log.Printf("saving the spend file failed: %v", err)
	}
}

// parseCap turns DAILY_CAP_USD, a number of dollars, into millionths of a
// dollar. It has to be at least one millionth and at most a million dollars.
func parseCap(dollars string) (int64, error) {
	value, err := strconv.ParseFloat(dollars, 64)
	// Written as "not in range" so that NaN, which fails every comparison,
	// is refused too.
	if err != nil || !(value >= 0.000001 && value <= 1e6) {
		return 0, errors.New("must be a number of dollars from 0.000001 to 1000000")
	}
	return int64(math.Round(value * 1e6)), nil
}

// attempt makes one upstream call with the caller's request.
func (g *gateway) attempt(ctx context.Context, r *http.Request, body []byte) (*http.Response, error) {
	target := g.upstream + "/v1/messages"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyHeaders(req.Header, r.Header)
	// The caller's gateway key stops here. Only the gateway holds the upstream
	// key, and the caller does not get to pick the workspace it is used in.
	for _, name := range []string{"X-Api-Key", "Cookie", "Anthropic-Workspace-Id", cacheHeader} {
		req.Header.Del(name)
	}
	req.Header.Set("Authorization", "Bearer "+g.apiKey)
	// With no Accept-Encoding of its own, Go's client asks for gzip and
	// unpacks the answer, so the body copied back is always plain.
	req.Header.Del("Accept-Encoding")
	// The body is already in memory, so there is nothing to wait for.
	req.Header.Del("Expect")

	return g.client.Do(req)
}

// copyHeaders copies every header from src to dst except the hop-by-hop ones.
func copyHeaders(dst, src http.Header) {
	skip := map[string]bool{}
	for _, name := range hopHeaders {
		skip[name] = true
	}
	// The Connection header can name more headers that belong to this hop.
	for _, value := range src.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			skip[http.CanonicalHeaderKey(strings.TrimSpace(name))] = true
		}
	}
	for name, values := range src {
		if skip[name] {
			continue
		}
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

// writeError answers in the same JSON shape the Messages API uses for errors,
// so a caller's error handling works whether the gateway or the upstream failed.
func writeError(w http.ResponseWriter, status int, kind, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": kind, "message": message},
	})
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func main() {
	g := &gateway{
		upstream: strings.TrimRight(env("UPSTREAM_URL", "https://api.anthropic.com"), "/"),
		apiKey:   os.Getenv("ANTHROPIC_API_KEY"),
		sleep:    sleep,

		writeTimeout: 30 * time.Second,
		drainLimit:   5 * time.Minute,
		cache:        newCache(),
		stats:        newStats(),
	}
	if g.apiKey == "" {
		log.Fatal("ANTHROPIC_API_KEY is not set")
	}
	if u, err := url.Parse(g.upstream); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		log.Fatal("UPSTREAM_URL must be an http or https address")
	}
	timeout, err := time.ParseDuration(env("UPSTREAM_TIMEOUT", "60s"))
	if err != nil || timeout <= 0 {
		log.Fatal("UPSTREAM_TIMEOUT must be a length of time above 0, like 60s or 2m")
	}
	g.client = newClient(timeout)
	g.maxRetries, err = strconv.Atoi(env("MAX_RETRIES", "2"))
	if err != nil || g.maxRetries < 0 || g.maxRetries > 10 {
		log.Fatal("MAX_RETRIES must be a whole number from 0 to 10")
	}
	keys, err := parseKeys(os.Getenv("GATEWAY_KEYS"))
	if err != nil {
		log.Fatalf("GATEWAY_KEYS: %v", err)
	}
	perMinute, err := strconv.Atoi(env("RATE_LIMIT_PER_MINUTE", "60"))
	if err != nil || perMinute < 1 {
		log.Fatal("RATE_LIMIT_PER_MINUTE must be a whole number above 0")
	}
	g.keys, g.limiter = keys, newLimiter(perMinute)
	if admin := os.Getenv("ADMIN_KEY"); admin != "" {
		g.adminKey, g.hasAdmin = sha256.Sum256([]byte(admin)), true
		if len(admin) < minKeyLength || keys[g.adminKey] != "" {
			log.Fatalf("ADMIN_KEY must be at least %d characters and must not be one of the gateway keys", minKeyLength)
		}
	}
	server := &http.Server{
		Addr:              env("ADDR", "127.0.0.1:8080"),
		Handler:           g.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if g.dailyCap, err = parseCap(env("DAILY_CAP_USD", "1")); err != nil {
		log.Fatalf("DAILY_CAP_USD %v", err)
	}
	if g.spend, err = openSpend(env("SPEND_FILE", "spend.json")); err != nil {
		log.Fatalf("SPEND_FILE: %v", err)
	}
	log.Printf("listening on %s, forwarding to %s", server.Addr, g.upstream)
	log.Fatal(server.ListenAndServe())
}
