package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (r *flushRecorder) Flush() {
	r.flushed = true
	r.ResponseRecorder.Flush()
}

func TestInstrumentHandlerPreservesFlusher(t *testing.T) {
	handler := instrumentHandler("events", func(w http.ResponseWriter, req *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("wrapped response writer does not implement http.Flusher")
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message\ndata: ok\n\n"))
		flusher.Flush()
	})

	rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest(http.MethodGet, "/events?user_id=1", nil)

	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !rec.flushed {
		t.Fatal("expected flush to be forwarded to underlying response writer")
	}
}
