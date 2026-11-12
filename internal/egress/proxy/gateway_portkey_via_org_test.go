// Copyright 2025 The Wardyn Authors
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
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A via_org destination that is also a configured gateway host: the organisation attaches the
// credential on its own leg, so the sandbox's x-portkey-* routing headers must not ride there
// either, on the plain forward and on a terminated tunnel's inner request.
func TestGatewayPortkeyStrip_ViaOrgForward(t *testing.T) {
	const viaHost = "gw.corp.example"
	for _, tc := range []struct {
		name     string
		gateway  bool
		wantKept bool
	}{
		{"gateway host", true, false},
		{"not a gateway host", false, true},
	} {
		for _, lane := range []string{"plain", "mitm"} {
			t.Run(tc.name+"/"+lane, func(t *testing.T) {
				var mu sync.Mutex
				var seen http.Header
				cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					seen = r.Header.Clone()
					mu.Unlock()
					_, _ = io.Copy(io.Discard, r.Body)
					_, _ = io.WriteString(w, "from-org")
				}))
				t.Cleanup(cp.Close)
				cpAddr := upstreamAddr(cp)
				o := Options{
					RunID:           uuid.New(),
					Sink:            &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 8)},
					Resolver:        publicResolver{},
					Policy:          CompilePolicy(types.RunPolicySpec{}),
					ControlPlaneURL: "http://" + cpAddr,
					RunToken:        newTokenSource("RUNTOK"),
					ViaOrgRoutes:    []ViaOrgRoute{{GrantID: uuid.New(), Host: viaHost}},
					Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
						return (&net.Dialer{}).DialContext(ctx, network, cpAddr)
					},
				}
				if tc.gateway {
					o.LLMUpstreams = map[string]string{anthropicHost: "https://" + viaHost + "/v1"}
				}
				p := newProxy(o)

				req := httptest.NewRequest(http.MethodPost, "https://"+viaHost+"/v1/messages", strings.NewReader(`{"hi":1}`))
				req.Header.Set("X-Portkey-Custom-Host", "https://attacker.example")
				rec := httptest.NewRecorder()
				if lane == "plain" {
					p.serveViaOrg(rec, req, p.viaOrgRoutes[0], viaHost)
				} else {
					p.serveMITMRequest(rec, req, viaHost, 443)
				}
				if rec.Code != http.StatusOK || rec.Body.String() != "from-org" {
					t.Fatalf("status = %d body = %q, want the organisation's answer", rec.Code, rec.Body.String())
				}
				mu.Lock()
				got := seen.Get("X-Portkey-Custom-Host")
				mu.Unlock()
				if kept := got != ""; kept != tc.wantKept {
					t.Errorf("organisation saw x-portkey-custom-host = %q, kept = %v, want kept = %v", got, kept, tc.wantKept)
				}
			})
		}
	}
}
