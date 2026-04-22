package scaffold_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kapetan-io/scaffold"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPanicRecoveryNilLogPanicsAtConstruction(t *testing.T) {
	assert.Panics(t, func() {
		scaffold.PanicRecovery(nil)
	})
}

func TestPanicRecoveryConvertsPanicTo500(t *testing.T) {
	mw := scaffold.PanicRecovery(discardLogger())
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("kaboom")
	}))

	req := httptest.NewRequest(http.MethodGet, "/explode", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, "internal server error", rec.Body.String())
	assert.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"))
}

func TestPanicRecoveryLogsPinnedFields(t *testing.T) {
	log, captured := newTestLogger()
	mw := scaffold.PanicRecovery(log)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("kaboom")
	}))

	req := httptest.NewRequest(http.MethodPost, "/boom", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	r := captured.findByMessage("panic recovered")
	require.NotNil(t, r)

	method, ok := recordAttr(*r, "method")
	require.True(t, ok)
	assert.Equal(t, http.MethodPost, method.Value.String())

	path, ok := recordAttr(*r, "path")
	require.True(t, ok)
	assert.Equal(t, "/boom", path.Value.String())

	panicAttr, ok := recordAttr(*r, "panic")
	require.True(t, ok)
	assert.Equal(t, "kaboom", panicAttr.Value.String())

	_, ok = recordAttr(*r, "stack")
	assert.True(t, ok)
}

func TestPanicRecoveryDoesNotPanicWhenResponseAlreadyStarted(t *testing.T) {
	log, captured := newTestLogger()
	mw := scaffold.PanicRecovery(log)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		panic("mid-write boom")
	}))

	req := httptest.NewRequest(http.MethodGet, "/halfway", nil)
	rec := httptest.NewRecorder()
	assert.NotPanics(t, func() {
		handler.ServeHTTP(rec, req)
	})

	// Response reflects the handler's partial write; the middleware does
	// not attempt to overwrite a response already in progress.
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "partial", rec.Body.String())

	r := captured.findByMessage("panic recovered")
	require.NotNil(t, r)
}
