// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// uiTokenStore adds the API-token table to the gateway's fake store: the bearer arm resolves through it.
type uiTokenStore struct {
	*uiMemStore
	tokens map[string]types.APIToken
}

func (s *uiTokenStore) GetAPITokenByRaw(_ context.Context, raw string) (types.APIToken, error) {
	t, ok := s.tokens[raw]
	if !ok {
		return types.APIToken{}, store.ErrNotFound
	}
	return t, nil
}

func (s *uiTokenStore) TouchAPIToken(context.Context, uuid.UUID, time.Time) error { return nil }

// inboundHarness is newUIHarness whose app "code" is declared inbound with callers, whose backend echoes
// the headers it was sent, and whose store holds an inbound token for the owner ("wdn_alice"), one for
// bob ("wdn_bob"), one for carol ("wdn_carol") and an API token for the owner ("wdn_api").
func inboundHarness(t *testing.T, callers ...string) (*uiHarness, chan http.Header) {
	t.Helper()
	seen := make(chan http.Header, 64)
	h := newUIHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		_, _ = io.WriteString(w, "sandbox app")
	}))
	h.store.putEffectivePolicy(h.run.ID, types.RunPolicySpec{MinConfinementClass: types.CC2,
		UIApps: []types.UIApp{{Name: "code", Port: uiTestPort, Inbound: true, Callers: callers}}})
	token := func(principal, kind string) types.APIToken {
		return types.APIToken{ID: uuid.New(), Principal: principal, Email: principal + "@example.com", Kind: kind,
			CreatedAt: time.Now().Add(-time.Hour)}
	}
	h.srv.cfg.Store = &uiTokenStore{uiMemStore: h.store, tokens: map[string]types.APIToken{
		"wdn_alice": token(h.owner, types.APITokenKindInbound),
		"wdn_bob":   token("bob", types.APITokenKindInbound),
		"wdn_carol": token("carol", types.APITokenKindInbound),
		"wdn_api":   token(h.owner, types.APITokenKindAPI),
	}}
	return h, seen
}

// call is one bearer request to the app, with mutate applied.
func (h *uiHarness) call(token string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.relay("/api/health?x=1", nil, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
		if mutate != nil {
			mutate(r)
		}
	})
}

func respondedReason(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body errorBody
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Reason
}

