// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// newMCPRestartFixture is a run dispatched with its one server ready, and its sandbox up.
func newMCPRestartFixture(t *testing.T) *mcpDispatchFixture {
	t.Helper()
	f := newMCPDispatchFixture(t)
	f.dispatch()
	if f.state() != types.RunRunning || f.st.mcpRowsMem.get(0).State != store.MCPSandboxReady || f.sbx.count("create") != 1 {
		t.Fatalf("dispatch left run %s row %+v creates %d", f.state(), f.st.mcpRowsMem.get(0), f.sbx.count("create"))
	}
	return f
}

// crash is the server's sandbox dying under it.
func (f *mcpDispatchFixture) crash() {
	f.sbx.mu.Lock()
	defer f.sbx.mu.Unlock()
	f.sbx.live[0] = false
}

func (f *mcpDispatchFixture) pass() bool { return f.srv.mcpWatchPass(context.Background(), f.run.ID) }

// setRun changes the stored run as a writer would.
func (f *mcpDispatchFixture) setRun(fn func(r *types.AgentRun, state *types.RunState)) {
	f.st.dispatchTestStore.mu.Lock()
	defer f.st.dispatchTestStore.mu.Unlock()
	fn(&f.st.dispatchTestStore.run, &f.st.dispatchTestStore.state)
}

func (f *mcpDispatchFixture) lastMCPSpec() runner.MCPSandboxSpec {
	f.sbx.mu.Lock()
	defer f.sbx.mu.Unlock()
	return f.sbx.specs[len(f.sbx.specs)-1]
}

// TestMCPWatcherRestartsCrashedServer: a dead sandbox of a live run is restarted with a fresh
// MCP-audience token, the old one kept until the grace, on the same relay token and rank, so the agent's proxy
// reaches it at its pinned address; the row is ready again and counts one restart.
func TestMCPWatcherRestartsCrashedServer(t *testing.T) {
	f := newMCPRestartFixture(t)
	first, old := f.lastMCPSpec(), f.st.mcpRowsMem.get(0)
	f.crash()
	if f.pass() {
		t.Fatal("the watcher ended on a live run")
	}
	row := f.st.mcpRowsMem.get(0)
	if f.sbx.count("create") != 2 || f.sbx.left() != 1 || row.State != store.MCPSandboxReady || row.Restarts != 1 {
		t.Fatalf("creates %d left %d row %+v", f.sbx.count("create"), f.sbx.left(), row)
	}
	if row.CPTokenJTI == old.CPTokenJTI || row.PrevCPTokenJTI != old.CPTokenJTI || row.PrevCPTokenUntil == nil ||
		row.PrevCPTokenUntil.Before(time.Now().Add(terminalUploadGrace-time.Minute)) {
		t.Errorf("token jti %q prev %q until %v: want a fresh one, the old kept until the grace", row.CPTokenJTI, row.PrevCPTokenJTI, row.PrevCPTokenUntil)
	}
	again := f.lastMCPSpec()
	if again.RelayTokenSHA256 != first.RelayTokenSHA256 || again.RelayRank != first.RelayRank || row.RelayAddr != old.RelayAddr {
		t.Errorf("relay token, rank or address moved: %q/%q %d/%d %q/%q", again.RelayTokenSHA256, first.RelayTokenSHA256,
			again.RelayRank, first.RelayRank, row.RelayAddr, old.RelayAddr)
	}
	if !f.audit.has(f.run.ID, "run.mcp_sandbox.restart", "success") {
		t.Error("no run.mcp_sandbox.restart success row")
	}
}

// TestMCPRestartWindowStopsAtLimit (Q6 A): the watcher restarts a server at most three times in ten minutes,
// then stops its row with cause restart_limit.
func TestMCPRestartWindowStopsAtLimit(t *testing.T) {
	f := newMCPRestartFixture(t)
	for range mcpRestartMax + 1 {
		f.crash()
		f.pass()
	}
	row := f.st.mcpRowsMem.get(0)
	if f.sbx.count("create") != 1+mcpRestartMax || row.State != store.MCPSandboxStopped || row.LastCause != mcpEndLimit || f.sbx.left() != 0 {
		t.Fatalf("creates %d row %s (%q) left %d: want %d restarts, then stopped", f.sbx.count("create"), row.State, row.LastCause, f.sbx.left(), mcpRestartMax)
	}
}

