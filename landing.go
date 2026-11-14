package main

import "net/http"

// landingPage is what a person sees when they open the gateway's address in a
// browser. The gateway is an API, so there is nothing here to use.
const landingPage = `<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>llm-gateway</title>
<h1>llm-gateway</h1>
<p>A small service that sits between an app and the Anthropic API. This is a running copy of it.</p>
<p>It is an API, not a website. Programs send it requests, and every request needs a key, so there is nothing to use on this page.</p>
<p>What it does and how it works: <a href="https://github.com/s6hib/llm-gateway">github.com/s6hib/llm-gateway</a></p>
<p>Is it up: <a href="/healthz">/healthz</a></p>
`

// landing answers GET / with the page above.
func landing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(landingPage))
}
