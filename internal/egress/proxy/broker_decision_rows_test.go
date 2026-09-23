// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// decisionRows is the sink's rows as "decision rule_source", in order.
func decisionRows(t *testing.T, buf *bytes.Buffer) []string {
	t.Helper()
	var rows []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var d egress.DecisionLog
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			t.Fatalf("decode decision %q: %v", line, err)
		}
		rows = append(rows, string(d.Decision)+" "+d.RuleSource)
	}
	return rows
}

// refusedAddr is a loopback address nothing listens on.
func refusedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// TestBrokerDecisionRows pins one decision row per request on all three git
// broker lanes (GitHub App, git_pat, Azure DevOps Entra): the allow row only
// after a successful round trip, builtin:dial-failed when the dial fails, and
// builtin:upstream-protocol-mismatch alone when the forge answers HTTP/2
// unasked — the contract the plain and LLM lanes already keep.
func TestBrokerDecisionRows(t *testing.T) {
	okForge := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(okForge.Close)

	type lane struct {
		name, path, allow string
		proxy             func(t *testing.T, forge string, buf *bytes.Buffer) *Proxy
	}
	lanes := []lane{
		{"git", "/wardyn/gh/octocat/hello-world.git/info/refs?service=git-upload-pack", ruleSourceGit,
			func(t *testing.T, forge string, buf *bytes.Buffer) *Proxy {
				mint := newGitBrokerUpstream(t, "gh-inst-token")
				return newProxy(Options{
					RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{}),
					Sink:     &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 8)},
					Resolver: publicResolver{}, Dial: splitDial(upstreamAddr(mint.srv), forge),
					ControlPlaneURL: "https://wardynd.test:8080", RunToken: newTokenSource("RUNTOK"),
					TLSClientConfig: testInsecureTLSConfig,
					ControlTLS:      testInsecureTLSConfig,
					GitGrants:       map[string]uuid.UUID{"octocat/hello-world": uuid.New()},
				})
			}},
		{"pat", "/wardyn/git/gitlab.com/org/repo.git/info/refs?service=git-upload-pack", ruleSourcePAT,
			func(t *testing.T, forge string, buf *bytes.Buffer) *Proxy {
				mint := newPATBrokerUpstream(t, "T", "oauth2")
				return newProxy(Options{
					RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{}),
					Sink:     &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 8)},
					Resolver: publicResolver{}, Dial: splitDial(upstreamAddr(mint.srv), forge),
					ControlPlaneURL: "https://wardynd.test:8080", RunToken: newTokenSource("RUNTOK"),
					TLSClientConfig: testInsecureTLSConfig,
					ControlTLS:      testInsecureTLSConfig,
					PATGrants:       map[string]PATGrant{"gitlab.com": {GrantID: uuid.New()}},
				})
			}},
		{"ado", "/wardyn/git/dev.azure.com/acme/proj/_git/app/info/refs?service=git-upload-pack", ruleSourceADOGit,
			func(t *testing.T, forge string, buf *bytes.Buffer) *Proxy {
				bearer := injectedHeader{name: "Authorization", value: "Bearer entra-" + uuid.NewString()}
				return newProxy(Options{
					RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{}),
					Injector: &injector{byHost: map[string]*injEntry{
						"dev.azure.com": {grantID: uuid.New(), header: bearer, requireTLS: true},
					}},
					Sink:     &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 8)},
					Resolver: publicResolver{}, Dial: splitDial(forge, forge),
					RunToken: newTokenSource("RUNTOK"), TLSClientConfig: testInsecureTLSConfig, ControlTLS: testInsecureTLSConfig,
					ADOGrants: adoGrantMap{"dev.azure.com": {Organization: "acme", Capabilities: []adoscope.Capability{adoscope.CapRead}}},
				})
			}},
	}
	for _, l := range lanes {
		for _, c := range []struct {
			name   string
			forge  func(t *testing.T) string
			status int
			rows   []string
		}{
			{"success", func(*testing.T) string { return upstreamAddr(okForge) }, http.StatusOK,
				[]string{"allow " + l.allow}},
			{"dial-failed", refusedAddr, http.StatusBadGateway,
				[]string{"deny builtin:dial-failed"}},
			{"h2-mismatch", startH2MismatchPeer, http.StatusBadRequest,
				[]string{"deny " + ruleSourceUpstreamProtocolMismatch}},
		} {
			t.Run(l.name+"/"+c.name, func(t *testing.T) {
				buf := &bytes.Buffer{}
				p := l.proxy(t, c.forge(t), buf)
				rec := httptest.NewRecorder()
				p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, l.path, nil))
				if rec.Code != c.status {
					t.Errorf("status = %d, want %d (body %q)", rec.Code, c.status, rec.Body.String())
				}
				if got := decisionRows(t, buf); !slices.Equal(got, c.rows) {
					t.Errorf("rows = %q, want %q", got, c.rows)
				}
			})
		}
	}
}