// mcpGenStore is the restart fixture's store with the run's policy generation.
type mcpGenStore struct {
	*mcpDispatchStore
	gen int
}

func (s *mcpGenStore) RunPolicyState(context.Context, uuid.UUID) (types.RunPolicyState, bool, error) {
	return types.RunPolicyState{Generations: []types.PolicyGeneration{{Generation: s.gen}}}, true, nil
}

func (s *mcpGenStore) AdvanceRunPolicyGeneration(context.Context, uuid.UUID, store.PolicySlot, string, []string, string, []byte) (int, error) {
	return 0, errors.New("not kept")
}

func (s *mcpGenStore) AbandonRunPolicyGeneration(context.Context, uuid.UUID, int, []byte) error {
	return errors.New("not kept")
}

func (s *mcpGenStore) MarkRunLostAtGeneration(context.Context, uuid.UUID, int, types.LostReason, time.Time, time.Duration) (bool, error) {
	return false, errors.New("not kept")
}

func (s *mcpGenStore) RunLostFate(context.Context, uuid.UUID) (store.RunLostFate, error) {
	return store.RunLostFate{}, errors.New("not kept")
}

func (s *mcpGenStore) RestoreRunLostFateAtGeneration(context.Context, uuid.UUID, int, store.RunLostFate) (bool, error) {
	return false, errors.New("not kept")
}

var _ store.RunPolicyGenerations = (*mcpGenStore)(nil)

