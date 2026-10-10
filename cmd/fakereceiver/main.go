// Command fakereceiver is a tool endpoint that does what a good receiver does: it applies each side effect once, no
// matter how many times the same idempotency key arrives. It exists for the crash-recovery demo, where a worker is
// killed in the middle of a call and the next worker sends the same request again.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/abdullah-9211/spillway/internal/tools"
)

func main() {
	addr := flag.String("addr", ":9998", "listen address")
	delay := flag.Duration("delay", time.Second, "how long each call takes")
	secret := flag.String("secret", os.Getenv("SPILLWAY_WEBHOOK_SECRET"), "verify X-Spillway-Signature with this secret (empty: do not check)")
	flag.Parse()

	var mu sync.Mutex
	deliveries, applied := map[string]int{}, map[string]bool{}

	http.HandleFunc("/ledger", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		dup := 0
		for _, n := range deliveries {
			dup += n - 1
		}
		_ = json.NewEncoder(w).Encode(map[string]int{"keys_seen": len(deliveries), "effects_applied": len(applied), "redelivered_and_ignored": dup})
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if *secret != "" && !tools.Verify(*secret, body, r.Header.Get("X-Spillway-Signature")) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		key := r.Header.Get("Idempotency-Key")
		mu.Lock()
		deliveries[key]++
		first := !applied[key]
		applied[key] = true
		mu.Unlock()
		short := key
		if len(short) > 10 {
			short = short[:10]
		}
		time.Sleep(*delay)
		if first {
			log.Printf("key %s: effect APPLIED", short)
		} else {
			log.Printf("key %s: already applied, not repeated", short)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"applied":%t}`, first)
	})
	log.Printf("fake receiver listening on %s", *addr)
	log.Fatal((&http.Server{Addr: *addr, ReadHeaderTimeout: 10 * time.Second}).ListenAndServe())
}