// TestUIGateway_InboundToken: an inbound token calls an inbound app as its owner or a named caller; every
// other bearer is the one 401, and a live token with no reach here the one 404.
func TestUIGateway_InboundToken(t *testing.T) {
	t.Run("the owner's token calls, audited without the query", func(t *testing.T) {
		h, _ := inboundHarness(t)
		if rec := h.call("wdn_alice", nil); rec.Code != http.StatusOK || rec.Body.String() != "sandbox app" {
			t.Fatalf("call = %d %s, want 200", rec.Code, rec.Body.String())
		}
		rows := h.audit.auditRows("inbound.allow", "success")
		if len(rows) != 1 || rows[0].Actor != h.owner || rows[0].Target != "code" {
			t.Fatalf("inbound.allow rows = %+v, want one by %s on code", rows, h.owner)
		}
		var data map[string]any
		_ = json.Unmarshal(rows[0].Data, &data)
		if data["via"] != "token" || data["caller"] != "owner" || data["path"] != "/api/health" || data["method"] != http.MethodGet || data["token_id"] == nil {
			t.Errorf("inbound.allow data = %v, want via token, caller owner, path /api/health, a token_id", data)
		}
	})

	t.Run("a named caller calls; the sandbox learns who, never the token", func(t *testing.T) {
		h, seen := inboundHarness(t, "user:bob")
		rec := h.call("wdn_bob", func(r *http.Request) {
			r.Header.Set("X-Wardyn-Caller", "alice")
			r.Header.Set("X-Wardyn-Run", "spoof")
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("call = %d %s, want 200", rec.Code, rec.Body.String())
		}
		got := <-seen
		if got.Get("Authorization") != "" || got.Get("X-Wardyn-Run") != "" {
			t.Errorf("the token or a client X-Wardyn-* header reached the sandbox: %v", got)
		}
		if got.Get("X-Wardyn-Caller") != "bob" || got.Get("X-Wardyn-Caller-Email") != "bob@example.com" {
			t.Errorf("caller headers = %q / %q, want bob", got.Get("X-Wardyn-Caller"), got.Get("X-Wardyn-Caller-Email"))
		}
		if !h.audit.hasDataValue("caller", "user:bob") {
			t.Errorf("no inbound.allow names the caller entry; rows: %v", h.audit.actions())
		}
	})

	for _, tc := range []struct{ name, token string }{
		{"an unknown token", "wdn_nobody"},
		{"an API token", "wdn_api"},
	} {
		t.Run(tc.name+" is 401", func(t *testing.T) {
			h, _ := inboundHarness(t)
			rec := h.call(tc.token, nil)
			if rec.Code != http.StatusUnauthorized || respondedReason(t, rec) != reasonInboundCredentialInvalid || rec.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("call = %d %s %v, want 401 %s with WWW-Authenticate", rec.Code, rec.Body.String(), rec.Header(), reasonInboundCredentialInvalid)
			}
			if !h.audit.hasDataValue("reason", inboundDenyCredentialInvalid) {
				t.Errorf("no inbound.deny credential_invalid; reasons: %s", h.audit.dataReasons())
			}
		})
	}

	t.Run("no reach is one 404", func(t *testing.T) {
		answer := func(h *uiHarness, path string) (int, string) {
			rec := h.relay("", nil, func(r *http.Request) {
				r.URL.Path = path
				r.Header.Set("Authorization", "Bearer wdn_carol")
			})
			return rec.Code, rec.Body.String()
		}
		h, _ := inboundHarness(t, "user:bob")
		code, body := answer(h, uiRelayPrefix(h.run.ID, "code")+"/")
		for name, path := range map[string]string{
			"no run":    uiRelayPrefix(uuid.New(), "code") + "/",
			"no app":    uiRelayPrefix(h.run.ID, "other") + "/",
			"not yours": uiRelayPrefix(h.run.ID, "code") + "/",
		} {
			if c, b := answer(h, path); c != http.StatusNotFound || c != code || b != body {
				t.Errorf("%s = %d %s, want the one 404 %d %s", name, c, b, code, body)
			}
		}
		plain, _ := inboundHarness(t)
		plain.store.putEffectivePolicy(plain.run.ID, types.RunPolicySpec{MinConfinementClass: types.CC2,
			UIApps: []types.UIApp{{Name: "code", Port: uiTestPort}}})
		if c, _ := answer(plain, uiRelayPrefix(plain.run.ID, "code")+"/"); c != http.StatusNotFound {
			t.Errorf("an app that is not inbound = %d, want 404", c)
		}
		if !h.audit.hasDataValue("reason", inboundDenyNotFound) {
			t.Errorf("no inbound.deny not_found; reasons: %s", h.audit.dataReasons())
		}
	})

	t.Run("a background run's inbound app answers a token", func(t *testing.T) {
		h, _ := inboundHarness(t)
		h.run.Experience = types.ExperienceBackground
		h.store.putRun(h.run)
		if rec := h.call("wdn_alice", nil); rec.Code != http.StatusOK {
			t.Fatalf("call = %d %s, want 200", rec.Code, rec.Body.String())
		}
	})
}

