# llm-gateway

A small service that sits between an app and the Anthropic API. The app sends its model calls here instead of straight to the API.

What works so far:

- It takes a `POST /v1/messages` request, sends it to the API with its own key, and returns the answer.
- Each caller has a gateway key. A request without a known key gets a 401, and the API key never leaves the gateway.
- Each key has a rate limit, 60 requests a minute unless you change it. A caller over the limit gets a 429 with a `Retry-After` header.
- When the API is overloaded, rate limited, erroring or unreachable, the gateway tries again, twice by default, waiting longer each time. If the API has not started answering after 60 seconds, that try counts as failed.

Still to come:

- token and cost tracking per key, with a daily dollar cap
- streaming
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
| `UPSTREAM_URL` | Where requests are forwarded | `https://api.anthropic.com` |
| `ADDR` | Where the gateway listens | `127.0.0.1:8080` |

A caller sends its gateway key the way it would send an API key, in `x-api-key` or `Authorization: Bearer`. The rate limit is counted in memory, so it starts over when the gateway restarts.

## Retries

The gateway retries a call when the API answers 429, 500, 502, 503, 504 or 529, or when the connection fails or times out. Other errors, such as a 400 for a bad request, go straight back to the caller.

The wait before the first retry is between 0.25 and 0.5 seconds. It doubles with each retry, up to 8 seconds. When the API sends a `Retry-After` header that asks for longer, the gateway waits that long, unless it is more than 10 seconds, in which case the caller gets the error and the header. When the retries run out, the caller gets the API's last answer, or a 504 or 502 from the gateway if there was none.

A retry sends the same request again. If a call fails after the API has started work on it, that work can be billed twice.

The Anthropic SDKs also retry by themselves. Set `max_retries` to 0 in a client that calls the gateway, or the tries multiply: with 2 retries on each side, one failing request is sent up to 9 times.

To see retries happen, make the fake upstream fail some of the time:

```sh
go run ./cmd/fakeupstream -error-rate 0.5
```
