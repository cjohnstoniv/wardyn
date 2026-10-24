// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// vmSignInTestStore is the UI gateway's memory store plus VM sign-in rows and a site config.
type vmSignInTestStore struct {
	*uiMemStore
	vmu  sync.Mutex
	rows map[uuid.UUID]store.DesktopVMSignIns
	sc   types.SiteConfig
}

var _ store.DesktopVMSignInStore = (*vmSignInTestStore)(nil)

func (s *vmSignInTestStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return s.sc, nil
}

func (s *vmSignInTestStore) RecordDesktopVMSignIns(_ context.Context, runID uuid.UUID, st string) error {
	s.vmu.Lock()
	defer s.vmu.Unlock()
	s.rows[runID] = store.DesktopVMSignIns{Store: st}
	return nil
}

func (s *vmSignInTestStore) DesktopVMSignIns(_ context.Context, runID uuid.UUID) (store.DesktopVMSignIns, bool, error) {
	s.vmu.Lock()
	defer s.vmu.Unlock()
	row, ok := s.rows[runID]
	return row, ok, nil
}

func (s *vmSignInTestStore) PauseDesktopVMAgent(_ context.Context, runID uuid.UUID, by string, at time.Time) (bool, error) {
	s.vmu.Lock()
	defer s.vmu.Unlock()
	row, ok := s.rows[runID]
	if !ok || row.Store != "memory" || row.PausedAt != nil {
		return false, nil
	}
	row.PausedBy, row.PausedAt = by, &at
	s.rows[runID] = row
	return true, nil
}

func (s *vmSignInTestStore) ResumeDesktopVMAgent(_ context.Context, runID uuid.UUID) (string, time.Time, bool, error) {
	s.vmu.Lock()
	defer s.vmu.Unlock()
	row, ok := s.rows[runID]
	if !ok || row.PausedAt == nil {
		return "", time.Time{}, false, nil
	}
	by, at := row.PausedBy, *row.PausedAt
	row.PausedBy, row.PausedAt = "", nil
	s.rows[runID] = row
	return by, at, true, nil
}

// pauseFakeRunner is the guest agent behind runner.AgentPauser: it records each call.
type pauseFakeRunner struct {
	*sshFakeRunner
	pmu      sync.Mutex
	calls    []string
	pauseErr error
}

var _ runner.AgentPauser = (*pauseFakeRunner)(nil)

func (f *pauseFakeRunner) PauseAgent(_ context.Context, ref string) error {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	f.calls = append(f.calls, "pause "+ref)
	return f.pauseErr
}

func (f *pauseFakeRunner) ResumeAgent(_ context.Context, ref string) error {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	f.calls = append(f.calls, "resume "+ref)
	return nil
}

func (f *pauseFakeRunner) seen() []string {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	return slices.Clone(f.calls)
}

type vmSignInFixture struct {
	srv    *Server
	h      *harness
	st     *vmSignInTestStore
	rn     *pauseFakeRunner
	run    types.AgentRun
	cookie *http.Cookie
}

func newVMSignInFixture(t *testing.T, kept string) *vmSignInFixture {
	t.Helper()
	h := newHarness(t)
	run := types.AgentRun{ID: uuid.New(), CreatedBy: signinOwner, Agent: "claude-code", State: types.RunRunning, SandboxRef: "vm-1",
		MachineKind: types.MachineLinuxVM}
	st := &vmSignInTestStore{uiMemStore: newUIMemStore(), rows: map[uuid.UUID]store.DesktopVMSignIns{run.ID: {Store: kept}}}
	st.putRun(run)
	rn := &pauseFakeRunner{sshFakeRunner: &sshFakeRunner{}}
	cfg := baseTestConfig(h, st)
	cfg.Runner, cfg.OIDC = rn, &oidc.Authenticator{}
	return &vmSignInFixture{srv: New(cfg), h: h, st: st, rn: rn, run: run,
		cookie: ssoSession(t, signinOwner, "owner@corp.example", oidc.RoleUser)}
}

func (f *vmSignInFixture) do(t *testing.T, method, path string) (int, desktopSignInsView, string) {
	t.Helper()
	w := doSSO(t, f.srv, method, "/api/v1/runs/"+f.run.ID.String()+"/desktop/sign-ins"+path, f.cookie, "")
	var v desktopSignInsView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	return w.Code, v, w.Body.String()
}