// TestMCPWatcherNoRestartAfterCut (F1, MCP1R-F1, MCP51-F1, MCP52-F2): in each state a cut leaves the run in,
// the watcher creates nothing and a restart kills what may run; on a RUNNING run with no cut it restarts.
func TestMCPWatcherNoRestartAfterCut(t *testing.T) {
	now := time.Now()
	runner1 := uuid.New()
	for _, tc := range []struct {
		name    string
		set     func(f *mcpDispatchFixture)
		restart bool
	}{
		{"killed", func(f *mcpDispatchFixture) {
			f.setRun(func(_ *types.AgentRun, s *types.RunState) { *s = types.RunKilled })
		}, false},
		{"completed", func(f *mcpDispatchFixture) {
			f.setRun(func(_ *types.AgentRun, s *types.RunState) { *s = types.RunCompleted })
		}, false},
		{"failed", func(f *mcpDispatchFixture) {
			f.setRun(func(_ *types.AgentRun, s *types.RunState) { *s = types.RunFailed })
		}, false},
		{"lost outage", func(f *mcpDispatchFixture) {
			f.setRun(func(r *types.AgentRun, _ *types.RunState) { r.LostAt, r.LostReason = &now, types.LostOutage })
		}, false},
		{"lost reboot", func(f *mcpDispatchFixture) {
			f.setRun(func(r *types.AgentRun, _ *types.RunState) { r.LostAt, r.LostReason = &now, types.LostReboot })
		}, false},
		{"unwritten mark, generation 0", func(f *mcpDispatchFixture) { f.srv.lostCuts.Store(f.run.ID, 0) }, false},
		{"unwritten mark, then a revive that succeeded", func(f *mcpDispatchFixture) {
			f.srv.lostCuts.Store(f.run.ID, 0)
			f.srv.lostCuts.Delete(f.run.ID) // reviveRunProxy's delete beside leaseEnded
		}, true},
		{"cut at the current generation", func(f *mcpDispatchFixture) {
			f.srv.cfg.Store = &mcpGenStore{mcpDispatchStore: f.st, gen: 3}
			f.srv.lostCuts.Store(f.run.ID, 3)
		}, false},
		{"older cut, the run revived past it", func(f *mcpDispatchFixture) {
			f.srv.cfg.Store = &mcpGenStore{mcpDispatchStore: f.st, gen: 3}
			f.srv.lostCuts.Store(f.run.ID, 2)
		}, true},
		{"revive in flight", func(f *mcpDispatchFixture) { f.srv.reviving.Store(f.run.ID, struct{}{}) }, false},
		{"containment pending", func(f *mcpDispatchFixture) {
			f.setRun(func(r *types.AgentRun, _ *types.RunState) { r.Placement, r.RunnerID = types.PlacementLocal, &runner1 })
			f.st.pending = true
		}, false},
		{"running, no cut", func(*mcpDispatchFixture) {}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMCPRestartFixture(t)
			f.crash()
			tc.set(f)
			f.pass()
			if got := f.sbx.count("create") == 2; got != tc.restart {
				t.Fatalf("watcher restarted %v, want %v (creates %d)", got, tc.restart, f.sbx.count("create"))
			}
			if tc.restart {
				return
			}
			kills := f.sbx.count("kill")
			cur, _ := f.st.dispatchTestStore.GetRun(context.Background(), f.run.ID)
			if err := f.srv.restartMCPSandbox(context.Background(), cur, f.st.mcpRowsMem.get(0).ServerKey, types.ActorHuman, cur.CreatedBy, false); !errors.Is(err, errMCPRestartRunEnded) {
				t.Errorf("restart = %v, want errMCPRestartRunEnded", err)
			}
			if f.sbx.count("kill") <= kills || f.sbx.count("create") != 1 {
				t.Errorf("restart kills %d -> %d, creates %d: want a kill and no create", kills, f.sbx.count("kill"), f.sbx.count("create"))
			}
		})
	}
	t.Run("store read error", func(t *testing.T) {
		f := newMCPRestartFixture(t)
		f.crash()
		f.st.getRunErr = errors.New("database unavailable")
		f.pass()
		if _, ok := f.srv.mcpCreateAllowed(context.Background(), f.run); ok || f.sbx.count("create") != 1 {
			t.Fatalf("a read error allowed a create (creates %d)", f.sbx.count("create"))
		}
	})
	t.Run("run lock held", func(t *testing.T) {
		f := newMCPRestartFixture(t)
		f.crash()
		_, unlock, ok := f.srv.tryLockRunOp(context.Background(), f.run.ID)
		if !ok {
			t.Fatal("take the run lock")
		}
		f.pass()
		kills := f.sbx.count("kill")
		if f.sbx.count("create") != 1 {
			t.Fatalf("the watcher acted under a lock it does not hold (creates %d)", f.sbx.count("create"))
		}
		unlock()
		f.setRun(func(_ *types.AgentRun, s *types.RunState) { *s = types.RunCompleted }) // what the lock holder leaves
		f.pass()
		if f.sbx.count("create") != 1 || f.sbx.count("kill") < kills {
			t.Errorf("after the end: creates %d", f.sbx.count("create"))
		}
	})
	// A state change between the check and the start (MCP1R-F3): the ready write is refused and what started
	// is killed.
	for name, change := range map[string]func(r *types.AgentRun, s *types.RunState){
		"killed":      func(_ *types.AgentRun, s *types.RunState) { *s = types.RunKilled },
		"marked lost": func(r *types.AgentRun, _ *types.RunState) { r.LostAt, r.LostReason = &now, types.LostOutage },
	} {
		t.Run("race: "+name, func(t *testing.T) {
			f := newMCPRestartFixture(t)
			f.crash()
			f.sbx.createFn = func(context.Context, runner.MCPSandboxSpec) error { f.setRun(change); return nil }
			f.pass()
			if row := f.st.mcpRowsMem.get(0); row.State == store.MCPSandboxReady || f.sbx.left() != 0 {
				t.Fatalf("row %s left %d: want none ready, none left", row.State, f.sbx.left())
			}
		})
	}
	t.Run("cut recorded between the check and the ready write", func(t *testing.T) {
		f := newMCPRestartFixture(t)
		f.crash()
		f.sbx.createFn = func(context.Context, runner.MCPSandboxSpec) error { f.srv.lostCuts.Store(f.run.ID, 0); return nil }
		f.pass()
		if row := f.st.mcpRowsMem.get(0); row.State != store.MCPSandboxStopped || row.LastCause != mcpEndLost || f.sbx.left() != 0 {
			t.Fatalf("row %s (%q) left %d: want ended lost", row.State, row.LastCause, f.sbx.left())
		}
	})
}

