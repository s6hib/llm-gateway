// fakeupstream runs the fake model API as its own server, for trying the
// gateway by hand and for load tests.
//
//	go run ./cmd/fakeupstream -addr 127.0.0.1:9090 -delay 200ms -error-rate 0.2
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/s6hib/llm-gateway/fake"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9090", "address to listen on")
	delay := flag.Duration("delay", 200*time.Millisecond, "how long to wait before each reply")
	errorRate := flag.Float64("error-rate", 0, "share of requests, from 0 to 1, answered with a 529 error")
	flag.Parse()

	log.Printf("fake upstream on %s, delay %s, error rate %g", *addr, *delay, *errorRate)
	log.Fatal(http.ListenAndServe(*addr, fake.Handler(*delay, *errorRate)))
}
