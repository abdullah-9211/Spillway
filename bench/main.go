// Command bench measures what the gateway adds to a model call, how many requests per second one instance serves, and
// what the exact cache saves on a repeated workload. The upstream is the fake provider with no delay, so the numbers
// are the gateway's own cost; a real provider adds its own latency to both sides equally.
//
//	go run ./cmd/fakeprovider -addr :9990 -text ok &
//	SPILLWAY_CONFIG=bench/models.yaml spillway serve &
//	go run ./bench -key $KEY
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

var client = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 256}}

func call(url, key, prompt string, temp float64) (time.Duration, http.Header, error) {
	body, _ := json.Marshal(map[string]any{"model": "default", "temperature": temp, "messages": []map[string]string{{"role": "user", "content": prompt}}})
	req, _ := http.NewRequest("POST", url+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	t := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	d := time.Since(t)
	if resp.StatusCode != 200 {
		return d, resp.Header, fmt.Errorf("status %d", resp.StatusCode)
	}
	return d, resp.Header, nil
}

func pct(s []time.Duration, p float64) float64 {
	if len(s) == 0 {
		return 0
	}
	i := int(math.Ceil(p/100*float64(len(s)))) - 1
	return float64(s[max(i, 0)].Microseconds()) / 1000
}

func sample(url, key string, n int, tag string) []time.Duration {
	out := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		d, _, err := call(url, key, fmt.Sprintf("%s-%d-%d", tag, time.Now().UnixNano(), i), 1)
		if err != nil {
			fmt.Fprintln(os.Stderr, "request failed:", err)
			os.Exit(1)
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func throughput(url, key string, c int, dur time.Duration) (float64, int64) {
	var done, failed atomic.Int64
	stop := time.Now().Add(dur)
	var wg sync.WaitGroup
	for w := 0; w < c; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; time.Now().Before(stop); i++ {
				if _, _, err := call(url, key, "tp-"+strconv.Itoa(w)+"-"+strconv.Itoa(i)+strconv.FormatInt(time.Now().UnixNano(), 36), 1); err != nil {
					failed.Add(1)
				} else {
					done.Add(1)
				}
			}
		}(w)
	}
	wg.Wait()
	return float64(done.Load()) / dur.Seconds(), failed.Load()
}

func main() {
	gw := flag.String("gateway", "http://localhost:8080", "gateway URL")
	up := flag.String("upstream", "http://localhost:9990", "the fake provider, called directly for the baseline")
	key := flag.String("key", os.Getenv("SPILLWAY_API_KEY"), "API key with no rate limit or budget")
	n := flag.Int("n", 3000, "requests per latency sample")
	c := flag.Int("c", 32, "concurrency for the throughput test")
	dur := flag.Duration("duration", 10*time.Second, "throughput test length")
	cacheN := flag.Int("cache-requests", 2000, "requests in the cache workload")
	distinct := flag.Int("cache-prompts", 300, "distinct prompts in the cache workload")
	flag.Parse()
	if *key == "" {
		fmt.Fprintln(os.Stderr, "give an API key with -key or SPILLWAY_API_KEY")
		os.Exit(2)
	}
	// The baseline speaks the same OpenAI shape, so the same request goes straight to the upstream.
	sample(*up, "x", 200, "warm-direct")
	sample(*gw, *key, 200, "warm-gw")
	direct := sample(*up, "x", *n, "direct")
	via := sample(*gw, *key, *n, "gateway")

	rpsGW, failGW := throughput(*gw, *key, *c, *dur)
	rpsDirect, _ := throughput(*up, "x", *c, *dur)

	// Cache workload: popular prompts repeat (a Zipf-like draw), at temperature 0 so the exact cache applies.
	rng := rand.New(rand.NewSource(7))
	z := rand.NewZipf(rng, 1.2, 1, uint64(*distinct-1))
	run := fmt.Sprint(time.Now().UnixNano())
	var hits, misses int
	var missCost, hitCostAvoided float64
	firstCost := map[uint64]float64{}
	for i := 0; i < *cacheN; i++ {
		p := z.Uint64()
		_, h, err := call(*gw, *key, fmt.Sprintf("cache-%s-%d: summarise the incident report for team %d", run, p, p), 0)
		if err != nil {
			fmt.Fprintln(os.Stderr, "cache request failed:", err)
			os.Exit(1)
		}
		cost, _ := strconv.ParseFloat(h.Get("X-Spillway-Cost-Usd"), 64)
		switch h.Get("X-Spillway-Cache") {
		case "hit-exact", "hit-semantic":
			hits++
			hitCostAvoided += firstCost[p]
		default:
			misses++
			missCost += cost
			if _, ok := firstCost[p]; !ok {
				firstCost[p] = cost
			}
		}
	}

	fmt.Printf("machine: %s/%s, %d logical CPUs, Go %s\n\n", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version())
	fmt.Println("| | p50 | p95 | p99 |")
	fmt.Println("|---|---|---|---|")
	fmt.Printf("| direct to the upstream | %.2f ms | %.2f ms | %.2f ms |\n", pct(direct, 50), pct(direct, 95), pct(direct, 99))
	fmt.Printf("| through the gateway | %.2f ms | %.2f ms | %.2f ms |\n", pct(via, 50), pct(via, 95), pct(via, 99))
	fmt.Printf("| **added by the gateway** | **%.2f ms** | **%.2f ms** | **%.2f ms** |\n\n", pct(via, 50)-pct(direct, 50), pct(via, 95)-pct(direct, 95), pct(via, 99)-pct(direct, 99))
	fmt.Printf("throughput at concurrency %d for %s: gateway %.0f req/s (%d failed), direct %.0f req/s\n\n", *c, *dur, rpsGW, failGW, rpsDirect)
	total := hits + misses
	fmt.Printf("cache workload: %d requests over %d distinct prompts: %d hits, %d misses, hit rate %.1f%%\n", total, *distinct, hits, misses, 100*float64(hits)/float64(total))
	fmt.Printf("spend with the cache $%.6f, avoided by hits $%.6f (%.1f%% saved)\n", missCost, hitCostAvoided, 100*hitCostAvoided/math.Max(missCost+hitCostAvoided, 1e-12))
}
