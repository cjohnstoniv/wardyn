// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Over https the bearer goes out on the wire, and a failed delivery never
// puts it, or the endpoint's query secret, in the logged error.
func TestOTLPSink_BearerSentOverHTTPSAndNeverInErrors(t *testing.T) {
	t.Parallel()
	const token = "s3cr3t-b34r3r"
	var gotAuth atomic.Value
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	sink, err := NewOTLPSink(OTLPConfig{Endpoint: srv.URL + "?key=querysecret", BearerToken: token, BatchSize: 1, FlushInterval: "1h"})
	if err != nil {
		t.Fatal(err)
	}
	sink.client = srv.Client()
	body, err := encodeOTLPLogs([]types.AuditEvent{{Action: "a", Outcome: "success", Time: time.Now()}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sink.post(context.Background(), body); err != nil {
		t.Fatalf("post: %v", err)
	}
	if got, _ := gotAuth.Load().(string); got != "Bearer "+token {
		t.Errorf("Authorization = %q, want the bearer", got)
	}

	srv.Close()
	_, err = sink.post(context.Background(), body)
	if err == nil {
		t.Fatal("post to a closed collector succeeded")
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "querysecret") {
		t.Errorf("error leaks a credential: %v", err)
	}
}
