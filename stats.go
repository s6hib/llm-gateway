package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"
)

// call is what the gateway notes about one request as it goes through, for
// the log line and for /stats.
type call struct {
	name     string        // the caller, or "" when the key was missing or unknown
	start    time.Time     // when the request arrived
	upstream time.Duration // time spent on the upstream: tries, waits between them, reading the answer
	tries    int           // upstream calls made
	cache    string        // "hit", "miss", or "" when caching was not asked for
	model    string
	prompt   int64 // tokens in
	output   int64 // tokens out
	micro    int64 // cost, in millionths of a dollar
}

// statusWriter remembers the status code a handler sends.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK // a Write with no WriteHeader sends 200
	}
	return w.ResponseWriter.Write(p)
}

// Unwrap lets Go find the real writer underneath, which is the one that can
// flush and set deadlines.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// clientClosed is recorded when the caller left before anything was sent
// back. It is not a real HTTP status; nginx uses the same number for this.
const clientClosed = 499

// finish writes one log line for a request and adds it to the stats.
func (g *gateway) finish(c *call, status int) {
	if status == 0 {
		status = clientClosed
	}
	total := time.Since(c.start)
	g.stats.add(c, status, total)
	slog.Info("request",
		"caller", c.name, "status", status,
		"ms", total.Milliseconds(), "upstream_ms", c.upstream.Milliseconds(), "tries", c.tries,
		"cache", c.cache, "model", c.model,
		"prompt_tokens", c.prompt, "output_tokens", c.output, "cost_usd", strconv.FormatFloat(float64(c.micro)/1e6, 'f', 6, 64))
}

// keepLast is how many recent requests per caller the latency figures cover.
const keepLast = 1000

// stats counts requests in memory since the gateway started. A restart
// clears it; the spend file is what survives.
type stats struct {
	mu      sync.Mutex
	started time.Time
	callers map[string]*callerStats
}

type callerStats struct {
	requests  int64
	byStatus  map[int]int64
	cacheHits int64
	retries   int64
	total     recent // how long each request took, start to finish
	overhead  recent // the part of that which was not spent on the upstream
}

func newStats() *stats {
	return &stats{started: time.Now(), callers: map[string]*callerStats{}}
}

func (s *stats) add(c *call, status int, total time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := c.name
	if name == "" {
		name = "(no key)"
	}
	cs := s.callers[name]
	if cs == nil {
		cs = &callerStats{byStatus: map[int]int64{}}
		s.callers[name] = cs
	}
	cs.requests++
	cs.byStatus[status]++
	if c.cache == "hit" {
		cs.cacheHits++
	}
	cs.retries += int64(max(0, c.tries-1))
	cs.total.add(total)
	cs.overhead.add(max(0, total-c.upstream))
}

// recent holds the last keepLast durations. Once full, each new one
// overwrites the oldest, so memory stays fixed however long the gateway runs.
type recent struct {
	values []time.Duration
	next   int // where the next one goes once the slice is full
}

func (r *recent) add(d time.Duration) {
	if len(r.values) < keepLast {
		r.values = append(r.values, d)
		return
	}
	r.values[r.next] = d
	r.next = (r.next + 1) % keepLast
}

// spread sums up a set of durations as three percentiles. p95 is the value
// that 95 of every 100 requests were at or under.
type spread struct {
	Count int     `json:"count"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
	P99   float64 `json:"p99"`
}

func (r *recent) spread() spread {
	sorted := slices.Clone(r.values)
	slices.Sort(sorted)
	at := func(p float64) float64 {
		if len(sorted) == 0 {
			return 0
		}
		// Nearest rank: the smallest value with at least p of the values at or under it.
		rank := max(1, int(math.Ceil(p*float64(len(sorted)))))
		return math.Round(float64(sorted[rank-1].Microseconds())/10) / 100 // milliseconds, 2 decimals
	}
	return spread{Count: len(sorted), P50: at(0.50), P95: at(0.95), P99: at(0.99)}
}

type callerReport struct {
	Requests   int64            `json:"requests"`
	ByStatus   map[string]int64 `json:"by_status"`
	CacheHits  int64            `json:"cache_hits"`
	Retries    int64            `json:"upstream_retries"`
	LatencyMs  spread           `json:"latency_ms"`
	OverheadMs spread           `json:"gateway_overhead_ms"`
	Today      todayReport      `json:"today"`
}

type todayReport struct {
	Requests     int64   `json:"counted_requests"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// statsPage answers GET /stats with the counts for every caller. It needs the
// admin key. Without ADMIN_KEY set there is no such page.
func (g *gateway) statsPage(w http.ResponseWriter, r *http.Request) {
	if !g.hasAdmin {
		writeError(w, http.StatusNotFound, "not_found_error", "the gateway only serves POST /v1/messages")
		return
	}
	given := sha256.Sum256([]byte(presentedKey(r)))
	if subtle.ConstantTimeCompare(given[:], g.adminKey[:]) != 1 {
		writeError(w, http.StatusUnauthorized, "authentication_error", "missing or wrong admin key")
		return
	}

	spent := g.spend.today()
	g.stats.mu.Lock()
	callers := map[string]callerReport{}
	for name, cs := range g.stats.callers {
		byStatus := map[string]int64{}
		for status, n := range cs.byStatus {
			byStatus[strconv.Itoa(status)] = n
		}
		callers[name] = callerReport{
			Requests: cs.requests, ByStatus: byStatus, CacheHits: cs.cacheHits, Retries: cs.retries,
			LatencyMs: cs.total.spread(), OverheadMs: cs.overhead.spread(),
		}
	}
	started := g.stats.started
	g.stats.mu.Unlock()

	// A caller can have spend today and no requests since the last restart.
	for name, t := range spent {
		report := callers[name]
		report.Today = todayReport{Requests: t.Requests, InputTokens: t.InputTokens, OutputTokens: t.OutputTokens,
			CostUSD: float64(t.CostMicroUSD) / 1e6}
		if report.ByStatus == nil {
			report.ByStatus = map[string]int64{}
		}
		callers[name] = report
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"since":          started.UTC().Format(time.RFC3339),
		"uptime_seconds": int64(time.Since(started).Seconds()),
		"latency_covers": "the last " + strconv.Itoa(keepLast) + " requests per caller",
		"callers":        callers,
	})
}

// health answers GET /healthz, for whatever is checking that the gateway is up.
func health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}` + "\n"))
}
