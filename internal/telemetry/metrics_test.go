package telemetry

import (
	"math"
	"testing"
	"time"
)

func observe(m *Metrics, d time.Duration, n int) {
	for i := 0; i < n; i++ {
		m.ObserveRequest(RequestEvent{Policy: "p", Provider: "x", Model: "m", Outcome: "ok", Cache: "miss", Latency: d + time.Second, Overhead: d, HasOverhead: true})
	}
}

func TestOverheadQuantilesNeedSamples(t *testing.T) {
	m := NewMetrics()
	if _, n, _, ok := m.OverheadQuantiles(0.5); ok || n != 0 {
		t.Error("no observations means no quantiles")
	}
	// Requests whose overhead is meaningless (retries) are not counted.
	m.ObserveRequest(RequestEvent{Outcome: "ok", Cache: "miss", Overhead: time.Hour, HasOverhead: false})
	if _, _, _, ok := m.OverheadQuantiles(0.5); ok {
		t.Error("a request without a usable overhead figure must not appear in the histogram")
	}
}

func TestOverheadQuantilesInterpolateWithinABucket(t *testing.T) {
	m := NewMetrics()
	// Buckets are 5e-5, 1e-4, 2.5e-4, 5e-4, 1e-3 ... 100 observations at 300µs fall in the (250µs, 500µs] bucket.
	observe(m, 300*time.Microsecond, 100)
	v, n, _, ok := m.OverheadQuantiles(0.5, 0.95, 0.99)
	if !ok || n != 100 {
		t.Fatalf("ok=%v n=%d", ok, n)
	}
	// Linear interpolation across that bucket: p50 sits at its middle, p95 near the top.
	want := []float64{0.000375, 0.0004875, 0.0004975}
	for i := range want {
		if math.Abs(v[i]-want[i]) > 1e-9 {
			t.Errorf("q%d = %.7f, want %.7f", i, v[i], want[i])
		}
	}
}

func TestOverheadQuantilesAcrossBuckets(t *testing.T) {
	m := NewMetrics()
	observe(m, 80*time.Microsecond, 90) // (50µs, 100µs] bucket
	observe(m, 20*time.Millisecond, 10) // (10ms, 25ms] bucket
	v, _, _, _ := m.OverheadQuantiles(0.5, 0.99)
	if v[0] <= 0.00005 || v[0] > 0.0001 {
		t.Errorf("p50 = %v, want inside the 50-100µs bucket", v[0])
	}
	if v[1] <= 0.01 || v[1] > 0.025 {
		t.Errorf("p99 = %v, want inside the 10-25ms bucket", v[1])
	}
}

func TestOverheadQuantilesPastTheLastBucketAreCapped(t *testing.T) {
	m := NewMetrics()
	observe(m, 5*time.Second, 10) // beyond the 1s top bucket
	v, _, _, ok := m.OverheadQuantiles(0.99)
	if !ok || v[0] != 1 {
		t.Errorf("an observation past the last bucket reports the last finite bound, got %v", v)
	}
}

func TestRequestMetricsAreCounted(t *testing.T) {
	m := NewMetrics()
	m.ObserveRequest(RequestEvent{Policy: "p", Provider: "x", Model: "m", Outcome: "ok", Cache: "hit_exact", Key: "spw_aaaa", Latency: time.Millisecond, SavedUSD: 0.5, HasOverhead: true})
	m.ObserveRequest(RequestEvent{Policy: "p", Provider: "x", Model: "m", Outcome: "ok", Cache: "miss", Key: "spw_aaaa", Latency: time.Millisecond, CostUSD: 0.25, HasOverhead: true})
	_, n, since, ok := m.OverheadQuantiles(0.5)
	if !ok || n != 2 || since.IsZero() {
		t.Errorf("n=%d since=%v ok=%v", n, since, ok)
	}
}
