package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A control-plane URL with a trailing slash must not post to "//api/...": the
// internal listener does not clean paths, so that matches no route.
func TestEventSink_TrailingSlashInControlPlaneURL(t *testing.T) {
	paths := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := newEventSink(srv.URL+"/", "tok", 8, 1, 10*time.Millisecond, srv.Client())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.close(ctx)
	})
	s.emit(types.AuditEvent{})
	select {
	case got := <-paths:
		if got != "/api/v1/internal/groundtruth" {
			t.Errorf("request path = %q, want exactly /api/v1/internal/groundtruth", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the sink never posted")
	}
}
