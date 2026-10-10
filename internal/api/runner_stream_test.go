// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type streamRunnerStore struct {
	store.Store
	store.RunnerStore
	row       types.Runner
	settings  *types.RunnerSettings
	configErr error
	lookups   *atomic.Int32
}

func (s streamRunnerStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{Runners: s.settings}, s.configErr
}

func (s streamRunnerStore) GetRunner(_ context.Context, id uuid.UUID) (types.Runner, error) {
	if s.lookups != nil {
		s.lookups.Add(1)
	}
	if id != s.row.ID {
		return types.Runner{}, store.ErrNotFound
	}
	return s.row, nil
}

func TestRunnerStreamPublicURLFailClosed(t *testing.T) {
	for _, raw := range []string{"", "http://org.example", "https://user@org.example", "https://org.example?", "https://org.example#"} {
		s := New(Config{RunnerOrgURL: raw, Store: streamRunnerStore{settings: &types.RunnerSettings{Enabled: true}}})
		r := httptest.NewRequest(http.MethodGet, "/api/v1/runners/"+uuid.NewString()+"/stream", nil)
		r.Host = "trusted.example"
		r.Header.Set("X-Forwarded-Host", "trusted.example")
		r.Header.Set("X-Forwarded-Proto", "https")
		w := httptest.NewRecorder()
		panicFails(t, s.Handler()).ServeHTTP(w, r)
		if w.Code != http.StatusServiceUnavailable || errorReason(w) != "runner_stream_unavailable" || !strings.Contains(w.Body.String(), "WARDYN_RUNNER_ORG_URL") {
			t.Fatalf("%q: %d %s", raw, w.Code, w.Body.String())
		}
	}
}

func TestRunnerStreamPreHandshakeReasons(t *testing.T) {
	st := &fakeSiteConfigStore{cfg: types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true}}}
	s, _ := newSiteConfigHarness(t, st)
	s.cfg.RunnerOrgURL = "https://org.example"
	w := do(t, s, http.MethodGet, "/api/v1/runners/"+uuid.NewString()+"/stream", "", "")
	if w.Code != http.StatusNotImplemented || errorReason(w) != "runner_stream_unavailable" {
		t.Fatalf("missing runner store: %d %s", w.Code, w.Body.String())
	}
	var lookups atomic.Int32
	s = New(Config{RunnerOrgURL: "https://org.example", Now: func() time.Time { return time.Unix(100, 0) },
		Store: streamRunnerStore{settings: &types.RunnerSettings{Enabled: true}, lookups: &lookups}})
	s.runnerStreams.limiter = principalLimiter{rate: 1, burst: 1, max: 1}
	for _, want := range []struct {
		status int
		reason string
	}{{http.StatusBadRequest, "invalid_id_param"}, {http.StatusTooManyRequests, "runner_stream_rate_limited"}} {
		w := do(t, s, http.MethodGet, "/api/v1/runners/not-an-id/stream", "", "")
		if w.Code != want.status || errorReason(w) != want.reason || lookups.Load() != 0 || s.runnerStreams.hub != nil {
			t.Fatalf("pre-handshake refusal: %d %s lookup=%d hub=%v", w.Code, w.Body.String(), lookups.Load(), s.runnerStreams.hub != nil)
		}
	}
}

func TestRunnerStreamBearerCannotReplaceSignature(t *testing.T) {
	for _, token := range []string{"", "test-admin", deviceTokenPrefix + "test-device", "delegated-bearer"} {
		t.Run(token, func(t *testing.T) { testRunnerStreamSignature(t, token) })
	}
}

func testRunnerStreamSignature(t *testing.T, token string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, wrong, _ := ed25519.GenerateKey(rand.Reader)
	row := types.Runner{ID: uuid.New(), PublicKey: pub, OrgURLSHA256: federation.OrgURLSHA256("https://org.example"), State: types.RunnerClaimed}
	var admitted atomic.Int32
	s := New(Config{AdminToken: "test-admin", RunnerOrgURL: "https://org.example", BaseCtx: ctx,
		Store: streamRunnerStore{row: row, settings: &types.RunnerSettings{Enabled: true}}, RunnerHub: remote.HubConfig{
			Admit: func(context.Context, types.Runner) (remote.Admission, error) {
				admitted.Add(1)
				return remote.Admission{}, nil
			},
		},
	})
	server := httptest.NewServer(panicFails(t, s.Handler()))
	defer server.Close()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/api/v1/runners/"+row.ID.String()+"/stream",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	_, err = runnerwire.AuthenticateOrg(ctx, runnerwire.NewWebSocket(conn), row.ID.String(), "https://org.example", wrong, nil)
	if err == nil || admitted.Load() != 0 {
		t.Fatalf("forged key admitted: %v, callbacks %d", err, admitted.Load())
	}
}

func TestRunnerStreamDefaultOffAndConfigFailureHaveNoAuthSideEffects(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings *types.RunnerSettings
		err      error
		want     int
	}{
		{"absent", nil, nil, http.StatusForbidden},
		{"disabled", &types.RunnerSettings{}, nil, http.StatusForbidden},
		{"read failure", &types.RunnerSettings{Enabled: true}, errors.New("config unavailable"), http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lookups, admits atomic.Int32
			s := New(Config{RunnerOrgURL: "https://org.example",
				Store: streamRunnerStore{settings: tc.settings, configErr: tc.err, lookups: &lookups},
				RunnerHub: remote.HubConfig{Admit: func(context.Context, types.Runner) (remote.Admission, error) {
					admits.Add(1)
					return remote.Admission{}, nil
				}},
			})
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/api/v1/runners/"+uuid.NewString()+"/stream", nil)
			r.Header.Set("Connection", "Upgrade")
			r.Header.Set("Upgrade", "websocket")
			panicFails(t, s.Handler()).ServeHTTP(w, r)
			if w.Code != tc.want || lookups.Load() != 0 || admits.Load() != 0 || s.runnerStreams.hub != nil {
				t.Fatalf("status=%d lookup=%d admits=%d hub=%v", w.Code, lookups.Load(), admits.Load(), s.runnerStreams.hub != nil)
			}
		})
	}
}
