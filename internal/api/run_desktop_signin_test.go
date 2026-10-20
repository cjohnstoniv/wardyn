// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretspresent"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const signinOwner = "sub-signin-owner"

// signinTestStore is the UI gateway's memory store plus desktop sign-ins.
type signinTestStore struct {
	*uiMemStore
	smu     sync.Mutex
	signins map[uuid.UUID]types.DesktopSignin
	shares  []types.DesktopSigninShare
	grants  []types.CredentialGrant
	readErr error
}

var _ store.DesktopSigninStore = (*signinTestStore)(nil)

func (s *signinTestStore) ListGrantsByRun(context.Context, uuid.UUID) ([]types.CredentialGrant, error) {
	return s.grants, s.readErr
}

func (s *signinTestStore) BeginDesktopSignin(_ context.Context, si types.DesktopSignin) error {
	s.smu.Lock()
	defer s.smu.Unlock()
	for _, o := range s.signins {
		if o.RunID == si.RunID && o.EndedAt == nil {
			return store.ErrConflict
		}
	}
	s.signins[si.ID] = si
	return nil
}

func (s *signinTestStore) SetDesktopSigninPauseRecord(_ context.Context, id uuid.UUID, rec json.RawMessage) error {
	s.smu.Lock()
	defer s.smu.Unlock()
	si := s.signins[id]
	si.PauseRecord = rec
	s.signins[id] = si
	return nil
}

func (s *signinTestStore) open(runID uuid.UUID) (types.DesktopSignin, bool) {
	for _, si := range s.signins {
		if si.RunID == runID && si.EndedAt == nil {
			return si, true
		}
	}
	return types.DesktopSignin{}, false
}

func (s *signinTestStore) OpenDesktopSignin(_ context.Context, runID uuid.UUID) (types.DesktopSignin, error) {
	s.smu.Lock()
	defer s.smu.Unlock()
	if si, ok := s.open(runID); ok {
		return si, nil
	}
	return types.DesktopSignin{}, store.ErrNotFound
}

func (s *signinTestStore) ListOpenDesktopSignins(context.Context) ([]types.DesktopSignin, error) {
	s.smu.Lock()
	defer s.smu.Unlock()
	var out []types.DesktopSignin
	for _, si := range s.signins {
		if si.EndedAt == nil {
			out = append(out, si)
		}
	}
	return out, nil
}

func (s *signinTestStore) DesktopSigninCredentialOpen(_ context.Context, runID uuid.UUID, hash []byte, now time.Time) (bool, error) {
	s.smu.Lock()
	defer s.smu.Unlock()
	si, ok := s.open(runID)
	return ok && si.EndRequested == "" && now.Before(si.ExpiresAt) && bytes.Equal(si.CredentialHash, hash), nil
}

func (s *signinTestStore) RequestDesktopSigninEnd(_ context.Context, id uuid.UUID, reason string) error {
	s.smu.Lock()
	defer s.smu.Unlock()
	si := s.signins[id]
	if si.EndRequested == "" {
		si.EndRequested = reason
	}
	s.signins[id] = si
	return nil
}

func (s *signinTestStore) EndDesktopSignin(_ context.Context, id uuid.UUID, reason string, at time.Time) (bool, error) {
	s.smu.Lock()
	defer s.smu.Unlock()
	si, ok := s.signins[id]
	if !ok || si.EndedAt != nil {
		return false, nil
	}
	si.EndedAt, si.EndReason = &at, reason
	s.signins[id] = si
	return true, nil
}

func (s *signinTestStore) RecordDesktopSigninShare(_ context.Context, sh types.DesktopSigninShare) error {
	s.smu.Lock()
	defer s.smu.Unlock()
	s.shares = append(s.shares, sh)
	return nil
}

func (s *signinTestStore) CountDesktopSigninShares(_ context.Context, id uuid.UUID) (int, error) {
	s.smu.Lock()
	defer s.smu.Unlock()
	n := 0
	for _, sh := range s.shares {
		if sh.SigninID == id {
			n++
		}
	}
	return n, nil
}

