# llm-gateway

A small service that sits between an app and the Anthropic API. The app sends its model calls here instead of straight to the API.

So far it does one thing: it takes a `POST /v1/messages` request, sends it to the API with its own key, and returns the answer unchanged. The rest is the plan:

- one key per caller, with a rate limit on each key
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
ANTHROPIC_API_KEY=anything UPSTREAM_URL=http://127.0.0.1:9090 go run .

curl -s http://127.0.0.1:8080/v1/messages \
  -H 'content-type: application/json' \
  -d '{"model":"claude-haiku-5-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}'
```

Without `UPSTREAM_URL` the gateway forwards to `https://api.anthropic.com`, and `ANTHROPIC_API_KEY` has to be a real key. Callers don't have keys of their own yet, so anyone who can reach the gateway's port can spend that key. By default it listens on 127.0.0.1 only.
