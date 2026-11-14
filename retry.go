package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// retryStatus lists the upstream answers worth another try: the upstream was
// busy or broke, and the same request may well work a moment later. Any other
// error, such as a 400 for a bad request, would fail the same way again.
var retryStatus = map[int]bool{429: true, 500: true, 502: true, 503: true, 504: true, 529: true}

const (
	baseBackoff = 500 * time.Millisecond // the wait before the first retry, at most
	maxBackoff  = 8 * time.Second        // waits stop doubling here
	// maxRetryAfter is the longest wait the gateway will sit through when the
	// upstream names one. Past this the caller gets the error and decides.
	maxRetryAfter = 10 * time.Second
)

// send makes the upstream call and tries again, up to maxRetries more times,
// when the call fails in a way that may pass. A retry sends the same request
// again, so a call that died after the upstream began work can be paid for twice.
func (g *gateway) send(r *http.Request, body []byte) (*http.Response, error) {
	for try := 0; ; try++ {
		resp, err := g.attempt(r, body)
		if err != nil && r.Context().Err() != nil {
			return nil, err // the caller hung up
		}
		if try == g.maxRetries || (err == nil && !retryStatus[resp.StatusCode]) {
			return resp, err
		}

		wait := backoff(try)
		reason := fmt.Sprint(err)
		if err == nil {
			reason = fmt.Sprintf("status %d", resp.StatusCode)
			// The upstream may ask for a longer wait, never a shorter one.
			if after, ok := retryAfter(resp.Header); ok {
				wait = max(wait, after)
			}
			if wait > maxRetryAfter {
				return resp, nil
			}
			// Reading the rest of the failed answer lets Go reuse the connection.
			io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
		}
		log.Printf("upstream try %d failed (%s), trying again in %s", try+1, reason, wait.Round(time.Millisecond))
		if err := g.sleep(r.Context(), wait); err != nil {
			return nil, err
		}
	}
}

// backoff is how long to wait before retry number try (0 is the first). The
// wait doubles each time up to maxBackoff. The real wait is a random point in
// the top half of that, so callers that failed together do not all come back
// at the same instant.
func backoff(try int) time.Duration {
	wait := min(maxBackoff, baseBackoff<<try)
	return wait/2 + rand.N(wait/2+1)
}

// retryAfter reads the upstream's Retry-After header, a number of seconds.
func retryAfter(h http.Header) (time.Duration, bool) {
	seconds, err := strconv.Atoi(h.Get("Retry-After"))
	if err != nil || seconds < 0 {
		return 0, false
	}
	// Anything past an hour is treated as an hour, far over maxRetryAfter
	// already, so a huge number cannot overflow the multiplication below.
	return time.Duration(min(seconds, 3600)) * time.Second, true
}

// sleep waits for d, or returns early with an error when ctx is cancelled.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