// TestUIGateway_InboundRateLimit: past the run's bucket a call is 429 with Retry-After, and audited.
func TestUIGateway_InboundRateLimit(t *testing.T) {
	h, _ := inboundHarness(t)
	for range int(inboundBurst) {
		h.srv.uiInbound.take(h.run.ID, h.clock.now(), "")
	}
	rec := h.call("wdn_alice", nil)
	if rec.Code != http.StatusTooManyRequests || respondedReason(t, rec) != reasonInboundRateLimited || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("call = %d %s, want 429 %s with Retry-After 1", rec.Code, rec.Body.String(), reasonInboundRateLimited)
	}
	if !h.audit.hasDataValue("reason", inboundDenyRateLimited) {
		t.Errorf("no inbound.deny rate_limited; reasons: %s", h.audit.dataReasons())
	}
	h.clock.advance(time.Second)
	if rec := h.call("wdn_alice", nil); rec.Code != http.StatusOK {
		t.Fatalf("a second later = %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// TestUIGateway_HostModeCrossSite: in host mode a session request from another site is refused unless it
// is a top-level navigation, and an Origin must be the request's own; path mode is unchanged.
func TestUIGateway_HostModeCrossSite(t *testing.T) {
	other := "https://" + uuid.NewString() + ".ui.example.com"
	for _, tc := range []struct {
		name    string
		host    bool
		headers map[string]string
		want    int
	}{
		{"a sibling run's WebSocket", true, map[string]string{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "websocket", "Origin": other}, http.StatusForbidden},
		{"a sibling run's fetch", true, map[string]string{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty"}, http.StatusForbidden},
		{"a foreign Origin with no fetch metadata", true, map[string]string{"Origin": other}, http.StatusForbidden},
		{"a top-level navigation from a link", true, map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}, http.StatusOK},
		{"its own page's fetch", true, map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "https://{run}.ui.example.com"}, http.StatusOK},
		{"a client with no fetch metadata", true, nil, http.StatusOK},
		{"path mode is unchanged", false, map[string]string{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "websocket", "Origin": other}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newUIHarness(t, okBackend())
			host := "ui.example.com"
			if tc.host {
				h.srv.cfg.UIOriginTemplate = "https://{run}.ui.example.com"
				host = h.run.ID.String() + ".ui.example.com"
			}
			now := time.Now()
			cookie := &http.Cookie{Name: uiCookieName, Value: h.srv.encodeUISession(uiSession{Run: h.run.ID, App: "code", Port: uiTestPort,
				Principal: h.owner, Expires: now.Add(time.Hour).Unix(), IssuedAt: now.Unix(), AuthorizedAt: now.Unix()})}
			rec := h.relay("/", cookie, func(r *http.Request) {
				r.Host = host
				for k, v := range tc.headers {
					r.Header.Set(k, strings.ReplaceAll(v, "{run}", h.run.ID.String()))
				}
			})
			if rec.Code != tc.want {
				t.Fatalf("relay = %d %s, want %d", rec.Code, rec.Body.String(), tc.want)
			}
			if tc.want == http.StatusForbidden && (respondedReason(t, rec) != reasonUIGatewayCrossSite || !h.audit.hasDataValue("reason", inboundDenyCrossSite)) {
				t.Errorf("refusal = %s, reasons %s; want %s audited cross_site", rec.Body.String(), h.audit.dataReasons(), reasonUIGatewayCrossSite)
			}
		})
	}
}

// TestUIGateway_RunnerOfflineIs503: an exec the runner cannot take because it is offline is a retryable
// 503 runner_offline, not a 502.
func TestUIGateway_RunnerOfflineIs503(t *testing.T) {
	h := newUIHarness(t, okBackend())
	cookie := h.openSession()
	h.runner.execFn = func(spec runner.ExecSpec) (*runner.ExecSession, error) {
		if spec.Argv[0] == "sh" {
			return &runner.ExecSession{Wait: func() (int, error) { return 0, nil }}, nil
		}
		return nil, runner.ErrRunnerOffline
	}
	rec := h.relay("/", cookie, nil)
	if rec.Code != http.StatusServiceUnavailable || respondedReason(t, rec) != string(placement.ReasonRunnerOffline) {
		t.Fatalf("relay = %d %s, want 503 %s", rec.Code, rec.Body.String(), placement.ReasonRunnerOffline)
	}
}

// TestUIGateway_HealthzPublishesInboundTemplate: an API client is told where to call an inbound app.
func TestUIGateway_HealthzPublishesInboundTemplate(t *testing.T) {
	h := newUIHarness(t, okBackend())
	if got := h.srv.uiSandboxHealthz()["inbound_url_template"]; got != "https://ui.example.com/r/{run}/{app}/" {
		t.Fatalf("inbound_url_template = %v", got)
	}
	h.srv.cfg.UIOriginTemplate = "https://{run}.ui.example.com"
	if got := h.srv.uiSandboxHealthz()["inbound_url_template"]; got != "https://{run}.ui.example.com/r/{run}/{app}/" {
		t.Fatalf("host-mode inbound_url_template = %v", got)
	}
}
