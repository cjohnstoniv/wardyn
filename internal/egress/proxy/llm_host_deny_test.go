// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A local model with no auth is reached by the broker with no credential at all, so dropping the
// credential lane does not close it: a deny of its host (a live egress edit's) must refuse the
// request before it is forwarded, on both brokered routes.
func TestLLMBroker_DeniedLocalModelHostIsRefused(t *testing.T) {
	for _, path := range []string{llmAnthropicPrefix + "v1/messages", llmOpenAIPrefix + "v1/chat/completions"} {
		up := captureUpstream(t, false, `{"ok":true}`)
		buf := &bytes.Buffer{}
		p := newProxy(Options{
			RunID:        uuid.New(),
			Policy:       CompilePolicy(types.RunPolicySpec{DeniedDomains: []string{"ollama.lan"}}),
			Sink:         &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
			Resolver:     fakeResolver{m: map[string][]net.IP{"ollama.lan": ips("10.40.1.5")}},
			Dial:         redirectDial(upstreamAddr(up.srv)),
			LLMUpstreams: map[string]string{anthropicHost: localModelBase, openaiHost: localModelBase},
			LocalModels:  map[string]types.LocalModelSettings{localModelBase: {Models: []string{localModelName}}},
		})
		req := mustLocalReq(t, http.MethodPost, path, strings.NewReader(`{"model":"qwen2.5:3b","messages":[]}`))
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || up.reached {
			t.Fatalf("%s: status = %d reached = %v, want 403 and the denied local model never dialled", path, rec.Code, up.reached)
		}
		if d := lastDecision(t, buf); d.Decision != egress.Deny || d.RuleSource != "policy:denied" {
			t.Fatalf("%s: decision = %+v, want deny policy:denied", path, d)
		}
	}
}

// The same local model with no deny is still served: the check refuses only an explicit deny.
func TestLLMBroker_UndeniedLocalModelStillServed(t *testing.T) {
	up := captureUpstream(t, false, `{"ok":true}`)
	p, _ := localModelProxy(t, upstreamAddr(up.srv), nil, types.LocalModelSettings{})
	req := mustLocalReq(t, http.MethodPost, llmOpenAIPrefix+"v1/chat/completions", strings.NewReader(`{"model":"qwen2.5:3b","messages":[]}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !up.reached {
		t.Fatalf("status = %d reached = %v, want 200 and the local model dialled", rec.Code, up.reached)
	}
}