// TestMCPRestartAfterRenewResolvesItsGrant: after a renew rotated the token, a restart mints a fresh
// one, keeps the renewed one as the previous until the grace, and the new proxy's header injection still
// names the grant the row holds, so its resolve is served.
func TestMCPRestartAfterRenewResolvesItsGrant(t *testing.T) {
	f := newMCPRestartFixture(t)
	f.st.dispatchTestStore.mu.Lock()
	f.st.dispatchTestStore.grants = []types.CredentialGrant{{ID: f.keyID, Spec: componentGrant(mcpFilesDef().Secrets[1])}}
	f.st.dispatchTestStore.mu.Unlock()
	old := f.st.mcpRowsMem.get(0)
	if ok, err := f.st.RotateMCPSandboxToken(context.Background(), f.run.ID, 0, old.CPTokenJTI, "renewed-jti", time.Now().Add(time.Minute)); err != nil || !ok {
		t.Fatalf("rotate = %v, %v", ok, err)
	}
	f.crash()
	f.pass()
	row := f.st.mcpRowsMem.get(0)
	if row.State != store.MCPSandboxReady || row.PrevCPTokenJTI != "renewed-jti" || row.CPTokenJTI == "renewed-jti" || row.CPTokenJTI == "" {
		t.Fatalf("row %+v: want ready on a fresh token, the renewed one previous", row)
	}
	var pc proxy.Config
	if err := json.Unmarshal(f.lastMCPSpec().ProxyConfig, &pc); err != nil {
		t.Fatal(err)
	}
	if len(pc.Injection) != 1 || pc.Injection[0].GrantID != f.keyID || len(row.GrantIDs) != 1 || row.GrantIDs[0] != f.keyID {
		t.Errorf("injection %+v grants %v: want the row's grant", pc.Injection, row.GrantIDs)
	}
	if pc.ControlPlaneAudience != mcpSandboxAudience || pc.RunToken == "" {
		t.Errorf("audience %q", pc.ControlPlaneAudience)
	}
}