func (s *signinTestStore) RunHadDesktopSignin(_ context.Context, runID uuid.UUID) (bool, error) {
	s.smu.Lock()
	defer s.smu.Unlock()
	if s.readErr != nil {
		return false, s.readErr
	}
	for _, si := range s.signins {
		if si.RunID == runID {
			return true, nil
		}
	}
	return false, nil
}

func (s *signinTestStore) RunsWithOpenDesktopSignin(context.Context) (map[uuid.UUID]bool, error) {
	s.smu.Lock()
	defer s.smu.Unlock()
	out := map[uuid.UUID]bool{}
	for _, si := range s.signins {
		if si.EndedAt == nil {
			out[si.RunID] = true
		}
	}
	return out, nil
}

func (s *signinTestStore) only(t *testing.T) types.DesktopSignin {
	t.Helper()
	s.smu.Lock()
	defer s.smu.Unlock()
	if len(s.signins) != 1 {
		t.Fatalf("%d sign-in rows, want 1", len(s.signins))
	}
	for _, si := range s.signins {
		return si
	}
	return types.DesktopSignin{}
}

// signinFakeRunner records every helper exec and signal in order, as
// "<agent|person>:<subcommand>" and "signal:<name>", with each exec's stdin.
type signinFakeRunner struct {
	*sshFakeRunner
	mu     sync.Mutex
	calls  []string
	stdin  map[string][]byte
	exit   map[string]int // by subcommand; 0 when absent
	stdout map[string]string
}

var _ runner.SigninExecer = (*signinFakeRunner)(nil)

func newSigninFakeRunner() *signinFakeRunner {
	return &signinFakeRunner{sshFakeRunner: &sshFakeRunner{}, stdin: map[string][]byte{}, exit: map[string]int{},
		stdout: map[string]string{"pause-agent": `{"already_stopped":[7]}`}}
}

func (f *signinFakeRunner) exec(who string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	if len(spec.Argv) < 2 || spec.Argv[0] != signinBinary {
		return nil, runner.ErrExecStreamUnsupported
	}
	sub := spec.Argv[1]
	f.mu.Lock()
	f.calls = append(f.calls, who+":"+sub)
	code, out := f.exit[sub], f.stdout[sub]
	f.mu.Unlock()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	done := make(chan struct{})
	go func() {
		b, _ := io.ReadAll(inR)
		f.mu.Lock()
		f.stdin[who+":"+sub] = b
		f.mu.Unlock()
		_ = errW.Close()
		_, _ = io.WriteString(outW, out)
		_ = outW.Close()
		close(done)
	}()
	return &runner.ExecSession{Stdin: inW, Stdout: outR, Stderr: errR,
		Wait: func() (int, error) { <-done; return code, nil }}, nil
}

func (f *signinFakeRunner) ExecStream(_ context.Context, _ string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	return f.exec("agent", spec)
}

func (f *signinFakeRunner) ExecStreamAs(_ context.Context, _, user string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	if user != runner.SigninPersonUser {
		return nil, errors.New("wrong user " + user)
	}
	return f.exec("person", spec)
}

func (f *signinFakeRunner) SignalMain(_ context.Context, _, signal string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "signal:"+signal)
	return nil
}

func (f *signinFakeRunner) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *signinFakeRunner) input(key string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stdin[key]
}

type signinFixture struct {
	srv    *Server
	h      *harness
	st     *signinTestStore
	rn     *signinFakeRunner
	run    types.AgentRun
	cookie *http.Cookie
}

