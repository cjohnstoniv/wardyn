// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package contentscan

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func manySpans(t *testing.T, n int) []byte {
	t.Helper()
	spans := make([]string, n)
	for i := range spans {
		spans[i] = fmt.Sprintf("span-%d", i)
	}
	b, err := json.Marshal(spans)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func countingSidecar(t *testing.T, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if status != http.StatusOK {
			http.Error(w, "sidecar boom", status)
			return
		}
		_ = json.NewEncoder(w).Encode(sidecarResponse{})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// A sidecar outage costs one call, not one per span, and is still a sidecar_error
// that on_scanner_error decides on.
func TestSidecarNotCalledAgainAfterAnError(t *testing.T) {
	for _, tc := range []struct {
		onErr     string
		wantBlock bool
	}{{"", false}, {"block", true}} {
		t.Run("on_scanner_error="+tc.onErr, func(t *testing.T) {
			srv, calls := countingSidecar(t, http.StatusInternalServerError)
			eng, err := NewEngine(types.LLMInspectionSpec{Mode: "block", OnScannerError: tc.onErr, DetectorSidecarURL: srv.URL}, nil)
			if err != nil {
				t.Fatal(err)
			}
			res, _, err := eng.ScanRequest(ChannelGeneric, manySpans(t, 20))
			if err != nil {
				t.Fatal(err)
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("sidecar called %d times for one request after its first error, want 1", got)
			}
			if !res.Skipped || res.SkipReason != "sidecar_error" {
				t.Fatalf("want skipped/sidecar_error, got %+v", res)
			}
			if got := eng.ShouldBlock(res); got != tc.wantBlock {
				t.Fatalf("ShouldBlock = %v, want %v", got, tc.wantBlock)
			}
		})
	}
}

// One request cannot cost the sidecar more than maxSidecarCallsPerRequest calls; the
// spans past it are an incomplete scan, which on_scanner_error=block still refuses.
func TestSidecarCallsAreCappedPerRequest(t *testing.T) {
	for _, tc := range []struct {
		onErr     string
		wantBlock bool
	}{{"", false}, {"block", true}} {
		t.Run("on_scanner_error="+tc.onErr, func(t *testing.T) {
			srv, calls := countingSidecar(t, http.StatusOK)
			eng, err := NewEngine(types.LLMInspectionSpec{Mode: "block", OnScannerError: tc.onErr, DetectorSidecarURL: srv.URL}, nil)
			if err != nil {
				t.Fatal(err)
			}
			res, _, err := eng.ScanRequest(ChannelGeneric, manySpans(t, maxSidecarCallsPerRequest+36))
			if err != nil {
				t.Fatal(err)
			}
			if got := calls.Load(); got != maxSidecarCallsPerRequest {
				t.Fatalf("sidecar called %d times for one request, want the cap %d", got, maxSidecarCallsPerRequest)
			}
			if !res.Skipped || res.SkipReason != "sidecar_error" {
				t.Fatalf("want skipped/sidecar_error, got %+v", res)
			}
			if got := eng.ShouldBlock(res); got != tc.wantBlock {
				t.Fatalf("ShouldBlock = %v, want %v", got, tc.wantBlock)
			}
			if res, _, _ := eng.ScanRequest(ChannelGeneric, manySpans(t, maxSidecarCallsPerRequest)); res.Skipped {
				t.Fatalf("a request at the cap was marked skipped: %+v", res)
			}
		})
	}
}

func TestSidecarCallsFollowTheRequestContext(t *testing.T) {
	srv, calls := countingSidecar(t, http.StatusOK)
	eng, err := NewEngine(types.LLMInspectionSpec{Mode: "block", OnScannerError: "block", DetectorSidecarURL: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, _, err := eng.ScanRequestContext(ctx, ChannelGeneric, manySpans(t, 5))
	if err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("sidecar called %d times for a request that was already gone, want 0", got)
	}
	if !res.Skipped || res.SkipReason != "sidecar_error" || !eng.ShouldBlock(res) {
		t.Fatalf("a cancelled scan must be a sidecar_error that blocks under on_scanner_error=block: %+v", res)
	}
}