// mcpRouteCall calls one of the run page's MCP routes as the run's owner.
func (f *mcpDispatchFixture) mcpRouteCall(t *testing.T, h http.HandlerFunc, method, key, verb string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/api/v1/runs/" + f.run.ID.String() + "/mcp-servers"
	if key != "" {
		path += "/" + key + "/" + verb
	}
	r := httptest.NewRequest(method, path, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", f.run.ID.String())
	if key != "" {
		rctx.URLParams.Add("key", key)
	}
	r = r.WithContext(context.WithValue(withOIDCHuman(r.Context(), f.run.CreatedBy), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

// TestMCPRestartRouteRefusedOnLostRun: the owner's Restart on a lost run (outage and reboot), and on a run
// whose containment is pending, answers 409 mcp_sandbox_run_ended and creates nothing.
func TestMCPRestartRouteRefusedOnLostRun(t *testing.T) {
	now, rn := time.Now(), uuid.New()
	for name, set := range map[string]func(f *mcpDispatchFixture){
		"outage": func(f *mcpDispatchFixture) {
			f.setRun(func(r *types.AgentRun, _ *types.RunState) { r.LostAt, r.LostReason = &now, types.LostOutage })
		},
		"reboot": func(f *mcpDispatchFixture) {
			f.setRun(func(r *types.AgentRun, _ *types.RunState) { r.LostAt, r.LostReason = &now, types.LostReboot })
		},
		"containment pending": func(f *mcpDispatchFixture) {
			f.setRun(func(r *types.AgentRun, _ *types.RunState) { r.Placement, r.RunnerID = types.PlacementLocal, &rn })
			f.st.pending = true
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newMCPRestartFixture(t)
			set(f)
			key := f.st.mcpRowsMem.get(0).ServerKey
			w := f.mcpRouteCall(t, f.srv.handleRestartRunMCPServer, http.MethodPost, key, "restart")
			if w.Code != http.StatusConflict || errorReason(w) != reasonMCPSandboxRunEnded || f.sbx.count("create") != 1 {
				t.Fatalf("restart = %d %s, creates %d: want 409 %s and no create", w.Code, w.Body.String(), f.sbx.count("create"), reasonMCPSandboxRunEnded)
			}
		})
	}
}

// TestMCPRestartRefusedAfterKill: a killed run's Restart and Stop each answer 409 mcp_sandbox_run_ended and
// create nothing; an unknown key answers 404 mcp_sandbox_unknown; the owner's Restart of a live run restarts.
func TestMCPRestartRefusedAfterKill(t *testing.T) {
	f := newMCPRestartFixture(t)
	key := f.st.mcpRowsMem.get(0).ServerKey
	if w := f.mcpRouteCall(t, f.srv.handleRestartRunMCPServer, http.MethodPost, "nope", "restart"); w.Code != http.StatusNotFound || errorReason(w) != reasonMCPSandboxUnknown {
		t.Errorf("unknown key = %d %s", w.Code, w.Body.String())
	}
	if w := f.mcpRouteCall(t, f.srv.handleRestartRunMCPServer, http.MethodPost, key, "restart"); w.Code != http.StatusOK || f.sbx.count("create") != 2 {
		t.Fatalf("live restart = %d %s, creates %d", w.Code, w.Body.String(), f.sbx.count("create"))
	}
	f.setRun(func(_ *types.AgentRun, s *types.RunState) { *s = types.RunKilled })
	for _, verb := range []string{"restart", "stop"} {
		h := f.srv.handleRestartRunMCPServer
		if verb == "stop" {
			h = f.srv.handleStopRunMCPServer
		}
		// The handler reads the run as the request's load does: the store's.
		if w := f.mcpRouteCall(t, h, http.MethodPost, key, verb); w.Code != http.StatusConflict || errorReason(w) != reasonMCPSandboxRunEnded {
			t.Errorf("%s on a killed run = %d %s", verb, w.Code, w.Body.String())
		}
	}
	if f.sbx.count("create") != 2 {
		t.Errorf("creates %d after the kill, want none more", f.sbx.count("create"))
	}
}

// TestMCPStopRoute (OP53-1): a person's Stop stops the row (person) before it kills the sandbox, writes its
// end row as that person, and the watcher never restarts a stopped row.
func TestMCPStopRoute(t *testing.T) {
	f := newMCPRestartFixture(t)
	key := f.st.mcpRowsMem.get(0).ServerKey
	w := f.mcpRouteCall(t, f.srv.handleStopRunMCPServer, http.MethodPost, key, "stop")
	if w.Code != http.StatusOK {
		t.Fatalf("stop = %d %s", w.Code, w.Body.String())
	}
	row := f.st.mcpRowsMem.get(0)
	if row.State != store.MCPSandboxStopped || row.LastCause != mcpEndPerson || f.sbx.left() != 0 {
		t.Fatalf("row %s (%q) left %d", row.State, row.LastCause, f.sbx.left())
	}
	ends := f.audit.eventsFor(f.run.ID, "run.mcp_sandbox.end")
	if len(ends) == 0 || ends[len(ends)-1].ActorType != types.ActorHuman || ends[len(ends)-1].Actor != f.run.CreatedBy {
		t.Errorf("end rows %+v: want the person's", ends)
	}
	f.pass()
	if f.sbx.count("create") != 1 {
		t.Errorf("the watcher restarted a stopped row")
	}
	if w := f.mcpRouteCall(t, f.srv.handleListRunMCPServers, http.MethodGet, "", ""); w.Code != http.StatusOK {
		t.Errorf("list = %d", w.Code)
	} else {
		var body struct {
			Servers []mcpServerView `json:"mcp_servers"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Servers) != 1 || body.Servers[0].State != store.MCPSandboxStopped {
			t.Errorf("list body %s", w.Body.String())
		}
	}
}

// TestMCPSettleStartingRow (MCP51-F4): a starting row whose sandbox is live goes ready; one not live past the
// wait goes failed (readiness) and is killed.
func TestMCPSettleStartingRow(t *testing.T) {
	f := newMCPRestartFixture(t)
	row := f.st.mcpRowsMem.get(0)
	if ok, err := f.st.ReopenMCPSandboxIfLive(context.Background(), f.run.ID, 0, row.UpdatedAt, "jti-x", time.Now()); err != nil || !ok {
		t.Fatalf("reopen = %v, %v", ok, err)
	}
	f.pass()
	if got := f.st.mcpRowsMem.get(0).State; got != store.MCPSandboxReady {
		t.Fatalf("live starting row settled to %s, want ready", got)
	}
	row = f.st.mcpRowsMem.get(0)
	if ok, _ := f.st.ReopenMCPSandboxIfLive(context.Background(), f.run.ID, 0, row.UpdatedAt, "jti-y", time.Now()); !ok {
		t.Fatal("reopen")
	}
	f.crash()
	wait := mcpReadyWait
	mcpReadyWait = -time.Hour // past the wait whatever the fake store's clock reads
	t.Cleanup(func() { mcpReadyWait = wait })
	f.pass()
	if row := f.st.mcpRowsMem.get(0); row.State != store.MCPSandboxFailed || row.LastCause != mcpFailReadiness || f.sbx.left() != 0 {
		t.Fatalf("dead starting row = %s (%q) left %d, want failed readiness", row.State, row.LastCause, f.sbx.left())
	}
}

// TestMCPWatcherTearsDownFailedRow (MCP4A-renew): a stopped row whose kill failed keeps a live proxy; the next
// pass kills it, so no failed or stopped row keeps a proxy that renews its token.
func TestMCPWatcherTearsDownFailedRow(t *testing.T) {
	f := newMCPRestartFixture(t)
	key := f.st.mcpRowsMem.get(0).ServerKey
	f.sbx.mu.Lock()
	f.sbx.killErr = errors.New("engine busy")
	f.sbx.mu.Unlock()
	f.mcpRouteCall(t, f.srv.handleStopRunMCPServer, http.MethodPost, key, "stop")
	if f.st.mcpRowsMem.get(0).State != store.MCPSandboxStopped || f.sbx.left() != 1 {
		t.Fatalf("row %s left %d: want stopped with its sandbox still up", f.st.mcpRowsMem.get(0).State, f.sbx.left())
	}
	f.sbx.mu.Lock()
	f.sbx.killErr = nil
	f.sbx.mu.Unlock()
	f.pass()
	if f.sbx.left() != 0 || f.sbx.count("create") != 1 {
		t.Fatalf("left %d creates %d: want the stopped row's sandbox killed and nothing created", f.sbx.left(), f.sbx.count("create"))
	}
}

// TestNoRouteTakesASandboxRef (I7): no route names a sandbox or container by its ref, and the run page's MCP
// server rows name a server by its key and carry no ref.
func TestNoRouteTakesASandboxRef(t *testing.T) {
	for route := range routeMatrix {
		for _, p := range []string{"{ref}", "{sandbox_ref}", "{sandbox}", "{container}"} {
			if strings.Contains(route, p) {
				t.Errorf("%s takes %s", route, p)
			}
		}
	}
	b, err := json.Marshal(mcpServerView{Key: "files"})
	if err != nil || strings.Contains(string(b), "ref") {
		t.Errorf("an MCP server row = %s (%v), want no ref", b, err)
	}
}

// TestMCPRestartStopDuringCreateStartsNothing: a person's Stop that lands while a Restart creates the server
// makes the record refused, so what was created is killed and never started, and the Restart answers 409.
func TestMCPRestartStopDuringCreateStartsNothing(t *testing.T) {
	f := newMCPRestartFixture(t)
	f.crash()
	f.sbx.createFn = func(context.Context, runner.MCPSandboxSpec) error {
		if _, err := f.st.mcpRowsMem.StopMCPSandboxRow(context.Background(), f.run.ID, 0, mcpEndPerson); err != nil {
			t.Errorf("stop: %v", err)
		}
		return nil
	}
	w := f.mcpRouteCall(t, f.srv.handleRestartRunMCPServer, http.MethodPost, "files", "restart")
	if w.Code != http.StatusConflict || errorReason(w) != reasonMCPSandboxRunEnded {
		t.Fatalf("restart = %d %s, want 409 %s", w.Code, w.Body.String(), reasonMCPSandboxRunEnded)
	}
	if f.sbx.count("create") != 2 || f.sbx.count("start") != 1 || f.sbx.left() != 0 {
		t.Fatalf("creates %d starts %d left %d: want the second create killed, never started", f.sbx.count("create"), f.sbx.count("start"), f.sbx.left())
	}
	if row := f.st.mcpRowsMem.get(0); row.State != store.MCPSandboxStopped || row.LastCause != mcpEndPerson {
		t.Errorf("row %s (%q), want stopped by the person", row.State, row.LastCause)
	}
}

// TestMCPRestartOfStartingRowOrStoreError: a person's Restart of a server still starting answers 409
// mcp_sandbox_starting and kills nothing; a Restart whose read or write of the rows fails answers 503
// mcp_sandbox_unavailable, never the run-ended answer.
func TestMCPRestartOfStartingRowOrStoreError(t *testing.T) {
	for name, tc := range map[string]struct {
		set    func(f *mcpDispatchFixture)
		code   int
		reason string
	}{
		"starting": {func(f *mcpDispatchFixture) {
			f.st.mcpRowsMem.mu.Lock()
			f.st.mcpRowsMem.rows[0].State = store.MCPSandboxStarting
			f.st.mcpRowsMem.mu.Unlock()
		}, http.StatusConflict, reasonMCPSandboxStarting},
		"rows unreadable": {func(f *mcpDispatchFixture) { f.st.mcpRowsMem.listErr = errors.New("store down") },
			http.StatusServiceUnavailable, reasonMCPSandboxUnavailable},
		"restart write fails": {func(f *mcpDispatchFixture) { f.st.mcpRowsMem.restartErr = errors.New("store down") },
			http.StatusServiceUnavailable, reasonMCPSandboxUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			f := newMCPRestartFixture(t)
			tc.set(f)
			kills := f.sbx.count("kill")
			w := f.mcpRouteCall(t, f.srv.handleRestartRunMCPServer, http.MethodPost, "files", "restart")
			if w.Code != tc.code || errorReason(w) != tc.reason {
				t.Fatalf("restart = %d %s, want %d %s", w.Code, w.Body.String(), tc.code, tc.reason)
			}
			if f.sbx.count("create") != 1 {
				t.Errorf("creates %d, want none past the dispatch's", f.sbx.count("create"))
			}
			if name == "starting" && (f.sbx.count("kill") != kills || f.sbx.left() != 1) {
				t.Errorf("kills %d -> %d left %d: a starting server's Restart killed it", kills, f.sbx.count("kill"), f.sbx.left())
			}
		})
	}
}

// TestMCPRestartPastItsBoundLeavesNoRestartingRow: a restart whose bound ends during the create (an image
// pull that took it all) kills what it made and fails the row on a bound of its own, so the row does not
// stay restarting with a container running.
func TestMCPRestartPastItsBoundLeavesNoRestartingRow(t *testing.T) {
	f := newMCPRestartFixture(t)
	f.crash()
	f.sbx.honourCtx, f.st.mcpRowsMem.honourCtx = true, true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.sbx.createFn = func(context.Context, runner.MCPSandboxSpec) error {
		cancel() // the bound ends as the create returns
		return nil
	}
	_ = f.srv.restartMCPSandbox(ctx, f.current(), "files", types.ActorSystem, "wardynd", true)
	if row := f.st.mcpRowsMem.get(0); row.State != store.MCPSandboxFailed {
		t.Fatalf("row %s (%q), want failed", row.State, row.LastCause)
	}
	if f.sbx.left() != 0 || !f.audit.has(f.run.ID, "run.mcp_sandbox.restart", "failure") {
		t.Errorf("left %d, restart failure row %v: want what was made killed and the failure written", f.sbx.left(),
			f.audit.has(f.run.ID, "run.mcp_sandbox.restart", "failure"))
	}
}
