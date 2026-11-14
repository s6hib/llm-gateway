# llm-gateway

A small service that sits between an app and the Anthropic API. The app sends its model calls here instead of straight to the API.

What works so far:

- It takes a `POST /v1/messages` request, sends it to the API with its own key, and returns the answer.
- Each caller has a gateway key. A request without a known key gets a 401, and the API key never leaves the gateway.
- Each key has a rate limit, 60 requests a minute unless you change it. A caller over the limit gets a 429 with a `Retry-After` header.
- When the API is overloaded, rate limited, erroring or unreachable, the gateway tries again, twice by default, waiting longer each time. If the API has not started answering after 60 seconds, that try counts as failed.
- After each answer it reads the token counts, works out what the call cost, and adds that to the caller's total for the day. A caller that reaches its daily dollar cap gets a 429 until the next day starts.
- A request with `"stream": true` is streamed: each piece of the answer goes to the caller as it arrives, and the token counts are read out of the stream as it passes.
- A request can ask for its answer to be cached. An identical request from the same caller is then answered from memory, without a call to the API.
- It writes one log line per request and keeps running counts per caller, which `GET /stats` returns.

Still to come:

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
| `ADMIN_KEY` | The key that may read `GET /stats`. At least 16 characters, and not one of the gateway keys. Leave it unset and there is no stats page | none |
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

The file is read at startup, so a restart does not reset the totals. The cap is checked before a call is sent and the cost is known only after it, so a key can finish the day over its cap by the calls that were already running when it got there. Failed calls are not counted. If a successful plain answer has no token counts the gateway can read, the caller gets a 502 in its place, so an answer is never handed over uncounted.

## Cache

The cache is off unless a request asks for it, because a model can give a different answer to the same prompt each time, and a caller that sends one prompt several times to compare the answers needs fresh ones. To ask, send the header `X-Cache-Ttl` with the number of seconds to keep the answer, from 1 to 3600:

```sh
curl -si http://127.0.0.1:8080/v1/messages \
  -H 'x-api-key: demo-key-0123456789' \
  -H 'x-cache-ttl: 60' \
  -H 'content-type: application/json' \
  -d '{"model":"claude-haiku-5-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}'
```

The answer comes back with `X-Cache: miss` the first time and `X-Cache: hit` after that, until the time runs out. A hit costs nothing and is not added to the caller's spend. It still uses one request of the rate limit.

Two requests share an answer only when the caller, the body, the query string and the `anthropic-version`, `anthropic-beta` and `content-type` headers are all the same, byte for byte. Each caller has its own entries, so one caller never gets an answer made for another. Only successful plain answers are kept. Errors and streamed answers are not.

The cache holds up to 1,000 answers of up to 1 MB each, in memory. A restart empties it.

This is separate from Anthropic's prompt caching. Prompt caching is done by the API and makes the repeated part of a prompt cheaper. The `cache_control` fields that turn it on are part of the request body, which the gateway passes through unchanged.

## Streaming

A streamed answer is a series of events. The gateway passes them on unchanged and reads two kinds as they go by: `message_start`, which names the model and the input tokens, and `message_delta`, which carries the running token counts. When the stream ends, the last counts it saw are what the call is charged for.

The final counts come at the end of a stream, and they include things the caller never sees go by, such as thinking tokens. So if the caller hangs up partway, or stops reading, the gateway stops sending but keeps reading the stream to its end, for up to 5 minutes, and charges for the whole answer. The same goes for a plain answer that has already started.

If a stream breaks off without its final counts, the gateway estimates the output from the amount of answer that did go by, at 3 bytes a token.

The timeout is on the wait for the stream to start. Once it has started, the gateway does not cut it off.

To see a stream, start the fake upstream and the gateway as above and add `"stream": true` to the request:

```sh
curl -sN http://127.0.0.1:8080/v1/messages \
  -H 'x-api-key: demo-key-0123456789' \
  -H 'content-type: application/json' \
  -d '{"model":"claude-haiku-5-5","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}'
```

## Logs and stats

Every request to `/v1/messages` writes one line when it ends:

```
INFO request caller=demo status=200 ms=216 upstream_ms=214 tries=1 cache=miss model=claude-haiku-5-5 prompt_tokens=12 output_tokens=6 cost_usd=0.000004
```

`ms` is the whole request and `upstream_ms` is the part spent on the API, including retries and the waits between them. The difference is the gateway's own share: everything that was not waiting on the API, which also includes reading the request from the caller. Status 499 means the caller left before the gateway sent anything back.

`GET /stats` returns counts for each caller since the gateway started: requests, a count per status code, cache hits, retries, and p50, p95 and p99 over the last 1,000 requests for both the total time and the gateway's own share of it. It also has each caller's tokens and cost for the day so far. It needs the admin key:

```sh
curl -s http://127.0.0.1:8080/stats -H 'x-api-key: YOUR_ADMIN_KEY'
```

The counts are kept in memory and start again from zero when the gateway restarts. The day's spend comes from the spend file, so it does not.

`GET /healthz` answers 200 with no key, for whatever checks that the gateway is up.

## Retries

The gateway retries a call when the API answers 429, 500, 502, 503, 504 or 529, or when the connection fails or times out. Other errors, such as a 400 for a bad request, go straight back to the caller.

The wait before the first retry is between 0.25 and 0.5 seconds. It doubles with each retry, up to 8 seconds. When the API sends a `Retry-After` header that asks for longer, the gateway waits that long, unless it is more than 10 seconds, in which case the caller gets the error and the header. When the retries run out, the caller gets the API's last answer, or a 504 or 502 from the gateway if there was none.

A retry sends the same request again. If a call fails after the API has started work on it, that work can be billed twice.

The Anthropic SDKs also retry by themselves. Set `max_retries` to 0 in a client that calls the gateway, or the tries multiply: with 2 retries on each side, one failing request is sent up to 9 times.

To see retries happen, make the fake upstream fail some of the time:

```sh
go run ./cmd/fakeupstream -error-rate 0.5
```
