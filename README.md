# llm-gateway

A small service that sits between an app and the Anthropic API. The app sends its model calls here instead of straight to the API.

Nothing works yet. This is the plan:

- forward requests in the same shape as the Messages API, so a caller only changes the address and the key
- one key per caller, with a rate limit on each key
- timeouts and retries when the upstream fails
- token and cost tracking per key, with a daily dollar cap
- streaming
- a cache for identical repeated requests
- a load test against a fake upstream, with the numbers published here

It will be written in Go with the standard library. The first caller will be [fineprint](https://github.com/s6hib/fineprint).
