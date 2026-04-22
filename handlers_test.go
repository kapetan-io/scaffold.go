package scaffold_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kapetan-io/scaffold"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHealthHandlerHappyPath(t *testing.T) {
	h := scaffold.HealthHandler(discardLogger(), func(ctx context.Context) any {
		return map[string]string{"status": "ok"}
	})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var parsed map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &parsed))
	assert.Equal(t, "ok", parsed["status"])
}

func TestHealthHandlerMarshalFailurePath(t *testing.T) {
	log, captured := newTestLogger()
	h := scaffold.HealthHandler(log, func(ctx context.Context) any {
		// chan values are rejected by encoding/json.
		return make(chan int)
	})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, `{"error":"health marshal failed"}`, rec.Body.String())

	r := captured.findByMessage("health marshal failed")
	require.NotNil(t, r)

	path, ok := recordAttr(*r, "path")
	require.True(t, ok)
	assert.Equal(t, "/health", path.Value.String())

	_, ok = recordAttr(*r, "error")
	assert.True(t, ok)
}

func TestHealthHandlerNilLogPanics(t *testing.T) {
	assert.Panics(t, func() {
		scaffold.HealthHandler(nil, func(ctx context.Context) any { return nil })
	})
}

func TestReadyHandlerReadyReturns200(t *testing.T) {
	h := scaffold.ReadyHandler(func(ctx context.Context) (bool, string) {
		return true, ""
	})

	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Body.String())
}

func TestReadyHandlerNotReadyReturns503WithReason(t *testing.T) {
	h := scaffold.ReadyHandler(func(ctx context.Context) (bool, string) {
		return false, "warming caches"
	})

	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.Equal(t, "warming caches", rec.Body.String())
}
