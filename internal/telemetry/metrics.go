// Package telemetry holds Prometheus metrics and OpenTelemetry setup.
package telemetry

import (
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// RequestEvent is one finished gateway request.
type RequestEvent struct {
	Policy, Provider, Model, Outcome, Cache, Key string
	Latency                                      time.Duration
	// Overhead is the time spent in Spillway itself: total latency minus the time providers took.
	Overhead    time.Duration
	HasOverhead bool    // false when retries or fallbacks make the figure meaningless
	CostUSD     float64 // for monitoring only; accounting uses micro-dollar integers
	SavedUSD    float64
}

// Metrics owns a private registry, so tests and multiple instances never collide on global state.
type Metrics struct {
	reg *prometheus.Registry

	requests  *prometheus.CounterVec
	latency   prometheus.Histogram
	overhead  prometheus.Histogram
	attempts  *prometheus.CounterVec
	cost      *prometheus.CounterVec
	saved     *prometheus.CounterVec
	cacheHits *prometheus.CounterVec
	depErrors *prometheus.CounterVec

	started time.Time

	mu      sync.RWMutex
	breaker func() map[string]int
}

func NewMetrics() *Metrics {
	m := &Metrics{reg: prometheus.NewRegistry(), started: time.Now()}
	m.requests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "spillway_gateway_requests_total", Help: "Gateway requests by policy, answering provider and model, outcome and cache status.",
	}, []string{"policy", "provider", "model", "outcome", "cache"})
	m.latency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "spillway_gateway_latency_seconds", Help: "Total request latency as the client sees it.",
		Buckets: prometheus.ExponentialBuckets(0.005, 2.2, 14),
	})
	m.overhead = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "spillway_gateway_overhead_seconds", Help: "Time spent in Spillway, excluding the upstream call.",
		Buckets: []float64{0.00005, 0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
	})
	m.attempts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "spillway_provider_attempts_total", Help: "Calls to providers by kind (primary, retry, fallback, hedge, skipped) and error kind.",
	}, []string{"provider", "kind", "error"})
	m.cost = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "spillway_cost_usd_total", Help: "Spend in US dollars by API key prefix and model.",
	}, []string{"key", "model"})
	m.saved = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "spillway_saved_usd_total", Help: "Spend avoided by cache hits, in US dollars.",
	}, []string{"kind"})
	m.cacheHits = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "spillway_cache_hits_total", Help: "Cache hits by kind (exact, semantic).",
	}, []string{"kind"})
	m.depErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "spillway_dependency_errors_total", Help: "Errors talking to Redis, the embedding model or Postgres on the request path.",
	}, []string{"dependency"})

	m.reg.MustRegister(m.requests, m.latency, m.overhead, m.attempts, m.cost, m.saved, m.cacheHits, m.depErrors,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}), breakerCollector{m})
	return m
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

func (m *Metrics) ObserveRequest(e RequestEvent) {
	m.requests.WithLabelValues(e.Policy, e.Provider, e.Model, e.Outcome, e.Cache).Inc()
	m.latency.Observe(e.Latency.Seconds())
	if e.HasOverhead {
		m.overhead.Observe(e.Overhead.Seconds())
	}
	if e.CostUSD > 0 {
		m.cost.WithLabelValues(e.Key, e.Model).Add(e.CostUSD)
	}
	switch e.Cache {
	case "hit_exact":
		m.cacheHits.WithLabelValues("exact").Inc()
		m.saved.WithLabelValues("exact").Add(e.SavedUSD)
	case "hit_semantic":
		m.cacheHits.WithLabelValues("semantic").Inc()
		m.saved.WithLabelValues("semantic").Add(e.SavedUSD)
	}
}

func (m *Metrics) ObserveAttempt(provider, kind, errKind string) {
	m.attempts.WithLabelValues(provider, kind, errKind).Inc()
}

func (m *Metrics) DependencyError(dep string) { m.depErrors.WithLabelValues(dep).Inc() }

// SetBreakerSource supplies breaker states (0 closed, 1 open, 2 half-open) read at scrape time.
func (m *Metrics) SetBreakerSource(f func() map[string]int) {
	m.mu.Lock()
	m.breaker = f
	m.mu.Unlock()
}

type breakerCollector struct{ m *Metrics }

var breakerDesc = prometheus.NewDesc("spillway_breaker_state",
	"Circuit breaker state per provider: 0 closed, 1 open, 2 half-open.", []string{"provider"}, nil)

func (breakerCollector) Describe(ch chan<- *prometheus.Desc) { ch <- breakerDesc }

func (c breakerCollector) Collect(ch chan<- prometheus.Metric) {
	c.m.mu.RLock()
	f := c.m.breaker
	c.m.mu.RUnlock()
	if f == nil {
		return
	}
	for p, s := range f() {
		ch <- prometheus.MustNewConstMetric(breakerDesc, prometheus.GaugeValue, float64(s), p)
	}
}

// Quantiles estimates quantiles of the overhead histogram the way Prometheus's histogram_quantile does, by linear
// interpolation inside the bucket that holds the target rank. Values are seconds. ok is false when nothing has
// been observed yet. The histogram lives in memory, so it covers the time since this process started.
func (m *Metrics) OverheadQuantiles(qs ...float64) (values []float64, samples uint64, since time.Time, ok bool) {
	fams, err := m.reg.Gather()
	if err != nil {
		return nil, 0, m.started, false
	}
	for _, f := range fams {
		if f.GetName() != "spillway_gateway_overhead_seconds" || len(f.Metric) == 0 {
			continue
		}
		h := f.Metric[0].GetHistogram()
		samples = h.GetSampleCount()
		if samples == 0 {
			return nil, 0, m.started, false
		}
		type bucket struct {
			upper float64
			cum   uint64
		}
		var bs []bucket
		for _, b := range h.Bucket {
			bs = append(bs, bucket{b.GetUpperBound(), b.GetCumulativeCount()})
		}
		for _, q := range qs {
			rank := q * float64(samples)
			v := bs[len(bs)-1].upper
			prevUpper, prevCum := 0.0, uint64(0)
			for _, b := range bs {
				if float64(b.cum) >= rank {
					if b.cum == prevCum || b.upper > 1e300 { // an empty bucket or the +Inf bucket: nothing to interpolate
						v = max(prevUpper, minFinite(b.upper, prevUpper))
					} else {
						v = prevUpper + (b.upper-prevUpper)*(rank-float64(prevCum))/float64(b.cum-prevCum)
					}
					break
				}
				prevUpper, prevCum = b.upper, b.cum
			}
			values = append(values, v)
		}
		return values, samples, m.started, true
	}
	return nil, 0, m.started, false
}

func minFinite(a, b float64) float64 {
	if a > 1e300 {
		return b
	}
	return a
}