func newSigninFixture(t *testing.T) *signinFixture {
	t.Helper()
	h := newHarness(t)
	st := &signinTestStore{uiMemStore: newUIMemStore(), signins: map[uuid.UUID]types.DesktopSignin{}}
	run := types.AgentRun{ID: uuid.New(), CreatedBy: signinOwner, State: types.RunRunning, SandboxRef: "sandbox-1"}
	st.putRun(run)
	st.putEffectivePolicy(run.ID, types.RunPolicySpec{UIApps: []types.UIApp{{Name: desktopUIApp, Port: 6080}}})
	rn := newSigninFakeRunner()
	cfg := baseTestConfig(h, st)
	cfg.Runner, cfg.OIDC, cfg.DesktopSignins = rn, &oidc.Authenticator{}, true
	return &signinFixture{srv: New(cfg), h: h, st: st, rn: rn, run: run,
		cookie: ssoSession(t, signinOwner, "owner@corp.example", oidc.RoleUser)}
}

func (f *signinFixture) post(t *testing.T, path, body string) (int, string) {
	t.Helper()
	w := doSSO(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+path, f.cookie, body)
	return w.Code, w.Body.String()
}

func (f *signinFixture) begin(t *testing.T) {
	t.Helper()
	if code, body := f.post(t, "/desktop-signin", ""); code != http.StatusCreated {
		t.Fatalf("begin = %d %s, want 201", code, body)
	}
}

// TestDesktopSignin_BeginStopsTheAgentBeforeTheDisplay: the person's display
// starts only after pid 1 is stopped from outside and pause-agent proved the
// rest stopped, and the lane credential reaches the person's exec and the
// store's hash, never the audit trail.
func TestDesktopSignin_BeginStopsTheAgentBeforeTheDisplay(t *testing.T) {
	f := newSigninFixture(t)
	code, body := f.post(t, "/desktop-signin", "")
	if code != http.StatusCreated || !strings.Contains(body, `"app":"_signin"`) {
		t.Fatalf("begin = %d %s, want 201 naming the _signin app", code, body)
	}
	if got, want := f.rn.seen(), []string{"signal:SIGSTOP", "agent:pause-agent", "person:start"}; !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	var cfg struct{ Credential, Proxy string }
	if err := json.Unmarshal(f.rn.input("person:start"), &cfg); err != nil || len(cfg.Credential) != 43 || cfg.Proxy != "wardyn-proxy:3129" {
		t.Fatalf("start stdin = %+v %v", cfg, err)
	}
	si := f.st.only(t)
	sum := sha256.Sum256([]byte(cfg.Credential))
	if !bytes.Equal(si.CredentialHash, sum[:]) || si.Holder != signinOwner || string(si.PauseRecord) != `{"already_stopped":[7]}` {
		t.Fatalf("row = %+v", si)
	}
	if !si.ExpiresAt.Equal(si.BeganAt.Add(30 * time.Minute)) {
		t.Fatalf("window = %v, want 30 minutes", si.ExpiresAt.Sub(si.BeganAt))
	}
	for _, ev := range f.h.audit.snapshot() {
		if strings.Contains(string(ev.Data), cfg.Credential) {
			t.Fatalf("audit row %s carries the lane credential", ev.Action)
		}
	}
	if code, body := f.post(t, "/desktop-signin", ""); code != http.StatusConflict || !strings.Contains(body, reasonDesktopSigninOpen) {
		t.Fatalf("second begin = %d %s, want 409 %s", code, body, reasonDesktopSigninOpen)
	}
}

// TestDesktopSignin_UnprovenPauseResumesAndNeverStarts: when pause-agent
// cannot prove the agent stopped, pid 1 is continued, the row is ended, and
// the person's display never starts.
func TestDesktopSignin_UnprovenPauseResumesAndNeverStarts(t *testing.T) {
	f := newSigninFixture(t)
	f.rn.exit["pause-agent"] = 1
	if code, body := f.post(t, "/desktop-signin", ""); code != http.StatusBadGateway || !strings.Contains(body, reasonDesktopSigninPauseFailed) {
		t.Fatalf("begin = %d %s, want 502 %s", code, body, reasonDesktopSigninPauseFailed)
	}
	if got, want := f.rn.seen(), []string{"signal:SIGSTOP", "agent:pause-agent", "signal:SIGCONT"}; !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if si := f.st.only(t); si.EndedAt == nil || si.EndReason != signinEndFailed {
		t.Fatalf("row = %+v, want ended failed", si)
	}
}

