// llm-gateway forwards Messages API requests to an upstream model API.
//
// Settings come from the environment:
//
//	ANTHROPIC_API_KEY      the upstream key (required)
//	GATEWAY_KEYS           who may call, as name:key,name:key (required)
//	RATE_LIMIT_PER_MINUTE  requests a minute allowed per caller (default 60)
//	UPSTREAM_TIMEOUT       how long to wait for the upstream to start answering (default 60s)
//	MAX_RETRIES            extra tries after a failed upstream call (default 2)
//	UPSTREAM_URL           where requests are forwarded (default https://api.anthropic.com)
//	ADDR                   where the gateway listens (default 127.0.0.1:8080)
package main

import (
	"bytes"
	"context"
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
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found_error", "the gateway only serves POST /v1/messages")
	})
	return mux
}

// messages sends the caller's request upstream and copies the answer back.
func (g *gateway) messages(w http.ResponseWriter, r *http.Request) {
	name, ok := g.caller(r)
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

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "the request body is larger than the gateway accepts")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request_error", "the gateway could not read the request body")
		return
	}

	resp, err := g.send(r, body)
	if err != nil {
		if r.Context().Err() != nil {
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
	io.Copy(w, resp.Body)
}

// attempt makes one upstream call with the caller's request.
func (g *gateway) attempt(r *http.Request, body []byte) (*http.Response, error) {
	target := g.upstream + "/v1/messages"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	// r.Context() is cancelled when the caller hangs up, which cancels the
	// upstream call too.
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyHeaders(req.Header, r.Header)
	// The caller's gateway key stops here. Only the gateway holds the upstream
	// key, and the caller does not get to pick the workspace it is used in.
	for _, name := range []string{"X-Api-Key", "Cookie", "Anthropic-Workspace-Id"} {
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
	server := &http.Server{
		Addr:              env("ADDR", "127.0.0.1:8080"),
		Handler:           g.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("listening on %s, forwarding to %s", server.Addr, g.upstream)
	log.Fatal(server.ListenAndServe())
}
