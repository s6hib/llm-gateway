// fakeupstream runs the fake model API as its own server, for trying the
// gateway by hand and for load tests.
//
//	go run ./cmd/fakeupstream -addr 127.0.0.1:9090 -delay 200ms
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
	flag.Parse()

	log.Printf("fake upstream on %s, delay %s", *addr, *delay)
	log.Fatal(http.ListenAndServe(*addr, fake.Handler(*delay)))
}