func (f *vmSignInFixture) signInRows() []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range f.h.audit.snapshot() {
		if ev.Action == auditDesktopSignIn {
			out = append(out, ev)
		}
	}
	return out
}

// The policy knob's default is Allowed: no roster, a row without the field, and the zero value all
// allow sign-ins, and the field is absent from a row that never set it. An unreadable roster keeps
// none.
func TestDesktopSignIns_DefaultIsAllowed(t *testing.T) {
	var zero types.AgentProvider
	if zero.DesktopSignInsOff {
		t.Fatal("the zero agent row is Not allowed")
	}
	b, _ := json.Marshal(types.AgentProvider{ID: "claude-code"})
	if strings.Contains(string(b), "desktop_sign_ins_off") {
		t.Fatalf("an untouched row carries the knob: %s", b)
	}
	roster := types.SiteConfig{AgentProviders: &types.AgentProviders{Agents: []types.AgentProvider{{ID: "claude-code"}}}}
	for name, sc := range map[string]types.SiteConfig{"no roster": {}, "a row without the field": roster} {
		if desktopSignInsOff(sc, nil, "claude-code") {
			t.Errorf("%s: sign-ins are off", name)
		}
	}
	if !desktopSignInsOff(types.SiteConfig{}, errors.New("pool closed"), "claude-code") {
		t.Error("an unreadable roster allowed sign-ins")
	}
	f := newVMSignInFixture(t, "memory")
	code, v, body := f.do(t, http.MethodGet, "")
	if code != http.StatusOK || v.Store != "memory" || !v.CanPause || v.Paused || v.Agent != "Claude Code" {
		t.Fatalf("GET = %d %s, want memory, can pause, agent Claude Code", code, body)
	}
}

// "Pause the agent to sign in" stops the agent through the runner's guest agent (never the run's
// freeze), and the resume continues it and writes the session's one audit row.
func TestDesktopSignIns_PauseStopsTheAgentAndResumeAuditsTheSession(t *testing.T) {
	f := newVMSignInFixture(t, "memory")
	if code, v, body := f.do(t, http.MethodPost, "/pause"); code != http.StatusOK || !v.Paused || v.PausedAt == nil {
		t.Fatalf("pause = %d %s", code, body)
	}
	if got := f.rn.seen(); !slices.Equal(got, []string{"pause vm-1"}) {
		t.Fatalf("runner calls after pause = %v", got)
	}
	if _, v, _ := f.do(t, http.MethodGet, ""); !v.Paused {
		t.Fatal("GET after pause does not say paused")
	}
	if len(f.signInRows()) != 0 {
		t.Fatal("the pause wrote the session's row before the resume")
	}
	if code, v, body := f.do(t, http.MethodPost, "/resume"); code != http.StatusOK || v.Paused {
		t.Fatalf("resume = %d %s", code, body)
	}
	if got := f.rn.seen(); !slices.Equal(got, []string{"pause vm-1", "resume vm-1"}) {
		t.Fatalf("runner calls after resume = %v", got)
	}
	rows := f.signInRows()
	if len(rows) != 1 || rows[0].Outcome != "success" || rows[0].Actor != signinOwner || rows[0].Target != f.run.ID.String() {
		t.Fatalf("desktop.sign_in.pause rows = %+v, want one success by the owner", rows)
	}
	var d map[string]any
	if err := json.Unmarshal(rows[0].Data, &d); err != nil || d["paused_by"] != signinOwner || d["paused_at"] == nil || d["resumed_at"] == nil {
		t.Fatalf("row data = %s, %v", rows[0].Data, err)
	}
	if code, _, _ := f.do(t, http.MethodPost, "/resume"); code != http.StatusOK || len(f.signInRows()) != 1 || len(f.rn.seen()) != 2 {
		t.Fatalf("a second resume = %d, rows %d, calls %v: want no call and no second row", code, len(f.signInRows()), f.rn.seen())
	}
}

