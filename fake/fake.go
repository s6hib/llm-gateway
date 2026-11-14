// Package fake is a stand-in for the model API. It answers POST /v1/messages
// in the Messages API's JSON shape after a set delay and never calls a model,
// so tests and load tests need no key and cost nothing.
package fake

import (
	"encoding/json"
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
			Model string `json:"model"`
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

func writeError(w http.ResponseWriter, status int, kind, message string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":       "error",
		"error":      map[string]string{"type": kind, "message": message},
		"request_id": "req_fake",
	})
}
