// Command fakeprovider runs the fake provider as an OpenAI-compatible HTTP server. Point a provider's
// base_url at it (http://localhost:9999/v1) to exercise Spillway without real API keys.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/abdullah-9211/spillway/internal/provider/fake"
)

func main() {
	addr := flag.String("addr", ":9999", "listen address")
	text := flag.String("text", "", "default answer text")
	delay := flag.Duration("delay", 0, "default latency added to every answer")
	flag.Parse()

	p := fake.New("fake")
	p.Default = fake.Behavior{Text: *text, Delay: *delay}
	srv := &http.Server{Addr: *addr, Handler: fake.NewHandler(p), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("fake provider listening on %s", *addr)
	log.Fatal(srv.ListenAndServe())
}