// Not allowed is refused at the server, not only hidden: on a run whose desktop keeps none, and on
// one the admin turned off after it started. Nothing reaches the runner.
func TestDesktopSignIns_OffIsRefusedServerSide(t *testing.T) {
	t.Run("store none", func(t *testing.T) {
		f := newVMSignInFixture(t, "none")
		if _, v, _ := f.do(t, http.MethodGet, ""); v.CanPause || v.Store != "none" {
			t.Fatalf("GET = %+v, want none and no pause", v)
		}
		if code, _, body := f.do(t, http.MethodPost, "/pause"); code != http.StatusForbidden || !strings.Contains(body, reasonDesktopSignInsOff) {
			t.Fatalf("pause = %d %s, want 403 %s", code, body, reasonDesktopSignInsOff)
		}
		if len(f.rn.seen()) != 0 {
			t.Fatalf("the refused pause reached the runner: %v", f.rn.seen())
		}
	})
	t.Run("admin set Not allowed", func(t *testing.T) {
		f := newVMSignInFixture(t, "memory")
		f.st.sc = types.SiteConfig{AgentProviders: &types.AgentProviders{Agents: []types.AgentProvider{{ID: "claude-code", DesktopSignInsOff: true}}}}
		if _, v, _ := f.do(t, http.MethodGet, ""); v.CanPause {
			t.Fatal("GET offers the pause on an agent set Not allowed")
		}
		if code, _, body := f.do(t, http.MethodPost, "/pause"); code != http.StatusForbidden || !strings.Contains(body, reasonDesktopSignInsOff) {
			t.Fatalf("pause = %d %s, want 403 %s", code, body, reasonDesktopSignInsOff)
		}
		if len(f.rn.seen()) != 0 {
			t.Fatalf("the refused pause reached the runner: %v", f.rn.seen())
		}
	})
}

// A pause the guest agent could not make leaves the agent running and unmarked, and is audited.
func TestDesktopSignIns_FailedPauseResumesAndLeavesNoMark(t *testing.T) {
	f := newVMSignInFixture(t, "memory")
	f.rn.pauseErr = errors.New("guestagent: 404 Not Found")
	if code, _, body := f.do(t, http.MethodPost, "/pause"); code != http.StatusBadGateway || !strings.Contains(body, reasonDesktopSignInsPauseFailed) {
		t.Fatalf("pause = %d %s", code, body)
	}
	if got := f.rn.seen(); !slices.Equal(got, []string{"pause vm-1", "resume vm-1"}) {
		t.Fatalf("runner calls = %v, want the pause then a resume", got)
	}
	if _, v, _ := f.do(t, http.MethodGet, ""); v.Paused {
		t.Fatal("a failed pause left the agent marked paused")
	}
	if rows := f.signInRows(); len(rows) != 1 || rows[0].Outcome != "failure" {
		t.Fatalf("rows = %+v, want one failure", rows)
	}
}

// Another person may not pause someone else's run's agent.
func TestDesktopSignIns_OnlyThePersonWhoMayEnter(t *testing.T) {
	f := newVMSignInFixture(t, "memory")
	f.cookie = ssoSession(t, "sub-someone-else", "else@corp.example", oidc.RoleUser)
	if code, _, _ := f.do(t, http.MethodPost, "/pause"); code != http.StatusNotFound {
		t.Fatalf("a stranger's pause = %d, want 404", code)
	}
	if len(f.rn.seen()) != 0 {
		t.Fatalf("a stranger's pause reached the runner: %v", f.rn.seen())
	}
}

// A VM's guest report becomes the run's row; a container, and a guest that reported nothing, get none,
// so the run page claims nothing for them.
func TestDesktopSignIns_RecordedOnlyFromTheGuestsReport(t *testing.T) {
	f := newVMSignInFixture(t, "memory")
	ctx := context.Background()
	vm := types.AgentRun{ID: uuid.New(), MachineKind: types.MachineLinuxVM}
	f.srv.recordDesktopVMSignIns(ctx, vm, runner.Sandbox{SignIns: "none"})
	if row, ok, _ := f.st.DesktopVMSignIns(ctx, vm.ID); !ok || row.Store != "none" {
		t.Fatalf("VM row = %+v %v, want none", row, ok)
	}
	for _, c := range []struct {
		run types.AgentRun
		sb  runner.Sandbox
	}{
		{types.AgentRun{ID: uuid.New(), MachineKind: types.MachineLinuxVM}, runner.Sandbox{}},
		{types.AgentRun{ID: uuid.New()}, runner.Sandbox{SignIns: "memory"}},
	} {
		f.srv.recordDesktopVMSignIns(ctx, c.run, c.sb)
		if _, ok, _ := f.st.DesktopVMSignIns(ctx, c.run.ID); ok {
			t.Fatalf("run %+v with sandbox %+v got a row", c.run.MachineKind, c.sb)
		}
	}
}