// TestDesktopSignin_ShareCarriesOneSiteOnly: the server re-filters the
// export to the site whatever the person's side returned, and the share's
// audit row names the site and a count, never a cookie.
func TestDesktopSignin_ShareCarriesOneSiteOnly(t *testing.T) {
	f := newSigninFixture(t)
	f.begin(t)
	f.rn.stdout["export"] = `[{"name":"sid","value":"SITE-SECRET","domain":".site.test"},` +
		`{"name":"a","value":"SUB-SECRET","domain":"accounts.site.test"},` +
		`{"name":"oid","value":"OTHER-SECRET","domain":"other.test"},` +
		`{"name":"x","value":"LOOKALIKE-SECRET","domain":"site.test.evil"}]`
	code, body := f.post(t, "/desktop-signin/shares", `{"site":"Accounts.Site.Test"}`)
	if code != http.StatusOK || body != `{"site":"site.test","cookies":2}`+"\n" {
		t.Fatalf("share = %d %q", code, body)
	}
	imported := string(f.rn.input("agent:import"))
	if !strings.Contains(imported, "SITE-SECRET") || !strings.Contains(imported, "SUB-SECRET") ||
		strings.Contains(imported, "OTHER-SECRET") || strings.Contains(imported, "LOOKALIKE-SECRET") {
		t.Fatalf("import stdin = %s", imported)
	}
	for _, ev := range f.h.audit.snapshot() {
		if strings.Contains(string(ev.Data), "SECRET") {
			t.Fatalf("audit row %s carries a cookie value", ev.Action)
		}
	}
	for _, bad := range []string{`{"site":"10.0.0.1"}`, `{"site":"com"}`, `{"site":"a/b.test"}`} {
		if code, body := f.post(t, "/desktop-signin/shares", bad); code != http.StatusBadRequest || !strings.Contains(body, reasonDesktopSigninSiteInvalid) {
			t.Errorf("share %s = %d %s, want 400", bad, code, body)
		}
	}
}

