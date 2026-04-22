package sprometheus_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	sprometheus "github.com/kapetan-io/scaffold/prometheus"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gatherCounter returns the value of http_requests_total for the given label
// tuple from the registry, or 0 if no such metric is present.
func gatherCounter(t *testing.T, reg prometheus.Gatherer, method, pattern, status string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != "http_requests_total" {
			continue
		}
		for _, m := range mf.Metric {
			if labelMatches(m.Label, method, pattern, status) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// gatherHistogramCount returns the sample count of http_request_duration_seconds
// for the given label tuple from the registry, or 0 if no such metric is present.
func gatherHistogramCount(t *testing.T, reg prometheus.Gatherer, method, pattern, status string) uint64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != "http_request_duration_seconds" {
			continue
		}
		for _, m := range mf.Metric {
			if labelMatches(m.Label, method, pattern, status) {
				return m.GetHistogram().GetSampleCount()
			}
		}
	}
	return 0
}

func labelMatches(labels []*dto.LabelPair, method, pattern, status string) bool {
	want := map[string]string{"method": method, "pattern": pattern, "status": status}
	got := map[string]string{}
	for _, l := range labels {
		got[l.GetName()] = l.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func TestHTTPMetricsRecordsCounterAndHistogram(t *testing.T) {
	reg := prometheus.NewRegistry()
	mw := sprometheus.HTTPMetrics(reg)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/thing", nil)
	req.Pattern = "/thing"
	handler.ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, float64(1), gatherCounter(t, reg, "GET", "/thing", "204"))
	assert.Equal(t, uint64(1), gatherHistogramCount(t, reg, "GET", "/thing", "204"))
}

func TestHTTPMetricsPatternLabelUsedWhenSet(t *testing.T) {
	reg := prometheus.NewRegistry()
	mw := sprometheus.HTTPMetrics(reg)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/fake", nil)
	req.Pattern = "/v1/fake"
	handler.ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, float64(1), gatherCounter(t, reg, "POST", "/v1/fake", "200"))
}

func TestHTTPMetricsPatternLabelUnknownWhenUnset(t *testing.T) {
	reg := prometheus.NewRegistry()
	mw := sprometheus.HTTPMetrics(reg)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/no-mux", nil)
	// r.Pattern intentionally left unset
	handler.ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, float64(1), gatherCounter(t, reg, "GET", "unknown", "200"))
}

func TestHTTPMetricsNilRegistererUsesDefault(t *testing.T) {
	mw := sprometheus.HTTPMetrics(nil)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	req := httptest.NewRequest(http.MethodGet, "/default-reg", nil)
	req.Pattern = "/default-reg-" + t.Name()
	handler.ServeHTTP(httptest.NewRecorder(), req)

	// Gather from the default registerer via the matching gatherer
	got := gatherCounter(t, prometheus.DefaultGatherer, "GET", req.Pattern, "418")
	assert.Equal(t, float64(1), got)
}

func TestHTTPMetricsSameRegistererReusesCollectors(t *testing.T) {
	reg := prometheus.NewRegistry()

	// First call registers; second call must not panic and must share collectors.
	mw1 := sprometheus.HTTPMetrics(reg)
	mw2 := sprometheus.HTTPMetrics(reg)

	h1 := mw1(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	h2 := mw2(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/reuse", nil)
	req.Pattern = "/reuse"
	h1.ServeHTTP(httptest.NewRecorder(), req)
	h2.ServeHTTP(httptest.NewRecorder(), req)

	// Both middlewares route to the same collector set, so the counter for
	// (GET, /reuse, 200) should equal 2.
	assert.Equal(t, float64(2), gatherCounter(t, reg, "GET", "/reuse", "200"))
}
