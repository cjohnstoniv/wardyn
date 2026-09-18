// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

func TestBrokeredScanUploadRejectsOversizeInsteadOfTruncating(t *testing.T) {
	var calls atomic.Int32
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil || !json.Valid(body) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer cp.Close()
	p, decisions := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cp), nil, nil)
	// The API shares this cap. Dropping the suffix turns an invalid, oversized
	// document into valid scan facts that its strict unmarshal accepts.
	body := "{}" + strings.Repeat(" ", maxScanResultBody-2) + "invalid suffix"
	r := mustLocalReq(t, http.MethodPut, routeScanResults+uuid.NewString(), strings.NewReader(body))
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge || calls.Load() != 0 {
		t.Fatalf("oversized scan: status=%d upstream calls=%d; want 413 without forwarding", w.Code, calls.Load())
	}
	if d := lastDecision(t, decisions); d.Decision != egress.Deny || d.RuleSource != ruleSourceScanResults {
		t.Fatalf("oversized scan decision = %+v", d)
	}
}

func TestBrokeredUploadBodyBoundary(t *testing.T) {
	const limit = 32
	for _, tc := range []struct {
		name          string
		size          int
		unknownLength bool
		wantStatus    int
	}{
		{"below", limit - 1, false, http.StatusNoContent},
		{"at", limit, false, http.StatusNoContent},
		{"over", limit + 1, false, http.StatusRequestEntityTooLarge},
		{"unknown-at", limit, true, http.StatusNoContent},
		{"unknown-over", limit + 1, true, http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			body := strings.Repeat("x", tc.size)
			cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				got, err := io.ReadAll(r.Body)
				if err != nil || string(got) != body {
					t.Errorf("forwarded body=%q err=%v; want complete body %q", got, err, body)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer cp.Close()
			p, _ := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cp), nil, nil)
			r := mustLocalReq(t, http.MethodPut, routeRecordings+uuid.NewString(), strings.NewReader(body))
			if tc.unknownLength {
				r.ContentLength = -1
			}
			w := httptest.NewRecorder()
			p.forwardBrokeredUpload(w, r, routeRecordings, "/api/v1/internal/recordings/",
				ruleSourceRecordings, "read recording body", limit)
			if w.Code != tc.wantStatus {
				t.Fatalf("status=%d; want %d", w.Code, tc.wantStatus)
			}
			wantCalls := int32(1)
			if tc.wantStatus == http.StatusRequestEntityTooLarge {
				wantCalls = 0
			}
			if calls.Load() != wantCalls {
				t.Fatalf("upstream calls=%d; want %d", calls.Load(), wantCalls)
			}
		})
	}
}
