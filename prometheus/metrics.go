// Package sprometheus provides a Prometheus metrics middleware for scaffold
// HTTP bindings. It is a subpackage so that services which do not need
// Prometheus metrics do not link the client_golang binary into their binary.
//
// The core scaffold package MUST NOT import anything from this package or
// from github.com/prometheus/client_golang.
package sprometheus

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/kapetan-io/scaffold"
	"github.com/prometheus/client_golang/prometheus"
)

// collectorSet is the pair of collectors registered against a single
// prometheus.Registerer. HTTPMetrics caches one collectorSet per registerer
// so that repeated calls with the same registerer do not panic from duplicate
// registration.
type collectorSet struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

// collectorCache holds the per-registerer collector sets. Keyed by the
// prometheus.Registerer interface value so that callers using
// prometheus.DefaultRegisterer or a fresh prometheus.NewRegistry each get
// their own collector set the first time and a cached set thereafter.
var collectorCache sync.Map

// HTTPMetrics returns a middleware that records two collectors for every
// request routed through the wrapped handler:
//
//   - http_requests_total (CounterVec) with labels method, pattern, status.
//   - http_request_duration_seconds (HistogramVec) with the same labels and
//     prometheus.DefBuckets.
//
// A nil registerer is treated as prometheus.DefaultRegisterer. Collectors are
// cached per registerer, so calling HTTPMetrics multiple times with the same
// registerer is safe and returns middleware bound to the originally
// registered collectors.
//
// The pattern label is read from r.Pattern after the wrapped handler returns.
// When r.Pattern is empty the label value is "unknown".
func HTTPMetrics(registerer prometheus.Registerer) scaffold.MiddlewareFunc {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	set := collectorsFor(registerer)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rw := &responseWriter{ResponseWriter: w}
			start := time.Now()
			next.ServeHTTP(rw, r)
			elapsed := time.Since(start).Seconds()

			pattern := r.Pattern
			if pattern == "" {
				pattern = "unknown"
			}
			status := rw.status
			if status == 0 {
				status = http.StatusOK
			}
			statusStr := strconv.Itoa(status)

			set.requests.WithLabelValues(r.Method, pattern, statusStr).Inc()
			set.duration.WithLabelValues(r.Method, pattern, statusStr).Observe(elapsed)
		})
	}
}

func collectorsFor(registerer prometheus.Registerer) *collectorSet {
	if cached, ok := collectorCache.Load(registerer); ok {
		return cached.(*collectorSet)
	}
	labels := []string{"method", "pattern", "status"}
	set := &collectorSet{
		requests: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "http_requests_total",
				Help: "Total number of HTTP requests processed, labeled by method, route pattern, and status code.",
			},
			labels,
		),
		duration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "http_request_duration_seconds",
				Help:    "Histogram of HTTP request durations in seconds, labeled by method, route pattern, and status code.",
				Buckets: prometheus.DefBuckets,
			},
			labels,
		),
	}
	actual, loaded := collectorCache.LoadOrStore(registerer, set)
	if loaded {
		return actual.(*collectorSet)
	}
	registerer.MustRegister(set.requests, set.duration)
	return set
}
