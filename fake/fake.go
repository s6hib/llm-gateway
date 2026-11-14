// Package fake is a stand-in for the model API. It answers POST /v1/messages
// in the Messages API's JSON shape after a set delay and never calls a model,
// so tests and load tests need no key and cost nothing. A request with
// "stream": true gets the same reply as a stream of events.
package fake

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"time"
)

// Handler returns a server that waits for delay and then sends a canned reply.
// errorRate is the share of requests, from 0 to 1, that get a 529
// overloaded_error in place of the reply, for testing how a caller copes.
func Handler(delay time.Duration, errorRate float64) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-Id", "req_fake")

		if r.Header.Get("Authorization") == "" && r.Header.Get("X-Api-Key") == "" {
			writeError(w, http.StatusUnauthorized, "authentication_error", "no API key was sent")
			return
		}
		var request struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model == "" {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "the body must be JSON with a model")
			return
		}

		select {
		case <-time.After(delay):
		case <-r.Context().Done(): // the caller hung up
			return
		}

		if rand.Float64() < errorRate {
			writeError(w, 529, "overloaded_error", "the fake upstream is pretending to be overloaded")
			return
		}

		if request.Stream {
			stream(w, r, request.Model)
			return
		}

		json.NewEncoder(w).Encode(map[string]any{
			"id":            "msg_fake",
			"type":          "message",
			"role":          "assistant",
			"content":       []map[string]string{{"type": "text", "text": "Hello from the fake upstream."}},
			"model":         request.Model,
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
			"usage":         map[string]int{"input_tokens": 12, "output_tokens": 6},
		})
	})
	return mux
}

// pieceGap is the pause between the pieces of a streamed reply.
const pieceGap = 20 * time.Millisecond

// stream sends the canned reply as server-sent events, a piece at a time,
// with the same token counts as the plain reply.
func stream(w http.ResponseWriter, r *http.Request, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	send := func(event string, data any) {
		payload, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
		http.NewResponseController(w).Flush()
	}
	object := func(pairs ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i < len(pairs); i += 2 {
			m[pairs[i].(string)] = pairs[i+1]
		}
		return m
	}

	send("message_start", object("type", "message_start", "message", object(
		"id", "msg_fake", "type", "message", "role", "assistant", "content", []any{}, "model", model,
		"stop_reason", nil, "stop_sequence", nil,
		"usage", object("input_tokens", 12, "output_tokens", 1))))
	send("content_block_start", object("type", "content_block_start", "index", 0,
		"content_block", object("type", "text", "text", "")))
	send("ping", object("type", "ping"))
	for _, piece := range []string{"Hello ", "from ", "the ", "fake ", "upstream."} {
		select {
		case <-time.After(pieceGap):
		case <-r.Context().Done(): // the caller hung up
			return
		}
		send("content_block_delta", object("type", "content_block_delta", "index", 0,
			"delta", object("type", "text_delta", "text", piece)))
	}
	send("content_block_stop", object("type", "content_block_stop", "index", 0))
	send("message_delta", object("type", "message_delta",
		"delta", object("stop_reason", "end_turn", "stop_sequence", nil),
		"usage", object("output_tokens", 6)))
	send("message_stop", object("type", "message_stop"))
}

func writeError(w http.ResponseWriter, status int, kind, message string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":       "error",
		"error":      map[string]string{"type": kind, "message": message},
		"request_id": "req_fake",
	})
}
