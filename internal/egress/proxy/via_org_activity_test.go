// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestViaOrgOriginForm_BytesCountAsActivity: bytes moved by a plain via_org request mark the run in
// use, and a request that moves none does not.
func TestViaOrgOriginForm_BytesCountAsActivity(t *testing.T) {
	const viaHost = "git.corp.example"
	for _, tc := range []struct {
		name   string
		method string
		body   string
		status int
		answer string
		want   bool
	}{
		{"response bytes only", http.MethodGet, "", http.StatusOK, "from-org", true},
		{"request bytes only", http.MethodPost, `{"hi":1}`, http.StatusNoContent, "", true},
		{"no bytes either way", http.MethodGet, "", http.StatusNoContent, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.answer)
			}))
			t.Cleanup(cp.Close)
			cpAddr := upstreamAddr(cp)
			p := newProxy(Options{
				RunID:           uuid.New(),
				Sink:            &decisionSink{out: &bytes.Buffer{}, ch: make(chan queuedDecision, 8)},
				Resolver:        publicResolver{},
				Policy:          CompilePolicy(types.RunPolicySpec{}),
				ControlPlaneURL: "http://" + cpAddr,
				RunToken:        newTokenSource("RUNTOK"),
				ViaOrgRoutes:    []ViaOrgRoute{{GrantID: uuid.New(), Host: viaHost}},
				Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, network, cpAddr)
				},
			})
			if p.streamMoved.Load() {
				t.Fatal("the run was marked active before any request")
			}

			var in io.Reader
			if tc.body != "" {
				in = strings.NewReader(tc.body)
			}
			req := httptest.NewRequest(tc.method, "https://"+viaHost+"/wardyn/git/x", in)
			rec := httptest.NewRecorder()
			p.serveViaOrg(rec, req, p.viaOrgRoutes[0], viaHost)
			if rec.Code != tc.status || rec.Body.String() != tc.answer {
				t.Fatalf("status = %d body = %q, want %d %q", rec.Code, rec.Body.String(), tc.status, tc.answer)
			}
			if got := p.streamMoved.Load(); got != tc.want {
				t.Errorf("streamMoved = %v, want %v", got, tc.want)
			}
		})
	}
}
