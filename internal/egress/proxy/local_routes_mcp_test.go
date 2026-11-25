// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestMCPProxyServesNoLocalRoutes: an MCP sandbox's proxy answers 404 to every local /wardyn/
// route, so the server behind it cannot raise approvals or take-over requests, mint, upload or
// reach a broker through it. A run's own proxy sends the same requests to their handlers.
func TestMCPProxyServesNoLocalRoutes(t *testing.T) {
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	t.Cleanup(cp.Close)
	id := uuid.NewString()
	routes := []struct{ method, path string }{
		{http.MethodGet, routeApprovals + id},
		{http.MethodPost, routeApprovalsCreate},
		{http.MethodPost, routeMint},
		{http.MethodPost, routeApprovals + id + routeApprovalsExpireSuffix},
		{http.MethodPut, routeRecordings + id},
		{http.MethodPut, routeScanResults + id},
		{http.MethodPut, routeSSOToken + id},
		{http.MethodPost, routeVMNetDNS},
		{http.MethodPost, llmAnthropicPrefix + "v1/messages"},
		{http.MethodPost, llmOpenAIPrefix + "v1/chat/completions"},
		{http.MethodGet, routeGitBroker + "o/r.git/info/refs"},
		{http.MethodGet, routePATBroker + "github.com/o/r.git/info/refs"},
		{http.MethodPost, routeMCPBroker + "tok/docs"},
		{http.MethodPost, routeSSHAgentClose},
		{http.MethodPost, routeSSHAgent + "sign"},
		{http.MethodPost, routeMCPRelay + "tok"},
	}
	for _, mcp := range []bool{true, false} {
		extra := `,"vm":true`
		if mcp {
			extra = `,"control_plane_audience":"wardyn-mcp-sandbox","mcp_relay_inbound":{"listen":":3130","token_sha256":"` + mcpRelayDigest + `","upstream":"10.88.0.3:8765"}`
		}
		cfg, err := LoadConfigBytes(fmt.Appendf(nil, `{"run_id":%q,"control_plane_url":%q,"run_token":"t"%s}`, uuid.New(), cp.URL, extra))
		if err != nil {
			t.Fatal(err)
		}
		noDial := func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("no network in this test")
		}
		srv, err := newServer(context.Background(), cfg, cp.Client(), &bytes.Buffer{}, serverHooks{dial: noDial, res: publicResolver{}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
		for _, rt := range routes {
			req := mustLocalReq(t, rt.method, rt.path, strings.NewReader(`{}`))
			req = req.WithContext(context.WithValue(req.Context(), vmnetLaneKey{}, true)) // the VM relay's lane, for its report route
			rec := httptest.NewRecorder()
			srv.http.Handler.ServeHTTP(rec, req)
			refused := rec.Code == http.StatusNotFound && rec.Body.String() == "unknown brokered route\n"
			switch {
			case mcp && !refused:
				t.Errorf("MCP sandbox proxy: %s %s answered %d %q, want 404 unknown brokered route", rt.method, rt.path, rec.Code, rec.Body.String())
			case !mcp && refused && rt.path != routeMCPRelay+"tok":
				t.Errorf("run's proxy: %s %s answered 404 unknown brokered route, want it to reach its handler", rt.method, rt.path)
			case !mcp && !refused && rt.path == routeMCPRelay+"tok":
				t.Errorf("run's proxy: the relay path answered %d %q; no local route serves it", rec.Code, rec.Body.String())
			}
		}
	}
}
