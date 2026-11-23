// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// readCountingStore counts the run and grant reads a via_org request makes. Embedding the fixture's store keeps its
// method set, so the store's type assertions still hold.
type readCountingStore struct {
	localDispatchStore
	runs, grants atomic.Int32
}

func (c *readCountingStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	c.runs.Add(1)
	return c.localDispatchStore.GetRun(ctx, id)
}

func (c *readCountingStore) ListGrantsByRun(ctx context.Context, id uuid.UUID) ([]types.CredentialGrant, error) {
	c.grants.Add(1)
	return c.localDispatchStore.ListGrantsByRun(ctx, id)
}

// The via_org route's liveness check judges the run internalAuth's gate carried, and reads the run only when the
// gate carried none (or another run's); either way it refuses what it refused when it read the run itself.
func TestRefuseViaOrgEndedRun_JudgesTheGateRun(t *testing.T) {
	srv, ls, rec, run := localDispatchFixture(t, types.PlacementLocal)
	cs := &readCountingStore{localDispatchStore: ls}
	srv.cfg.Store = cs
	claims := &identity.Claims{RunID: run.ID}
	paused := time.Now()
	lostAt := time.Now()
	shapes := []struct {
		name   string
		set    func()
		refuse bool
		status int
		reason authz.Reason
	}{
		{"running", func() { ls.dispatchTestStore.state = types.RunRunning }, false, http.StatusOK, ""},
		{"paused", func() {
			ls.dispatchTestStore.state = types.RunRunning
			ls.dispatchTestStore.run.PausedAt = &paused
		}, true, http.StatusConflict, authz.ReasonRunPaused},
		{"kept", func() {
			ls.dispatchTestStore.run.PausedAt = nil
			ls.dispatchTestStore.run.LostAt, ls.dispatchTestStore.run.LostReason = &lostAt, types.LostEnded
		}, true, http.StatusForbidden, authz.ReasonRunKept},
		{"terminal", func() {
			ls.dispatchTestStore.run.LostAt, ls.dispatchTestStore.run.LostReason = nil, ""
			ls.dispatchTestStore.state = types.RunKilled
		}, true, http.StatusForbidden, authz.ReasonRunTerminal},
	}
	type answer struct {
		refused bool
		status  int
		body    string
		rows    []string
	}
	call := func(ctx context.Context) answer {
		before := len(rec.snapshot())
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, proxy.ViaOrgPathPrefix+uuid.NewString()+"/v1/x", nil)
		ctx = context.WithValue(ctx, claimsCtxKey{}, claims)
		refused := srv.refuseViaOrgEndedRun(w, r.WithContext(ctx), claims)
		var rows []string
		for _, ev := range rec.snapshot()[before:] {
			rows = append(rows, ev.Action+"|"+ev.Outcome+"|"+ev.Target+"|"+string(ev.Data))
		}
		return answer{refused, w.Code, w.Body.String(), rows}
	}
	for _, tc := range shapes {
		t.Run(tc.name, func(t *testing.T) {
			tc.set()
			gateRun, _ := ls.dispatchTestStore.GetRun(context.Background(), run.ID)
			cs.runs.Store(0)
			withGate := call(contextWithLiveRun(context.Background(), &gateRun))
			if got := cs.runs.Load(); got != 0 {
				t.Fatalf("with the gate's run, %d run reads; want 0", got)
			}
			cs.runs.Store(0)
			without := call(context.Background())
			if got := cs.runs.Load(); got != 1 {
				t.Fatalf("without the gate's run, %d run reads; want 1", got)
			}
			if withGate.refused != tc.refuse || withGate.status != tc.status || (tc.reason != "" && errorReasonOf(withGate.body) != string(tc.reason)) {
				t.Fatalf("with the gate's run: refused %v, %d %s; want refused %v, %d %s", withGate.refused, withGate.status, withGate.body, tc.refuse, tc.status, tc.reason)
			}
			if withGate.refused != without.refused || withGate.status != without.status || withGate.body != without.body ||
				strings.Join(withGate.rows, "\n") != strings.Join(without.rows, "\n") {
				t.Fatalf("the two answers differ:\n gate %+v\n read %+v", withGate, without)
			}
			if tc.refuse && len(withGate.rows) != 1 {
				t.Fatalf("audited rows = %v, want one", withGate.rows)
			}
			// A gate run of another run is not this request's: the store is read.
			other := gateRun
			other.ID = uuid.New()
			cs.runs.Store(0)
			if got := call(contextWithLiveRun(context.Background(), &other)); got.refused != tc.refuse || got.status != tc.status {
				t.Fatalf("with another run's row: refused %v, %d; want refused %v, %d", got.refused, got.status, tc.refuse, tc.status)
			}
			if got := cs.runs.Load(); got != 1 {
				t.Fatalf("with another run's row, %d run reads; want 1", got)
			}
		})
	}
}

