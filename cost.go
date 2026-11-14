package main

import (
	"math"
	"strings"
)

// price is what one model costs, in dollars per million tokens.
type price struct {
	input, output float64
	cacheRead     float64 // tokens read from the prompt cache cost this share of the input price
}

// prices are Anthropic's list prices, read from
// https://platform.claude.com/docs/en/about-claude/pricing on 2026-11-13.
// A model missing from this table is charged as the most expensive one, so
// the gateway's totals can run high for it but never low.
var prices = map[string]price{
	"claude-fable-5-2":  {input: 8, output: 40, cacheRead: 0.025},
	"claude-opus-5-5":   {input: 4, output: 20, cacheRead: 0.05},
	"claude-sonnet-5-5": {input: 2, output: 10, cacheRead: 0.05},
	"claude-haiku-5-5":  {input: 0.10, output: 0.50, cacheRead: 0.1},
}

const (
	cacheWrite5m = 1.25 // writing to the 5-minute cache costs 1.25x the input price
	cacheWrite1h = 2    // and to the 1-hour cache 2x

	// Claude Haiku 5.5 costs 5x as much, for the whole request, when the
	// prompt is over 100,000 tokens.
	haikuLongPrompt = 100_000
	haikuLongFactor = 5

	fastFactor     = 2      // usage.speed "fast"
	usGeoFactor    = 1.1    // usage.inference_geo "us"
	webSearchMicro = 10_000 // $10 per 1,000 web searches, in millionths of a dollar

	// maxCost is the most one call can be charged: a billion dollars. Nothing
	// real comes near it. It keeps an absurd count in an answer from
	// overflowing the total.
	maxCost = 1e15
)

// usage is the part of a Messages API answer that says what the call used.
type usage struct {
	InputTokens              int64 `json:"input_tokens"` // prompt tokens that did not touch the cache
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheCreation            *struct {
		Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	Speed         string `json:"speed"`
	InferenceGeo  string `json:"inference_geo"`
	ServerToolUse struct {
		WebSearchRequests int64 `json:"web_search_requests"`
	} `json:"server_tool_use"`
}

// promptTokens is the whole prompt: the three input counts never overlap.
func (u usage) promptTokens() int64 {
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

// priceFor finds a model's price. known is false when the model is not in the
// table and the most expensive price was used.
func priceFor(model string) (p price, known bool) {
	for id, p := range prices {
		// An id may carry a date after the name, like claude-haiku-5-5-20261001.
		if model == id || strings.HasPrefix(model, id+"-") {
			return p, true
		}
	}
	for _, candidate := range prices {
		if candidate.output > p.output {
			p = candidate
		}
	}
	return p, false
}

// cost estimates what a call cost, in millionths of a dollar. Money is kept
// as a whole number of these so that adding many small costs never drifts the
// way adding decimals does. Prices are dollars per million tokens, so tokens
// times price is already in millionths of a dollar.
//
// It is an estimate from list prices. It leaves out batch discounts and
// charges for tools other than web search.
func cost(model string, u usage) int64 {
	// The counts come from the upstream's answer. A negative one must not be
	// able to lower a caller's total.
	for _, count := range []*int64{&u.InputTokens, &u.OutputTokens, &u.CacheReadInputTokens,
		&u.CacheCreationInputTokens, &u.ServerToolUse.WebSearchRequests} {
		*count = max(0, *count)
	}
	p, _ := priceFor(model)
	if strings.HasPrefix(model, "claude-haiku-5-5") && u.promptTokens() > haikuLongPrompt {
		p.input *= haikuLongFactor
		p.output *= haikuLongFactor
	}

	// Without the split, cache writes are charged at the 5-minute price,
	// which is the API's default cache lifetime.
	write5m, write1h := u.CacheCreationInputTokens, int64(0)
	if u.CacheCreation != nil {
		write5m, write1h = max(0, u.CacheCreation.Ephemeral5m), max(0, u.CacheCreation.Ephemeral1h)
	}

	micro := float64(u.InputTokens)*p.input +
		float64(write5m)*p.input*cacheWrite5m +
		float64(write1h)*p.input*cacheWrite1h +
		float64(u.CacheReadInputTokens)*p.input*p.cacheRead +
		float64(u.OutputTokens)*p.output
	if u.Speed == "fast" {
		micro *= fastFactor
	}
	if u.InferenceGeo == "us" {
		micro *= usGeoFactor
	}
	micro += float64(u.ServerToolUse.WebSearchRequests) * webSearchMicro
	return int64(math.Round(min(micro, maxCost)))
}