// TestDesktopSignin_EndClosesTheDisplayBeforeTheAgentResumes, and an end that
// cannot prove the display closed leaves the agent stopped for the sweep.
func TestDesktopSignin_EndClosesTheDisplayBeforeTheAgentResumes(t *testing.T) {
	f := newSigninFixture(t)
	f.begin(t)
	f.rn.exit["stop"] = 1
	if code, body := f.post(t, "/desktop-signin/end", ""); code != http.StatusBadGateway || !strings.Contains(body, reasonDesktopSigninEndFailed) {
		t.Fatalf("failed end = %d %s, want 502", code, body)
	}
	if got := f.rn.seen(); slices.Contains(got, "signal:SIGCONT") || slices.Contains(got, "agent:resume-agent") {
		t.Fatalf("calls after a failed stop = %v: the agent was resumed", got)
	}
	if si := f.st.only(t); si.EndedAt != nil || si.EndRequested != signinEndDone {
		t.Fatalf("row = %+v, want open with an end requested", si)
	}

	f.rn.exit["stop"] = 0
	f.srv.cfg.Now = func() time.Time { return time.Now().Add(time.Second) }
	if err := f.srv.sweepDesktopSignins(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := f.rn.seen()
	if want := []string{"person:stop", "person:stop", "agent:resume-agent", "signal:SIGCONT"}; !slices.Equal(got[3:], want) {
		t.Fatalf("calls = %v, want the sweep's %v after the begin", got, want)
	}
	if string(f.rn.input("agent:resume-agent")) != `{"already_stopped":[7]}` {
		t.Fatalf("resume-agent stdin = %s, want the pause record", f.rn.input("agent:resume-agent"))
	}
	if si := f.st.only(t); si.EndedAt == nil || si.EndReason != signinEndDone {
		t.Fatalf("row = %+v, want ended done", si)
	}
}

// TestDesktopSignin_ExpiredWindowIsEndedBySweep.
func TestDesktopSignin_ExpiredWindowIsEndedBySweep(t *testing.T) {
	f := newSigninFixture(t)
	f.begin(t)
	f.srv.cfg.Now = func() time.Time { return time.Now().Add(31 * time.Minute) }
	if err := f.srv.sweepDesktopSignins(context.Background()); err != nil {
		t.Fatal(err)
	}
	if si := f.st.only(t); si.EndedAt == nil || si.EndReason != signinEndExpired {
		t.Fatalf("row = %+v, want ended expired", si)
	}
}

// TestDesktopSignin_OnlyTheRunsCreator: the admin token and another person
// are refused before anything in the sandbox runs.
func TestDesktopSignin_OnlyTheRunsCreator(t *testing.T) {
	f := newSigninFixture(t)
	if w := do(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/desktop-signin", adminToken, ""); w.Code != http.StatusForbidden {
		t.Fatalf("admin token = %d %s, want 403", w.Code, w.Body.String())
	}
	other := ssoSession(t, "sub-someone-else", "else@corp.example", oidc.RoleUser)
	if w := doSSO(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/desktop-signin", other, ""); w.Code != http.StatusNotFound {
		t.Fatalf("another person = %d %s, want 404", w.Code, w.Body.String())
	}
	if got := f.rn.seen(); len(got) != 0 {
		t.Fatalf("calls = %v, want none", got)
	}
}

// TestDesktopSignin_OffByDefault: with the knob off no route acts and no run
// gets the store or the lane.
func TestDesktopSignin_OffByDefault(t *testing.T) {
	f := newSigninFixture(t)
	f.srv.cfg.DesktopSignins = false
	if code, body := f.post(t, "/desktop-signin", ""); code != http.StatusNotImplemented || !strings.Contains(body, reasonDesktopSigninsOff) {
		t.Fatalf("begin = %d %s, want 501", code, body)
	}
	desktop := types.RunPolicySpec{UIApps: []types.UIApp{{Name: desktopUIApp, Port: 6080}}}
	if f.srv.desktopSigninsFor(desktop) {
		t.Fatal("knob off: a desktop run got sign-ins")
	}
	f.srv.cfg.DesktopSignins = true
	if !f.srv.desktopSigninsFor(desktop) || f.srv.desktopSigninsFor(types.RunPolicySpec{UIApps: []types.UIApp{{Name: "code", Port: 1}}}) {
		t.Fatal("knob on: sign-ins must follow the novnc app")
	}
}

// TestDesktopSignin_CheckAdmitsOnlyTheOpenCredential: the proxy's check is
// 204 for the run's open sign-in's credential only, and 403 after its end.
func TestDesktopSignin_CheckAdmitsOnlyTheOpenCredential(t *testing.T) {
	f := newSigninFixture(t)
	f.begin(t)
	var cfg struct{ Credential string }
	_ = json.Unmarshal(f.rn.input("person:start"), &cfg)
	tok := f.h.mintRunToken(t, f.run.ID)
	check := func(cred string) int {
		return do(t, f.srv, http.MethodPost, "/api/v1/internal/desktop-signin/check", tok, `{"credential":"`+cred+`"}`).Code
	}
	if c := check(cfg.Credential); c != http.StatusNoContent {
		t.Fatalf("open credential = %d, want 204", c)
	}
	if c := check(strings.Repeat("A", 43)); c != http.StatusForbidden {
		t.Fatalf("wrong credential = %d, want 403", c)
	}
	other := f.h.mintRunToken(t, uuid.New())
	if c := do(t, f.srv, http.MethodPost, "/api/v1/internal/desktop-signin/check", other, `{"credential":"`+cfg.Credential+`"}`).Code; c == http.StatusNoContent {
		t.Fatal("another run's token was admitted with this run's credential")
	}
	if code, body := f.post(t, "/desktop-signin/end", ""); code != http.StatusNoContent {
		t.Fatalf("end = %d %s", code, body)
	}
	if c := check(cfg.Credential); c != http.StatusForbidden {
		t.Fatalf("credential after the end = %d, want 403", c)
	}
}

// TestDesktopSignin_DisplayOfferedToTheHolderOnly: the UI gateway's _signin
// app exists only for the person who holds the open sign-in.
func TestDesktopSignin_DisplayOfferedToTheHolderOnly(t *testing.T) {
	f := newSigninFixture(t)
	hasDisplay := func(principal string) bool {
		apps := f.srv.withDesktopSigninApp(context.Background(), f.run, principal, nil)
		return slices.ContainsFunc(apps, func(a types.UIApp) bool { return a.Name == desktopSigninApp })
	}
	if hasDisplay(signinOwner) {
		t.Fatal("display offered with no sign-in open")
	}
	f.begin(t)
	if !hasDisplay(signinOwner) || hasDisplay("sub-someone-else") || hasDisplay(adminTokenPrincipal) {
		t.Fatal("display must be the holder's alone")
	}
}

func TestSigninSite(t *testing.T) {
	for in, want := range map[string]string{"site.test": "site.test", "Accounts.Example.co.uk.": "example.co.uk", "login.example.com": "example.com"} {
		if got, ok := signinSite(in); !ok || got != want {
			t.Errorf("signinSite(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "::1", "127.0.0.1", "co.uk", "a b.test", "user@site.test", "site.test:443"} {
		if got, ok := signinSite(bad); ok {
			t.Errorf("signinSite(%q) = %q, want refused", bad, got)
		}
	}
}

// TestSecretsPresent_Gatherer: each source counts, a begun sign-in counts
// for good, and an unreadable source fails closed.
func TestSecretsPresent_Gatherer(t *testing.T) {
	f := newSigninFixture(t)
	ctx := context.Background()
	if present, why := f.srv.secretsPresent(ctx, f.run.ID); present {
		t.Fatalf("a plain run = %v", why)
	}
	f.begin(t)
	if _, why := f.srv.secretsPresent(ctx, f.run.ID); !slices.Equal(why, []secretspresent.Reason{secretspresent.SigninStore}) {
		t.Fatalf("after a sign-in = %v", why)
	}
	f.st.grants = []types.CredentialGrant{{Spec: types.GrantSpec{Kind: types.GrantSSHKey}}}
	if _, why := f.srv.secretsPresent(ctx, f.run.ID); !slices.Contains(why, secretspresent.ResidentGrant) {
		t.Fatalf("an ssh_key grant = %v", why)
	}
	f.st.readErr = errors.New("down")
	if _, why := f.srv.secretsPresent(ctx, f.run.ID); !slices.Contains(why, secretspresent.Unknown) {
		t.Fatalf("unreadable = %v, want unknown", why)
	}
	for _, tc := range []struct {
		action, data string
		want         bool
	}{
		{"run.bedrock.configure", `{"mode":"bearer"}`, false},
		{"run.bedrock.configure", `{"mode":"sso-inject-proxy"}`, true},
		{"run.component.attach", `{"kind":"custom","source":"inline","secrets":[{"delivery":"header"}]}`, false},
		{"run.component.attach", `{"kind":"custom","source":"self","secrets":[{"delivery":"file"}]}`, true},
		{"run.component.attach", `{"kind":"custom","source":"org","secrets":[]}`, true},
		{"run.component.attach", `{"kind":"git_provider","source":"org"}`, false},
	} {
		if got := residentByAudit(types.AuditEvent{Action: tc.action, Outcome: "success", Data: json.RawMessage(tc.data)}); got != tc.want {
			t.Errorf("%s %s = %v, want %v", tc.action, tc.data, got, tc.want)
		}
	}
}