// A grant viaOrgGrant has checked is held for the run's via_org state, so a later request reads the grants again
// only for a grant not held yet, after the state is released, or when the relaying runner is not the state's.
func TestViaOrgGrant_ReadsTheGrantsOncePerRunState(t *testing.T) {
	const keyHost, key2Host = "api.example.com", "api2.example.com"
	srv, ls, _, run := localDispatchFixture(t, types.PlacementLocal)
	cs := &readCountingStore{localDispatchStore: ls}
	srv.cfg.Store = cs
	gh := types.CredentialGrant{ID: uuid.New(), RunID: run.ID, Delivery: types.GrantDeliveryViaOrg, Spec: types.GrantSpec{Kind: types.GrantGitHubToken}}
	apiKey := func(host string) types.CredentialGrant {
		scope, _ := json.Marshal(map[string]any{"host": host, "header": "X-Api-Key", "secret_name": "EXAMPLE_API_KEY", "require_tls": true})
		return types.CredentialGrant{ID: uuid.New(), RunID: run.ID, Delivery: types.GrantDeliveryViaOrg, Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: scope}}
	}
	key, key2 := apiKey(keyHost), apiKey(key2Host)
	ls.dispatchTestStore.grants = []types.CredentialGrant{gh, key, key2}
	ls.dispatchTestStore.state = types.RunRunning
	runnerID := uuid.New()
	newState := func() *viaOrgRun {
		eng, err := proxy.NewViaOrgEngine(context.Background(), &proxy.Config{
			RunID: run.ID, ControlPlaneURL: "http://127.0.0.1:9", RunToken: "run-token", DecisionBufferSize: 16,
		}, proxy.ViaOrgEngineOptions{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = eng.Close(context.Background()) })
		return &viaOrgRun{id: run.ID, runner: runnerID, bytes: new(atomic.Int64), done: make(chan struct{}),
			streams: map[*viaOrgStream]struct{}{}, idleSince: time.Now(), engine: &viaOrgGen{engine: eng},
			routes:  []proxy.ViaOrgRoute{{GrantID: gh.ID, Host: githubHost}, {GrantID: key.ID, Host: keyHost}, {GrantID: key2.ID, Host: key2Host}},
			byteCap: viaOrgMaxBytes, idle: time.Minute, period: time.Hour}
	}
	state := newState()
	srv.viaOrg.runs = map[uuid.UUID]*viaOrgRun{run.ID: state}
	serve := func(g types.CredentialGrant, relayer uuid.UUID, rest, authority string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, proxy.ViaOrgPathPrefix+g.ID.String()+rest, nil)
		r.Header.Set(proxy.ViaOrgAuthorityHeader, authority)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("grantID", g.ID.String())
		ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
		ctx = context.WithValue(ctx, claimsCtxKey{}, &identity.Claims{RunID: run.ID})
		ctx = context.WithValue(ctx, relayRunnerKey{}, relayer)
		w := httptest.NewRecorder()
		srv.handleViaOrg(w, r.WithContext(ctx))
		return w
	}
	reads := func(want int32, what string) {
		t.Helper()
		if got := cs.grants.Load(); got != want {
			t.Fatalf("%s: %d grant reads, want %d", what, got, want)
		}
	}

	serve(key, runnerID, "/v1/models", keyHost+":443")
	serve(key, runnerID, "/v1/models", keyHost+":443")
	reads(1, "two requests for the api_key grant")
	serve(gh, runnerID, "/wardyn/gh/octocat/hello-world/info/refs", githubHost)
	reads(2, "a GitHub grant not held yet")

	srv.releaseViaOrgRun(state, false, false)
	state = newState()
	srv.viaOrg.mu.Lock()
	srv.viaOrg.runs[run.ID] = state
	srv.viaOrg.mu.Unlock()
	serve(key, runnerID, "/v1/models", keyHost+":443")
	reads(3, "the first request after the state was replaced")

	// Another runner's relay gets the mismatch and holds nothing: the right runner's request still reads.
	if w := serve(key2, uuid.New(), "/v1/models", key2Host+":443"); errorReason(w) != string(placement.ReasonRelayRunnerMismatch) {
		t.Fatalf("a request relayed by another runner = %d %s, want %s", w.Code, w.Body, placement.ReasonRelayRunnerMismatch)
	}
	reads(4, "the request another runner relayed")
	serve(key2, runnerID, "/v1/models", key2Host+":443")
	reads(5, "the right runner's request after a mismatch")
}

// The via_org grants a run's state holds are only sound while credential_grants rows never change after their
// insert: this walks the module's non-test code for a statement that would change or remove one.
func TestCredentialGrantsStayInsertOnly(t *testing.T) {
	root := filepath.Join("..", "..")
	changes := regexp.MustCompile(`(?i)(update\s+credential_grants|delete\s+from\s+credential_grants)`)
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if m := changes.Find(b); m != nil {
				t.Errorf("%s changes or removes credential_grants rows (%q): the grants a run's via_org state holds "+
					"(viaOrgRun.grants) and brokeredForgeMintKind (internal_mint.go) rely on the table being insert-only", path, m)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
