# llm-gateway

A small service that sits between an app and the Anthropic API. The app sends its model calls here instead of straight to the API.

What works so far:

- It takes a `POST /v1/messages` request, sends it to the API with its own key, and returns the answer.
- Each caller has a gateway key. A request without a known key gets a 401, and the API key never leaves the gateway.
- Each key has a rate limit, 60 requests a minute unless you change it. A caller over the limit gets a 429 with a `Retry-After` header.
- When the API is overloaded, rate limited, erroring or unreachable, the gateway tries again, twice by default, waiting longer each time. If the API has not started answering after 60 seconds, that try counts as failed.
- After each answer it reads the token counts, works out what the call cost, and adds that to the caller's total for the day. A caller that reaches its daily dollar cap gets a 429 until the next day starts.

Still to come:

- streaming. For now a request with `"stream": true` gets a 400.
- a cache for identical repeated requests
- a load test against a fake upstream, with the numbers published here

It is written in Go with the standard library. The first caller will be [fineprint](https://github.com/s6hib/fineprint).

## Try it

You need Go 1.27.4 or newer. The tests and the commands below use a fake upstream, so they need no API key and call nothing outside your machine.

```sh
go test ./...

go run ./cmd/fakeupstream          # stands in for the API on 127.0.0.1:9090
ANTHROPIC_API_KEY=anything GATEWAY_KEYS=demo:demo-key-0123456789 \
  UPSTREAM_URL=http://127.0.0.1:9090 go run .

curl -s http://127.0.0.1:8080/v1/messages \
  -H 'x-api-key: demo-key-0123456789' \
  -H 'content-type: application/json' \
  -d '{"model":"claude-haiku-5-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}'
```

## Settings

All settings are environment variables.

| Variable | What it does | Default |
|---|---|---|
| `ANTHROPIC_API_KEY` | The key the gateway sends upstream | required |
| `GATEWAY_KEYS` | Who may call, as `name:key,name:key`. A key is at least 16 characters, for example from `openssl rand -hex 32` | required |
| `RATE_LIMIT_PER_MINUTE` | Requests a minute for each gateway key | `60` |
| `UPSTREAM_TIMEOUT` | How long to wait for the API to start answering, like `60s` or `2m` | `60s` |
| `MAX_RETRIES` | Extra tries after a failed call, from 0 to 10 | `2` |
| `DAILY_CAP_USD` | Dollars each gateway key may spend per day. Days are UTC | `1` |
| `SPEND_FILE` | The file the daily totals are kept in | `spend.json` |
| `UPSTREAM_URL` | Where requests are forwarded | `https://api.anthropic.com` |
| `ADDR` | Where the gateway listens | `127.0.0.1:8080` |

A caller sends its gateway key the way it would send an API key, in `x-api-key` or `Authorization: Bearer`. The rate limit is counted in memory, so it starts over when the gateway restarts.

## Cost and the daily cap

The gateway takes `model` and `usage` from each successful answer and multiplies the token counts by Anthropic's list prices. The price table is in `cost.go` and was read from the [pricing page](https://platform.claude.com/docs/en/about-claude/pricing) on 2026-11-13. It covers Claude Fable 5.2, Opus 5.5, Sonnet 5.5 and Haiku 5.5. A model that is not in the table is charged at the highest price in it, so a total can come out too high but not too low.

The cost counts input and output tokens, prompt-cache reads and writes, the higher price for Haiku 5.5 prompts over 100,000 tokens, fast mode, US-only inference and web searches. It does not count batch discounts or other tool charges. It is an estimate; the invoice from Anthropic is the real number.

Totals are kept per key and per day in `SPEND_FILE`, in millionths of a dollar:

```json
{
  "2026-11-14": {
    "demo": {
      "requests": 3,
      "input_tokens": 36,
      "output_tokens": 18,
      "cost_micro_usd": 12
    }
  }
}
```

The file is read at startup, so a restart does not reset the totals. The cap is checked before a call is sent and the cost is known only after it, so a key can finish the day over its cap by the calls that were already running when it got there. Failed calls are not counted. If a successful answer has no token counts the gateway can read, the caller gets a 502 in its place, so an answer is never handed over uncounted.

## Retries

The gateway retries a call when the API answers 429, 500, 502, 503, 504 or 529, or when the connection fails or times out. Other errors, such as a 400 for a bad request, go straight back to the caller.

The wait before the first retry is between 0.25 and 0.5 seconds. It doubles with each retry, up to 8 seconds. When the API sends a `Retry-After` header that asks for longer, the gateway waits that long, unless it is more than 10 seconds, in which case the caller gets the error and the header. When the retries run out, the caller gets the API's last answer, or a 504 or 502 from the gateway if there was none.

A retry sends the same request again. If a call fails after the API has started work on it, that work can be billed twice.

The Anthropic SDKs also retry by themselves. Set `max_retries` to 0 in a client that calls the gateway, or the tries multiply: with 2 retries on each side, one failing request is sent up to 9 times.

To see retries happen, make the fake upstream fail some of the time:

```sh
go run ./cmd/fakeupstream -error-rate 0.5
```
