# llm-gateway

A small service that sits between an app and the Anthropic API. The app sends its model calls here instead of straight to the API.

What works so far:

- It takes a `POST /v1/messages` request, sends it to the API with its own key, and returns the answer.
- Each caller has a gateway key. A request without a known key gets a 401, and the API key never leaves the gateway.
- Each key has a rate limit, 60 requests a minute unless you change it. A caller over the limit gets a 429 with a `Retry-After` header.

Still to come:

- timeouts and retries when the upstream fails
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
| `UPSTREAM_URL` | Where requests are forwarded | `https://api.anthropic.com` |
| `ADDR` | Where the gateway listens | `127.0.0.1:8080` |

A caller sends its gateway key the way it would send an API key, in `x-api-key` or `Authorization: Bearer`. The rate limit is counted in memory, so it starts over when the gateway restarts.
