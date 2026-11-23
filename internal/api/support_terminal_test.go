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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The web terminal and POST /exec under a support grant (support_terminal.go and the doors it serves): the
// mint, the entry and the re-check of an open socket or command, the owner's claim on the writer seat, and
// the lease it takes across replicas. The grant record is fakeSupportGrants, so the clock is the test's.

var allSupportDoors = []string{"terminal", "commands", "files", "forward", "web_apps", "desktop"}

// supportTermStore is the run store with the grant record on top and the governance profiles a test sets.
type supportTermStore struct {
	*supportBackend
	mu       sync.Mutex
	profiles []types.GovernanceUnit
}

func (s *supportTermStore) ListGovernanceUnits(context.Context) ([]types.GovernanceUnit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]types.GovernanceUnit(nil), s.profiles...), nil
}

func (s *supportTermStore) GetGovernanceUnitChain(_ context.Context, id uuid.UUID) ([]types.GovernanceUnit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return unitChainOf(s.profiles, nil, id)
}

// supportTermRunner is a runner that opens terminals (holderTestRunner) and runs commands (execFn).
type supportTermRunner struct {
	holderTestRunner
	execFn func(runner.ExecSpec) (*runner.ExecSession, error)
	execs  atomic.Int32
}

func (r *supportTermRunner) ExecStream(_ context.Context, _ string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	r.execs.Add(1)
	if r.execFn == nil {
		return fakeExecSession("ok", "", 0), nil
	}
	return r.execFn(spec)
}

// supportTermRig is one replica serving a person's running run, with real sessions (cookies), tickets and
// WebSockets, a millisecond authority tick and the helper's grant when doors are given.
type supportTermRig struct {
	t      *testing.T
	srv    *Server
	st     *supportTermStore
	fr     *supportTermRunner
	aud    *sshTestRecorder
	ts     *httptest.Server
	run    types.AgentRun
	grant  store.SupportGrant
	helper *http.Cookie
	owner  *http.Cookie
}

func newSupportTermRig(t *testing.T, doors ...string) *supportTermRig {
	t.Helper()
	t.Setenv(envSupportSessions, "1")
	rig := &supportTermRig{t: t, st: &supportTermStore{supportBackend: newSupportBackend()}, fr: &supportTermRunner{}, aud: &sshTestRecorder{}}
	cfg := baseTestConfig(newHarness(t), rig.st)
	cfg.Audit = rig.aud
	cfg.Runner = rig.fr
	cfg.OIDC = &oidc.Authenticator{}
	rig.srv = New(cfg)
	rig.srv.authorityTest = attachAuthorityTimings{every: authorityTick, lookup: 100 * time.Millisecond}
	t.Cleanup(rig.srv.WaitBackground)
	rig.run = types.AgentRun{ID: uuid.New(), CreatedBy: supportOwnerSub, State: types.RunRunning, SandboxRef: "sbx-1"}
	rig.st.putRun(rig.run)
	rig.helper = ssoSession(t, supportHelperSub, supportHelperSub+"@example.test", oidc.RoleAdmin)
	rig.owner = ssoSession(t, supportOwnerSub, supportOwnerSub+"@example.test", oidc.RoleUser)
	rig.ts = httptest.NewServer(panicFails(t, rig.srv.Handler()))
	t.Cleanup(rig.ts.Close)
	if len(doors) > 0 {
		ctx := context.Background()
		g, err := rig.st.RequestSupportGrant(ctx, store.SupportGrant{RunID: rig.run.ID, Helper: supportHelperSub,
			HelperEmail: supportHelperSub + "@example.test", Reason: "r", DoorsAsked: allSupportDoors})
		if err != nil {
			t.Fatal(err)
		}
		if rig.grant, err = rig.st.AcceptSupportGrant(ctx, rig.run.ID, g.ID, supportOwnerSub, supportOwnerSub+"@example.test", doors, 30, 0); err != nil {
			t.Fatal(err)
		}
	}
	return rig
}

func (rig *supportTermRig) path(tail string) string {
	return "/api/v1/runs/" + rig.run.ID.String() + tail
}

func (rig *supportTermRig) mint(who *http.Cookie, query string) *httptest.ResponseRecorder {
	rig.t.Helper()
	return doSSO(rig.t, rig.srv, http.MethodPost, rig.path("/attach-ticket"+query), who, "")
}

func (rig *supportTermRig) exec(who *http.Cookie, command string) *httptest.ResponseRecorder {
	rig.t.Helper()
	return doSSO(rig.t, rig.srv, http.MethodPost, rig.path("/exec"), who, `{"command":"`+command+`","timeout_sec":60}`)
}

// stored is the ticket a 200 mint answered with, as the store holds it.
func (rig *supportTermRig) stored(w *httptest.ResponseRecorder) store.AttachTicket {
	rig.t.Helper()
	var out struct {
		Ticket string `json:"ticket"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Ticket == "" {
		rig.t.Fatalf("mint = %d %s, want a ticket", w.Code, w.Body)
	}
	rig.st.authzStore.mu.Lock()
	defer rig.st.authzStore.mu.Unlock()
	return rig.st.authzStore.tickets[out.Ticket]
}

func (rig *supportTermRig) ticketCount() int {
	rig.st.authzStore.mu.Lock()
	defer rig.st.authzStore.mu.Unlock()
	return len(rig.st.authzStore.tickets)
}

// ticket mints a ticket straight into the store: the owner's, or the helper's under the rig's grant.
func (rig *supportTermRig) ticket(principal string, helper bool) string {
	rig.t.Helper()
	tk := store.AttachTicket{RunID: rig.run.ID, ActorType: types.ActorHuman, Principal: principal, Role: oidc.RoleUser}
	if helper {
		tk.SupportGrant, tk.SupportExpires = supportTicketGrant(rig.grant)
	}
	tok, err := storeAttachTicket(context.Background(), rig.st, tk, oidc.Issue{At: time.Now()})
	if err != nil {
		rig.t.Fatalf("mint a ticket: %v", err)
	}
	return tok
}

// dial opens the attach socket with ticket, and answers the handshake's status when the upgrade was refused.
func (rig *supportTermRig) dial(ticket string) (*websocket.Conn, int) {
	rig.t.Helper()
	url := "ws" + strings.TrimPrefix(rig.ts.URL, "http") + rig.path("/attach")
	if ticket != "" {
		url += "?ticket=" + ticket
	}
	return rig.dialURL(url, nil)
}

func (rig *supportTermRig) dialURL(url string, opts *websocket.DialOptions) (*websocket.Conn, int) {
	rig.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, url, opts)
	if err != nil {
		if resp == nil {
			rig.t.Fatalf("dial attach: %v", err)
		}
		return nil, resp.StatusCode
	}
	rig.t.Cleanup(func() { _ = c.CloseNow() })
	return c, resp.StatusCode
}

// open dials ticket, expects the upgrade, reads the attach-mode frame and starts draining the socket.
func (rig *supportTermRig) open(ticket string, wantReadOnly bool) <-chan error {
	rig.t.Helper()
	c, status := rig.dial(ticket)
	if c == nil {
		rig.t.Fatalf("attach upgrade refused with %d, want it opened", status)
	}
	if mode := readAttachMode(rig.t, c); mode.ReadOnly != wantReadOnly {
		rig.t.Fatalf("attach mode read_only = %v, want %v", mode.ReadOnly, wantReadOnly)
	}
	end := make(chan error, 1)
	go func() {
		for {
			if _, _, err := c.Read(context.Background()); err != nil {
				end <- err
				return
			}
		}
	}()
	return end
}

// wantClosed waits for the socket's read to end and returns the close code and reason it ended with.
func wantClosed(t *testing.T, end <-chan error) (websocket.StatusCode, string) {
	t.Helper()
	select {
	case err := <-end:
		var ce websocket.CloseError
		if !errors.As(err, &ce) {
			t.Fatalf("socket ended with %v, want a close frame", err)
		}
		return ce.Code, ce.Reason
	case <-time.After(5 * time.Second):
		t.Fatal("socket is still open")
		return 0, ""
	}
}

func (rig *supportTermRig) endGrant(reason string) {
	rig.t.Helper()
	if _, err := rig.st.EndSupportGrant(context.Background(), rig.run.ID, rig.grant.ID, reason, true); err != nil {
		rig.t.Fatal(err)
	}
}

// rows is the audit rows of action written for the run so far.
func (rig *supportTermRig) rows(action string) []types.AuditEvent {
	return execRows(rig.aud.snapshot(), action)
}

// profile launches the run under a governance profile that carries limits (GovernAdminRuns stays off).
func (rig *supportTermRig) profile(limits types.GovernanceLimits) {
	unit := types.GovernanceUnit{ID: uuid.New(), Name: "ci", Limits: limits}
	rig.st.mu.Lock()
	rig.st.profiles = []types.GovernanceUnit{unit}
	rig.st.mu.Unlock()
	rig.run.UnitID = &unit.ID
	rig.st.putRun(rig.run)
}

// (a) a ticket minted under a live grant has role user and carries the grant and its expiry; the owner's own
// mint is as before.
func TestSupportTerminalMintCarriesTheGrant(t *testing.T) {
	rig := newSupportTermRig(t, "terminal")
	tk := rig.stored(rig.mint(rig.helper, ""))
	if tk.Role != oidc.RoleUser || tk.Principal != supportHelperSub || tk.SupportGrant != rig.grant.ID || !tk.SupportExpires.Equal(*rig.grant.ExpiresAt) {
		t.Fatalf("helper's ticket = %+v, want role %q, principal %q, grant %s expiring %v", tk, oidc.RoleUser, supportHelperSub, rig.grant.ID, rig.grant.ExpiresAt)
	}
	own := rig.stored(rig.mint(rig.owner, ""))
	if own.SupportGrant != uuid.Nil || !own.SupportExpires.IsZero() || own.Principal != supportOwnerSub {
		t.Fatalf("owner's ticket = %+v, want no grant on it", own)
	}
}

// (b), (c) and the switch: a door the owner did not tick, any other purpose under a grant, a path-mode UI
// ticket, no grant and the switch off are all refused with the reason the value list names, and mint nothing.
func TestSupportTerminalMintRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		doors  []string
		query  string
		off    bool
		status int
		reason string
	}{
		{"terminal not ticked", []string{"files"}, "", false, 403, string(authz.ReasonSupportDoorNotGranted)},
		{"no grant", nil, "", false, 403, runOwnerOnlyReason},
		{"switch off", []string{"terminal"}, "", true, 403, runOwnerOnlyReason},
		{"ssh-exec purpose under a grant", []string{"terminal"}, "?purpose=ssh-exec", false, 403, string(authz.ReasonSupportDoorNotGranted)},
		{"ssh-exec purpose with no grant", nil, "?purpose=ssh-exec", false, 403, runOwnerOnlyReason},
		{"ui purpose, no UI door ticked", []string{"terminal"}, "?purpose=ui", false, 403, string(authz.ReasonSupportDoorNotGranted)},
		{"ui purpose in path mode", []string{"web_apps"}, "?purpose=ui", false, 403, reasonInboundNeedsHostMode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newSupportTermRig(t, tc.doors...)
			if tc.off {
				t.Setenv(envSupportSessions, "")
			}
			w := rig.mint(rig.helper, tc.query)
			if w.Code != tc.status || wireReason(t, w) != tc.reason || rig.ticketCount() != 0 {
				t.Fatalf("mint = %d %s with %d tickets stored, want %d %s and none", w.Code, w.Body, rig.ticketCount(), tc.status, tc.reason)
			}
		})
	}
	t.Run("ui purpose with per-run hosts", func(t *testing.T) {
		rig := newSupportTermRig(t, "web_apps")
		rig.srv.cfg.UIOriginTemplate = "https://{run}.apps.example.test"
		if tk := rig.stored(rig.mint(rig.helper, "?purpose=ui")); tk.SupportGrant != rig.grant.ID || tk.Role != oidc.RoleUser {
			t.Fatalf("ui ticket = %+v, want a member-stamped ticket carrying the grant", tk)
		}
	})
}

// (d) the H1 value list: with GovernAdminRuns off and a deny_interactive profile, a live grant's helper is
// refused at the terminal and at POST /exec, where a super admin on their own run is exempt. Without the
// profile both are admitted.
func TestSupportTerminalGovernanceHasNoAdminExemption(t *testing.T) {
	t.Run("deny_interactive refuses the terminal and POST /exec", func(t *testing.T) {
		rig := newSupportTermRig(t, "terminal", "commands")
		rig.profile(types.GovernanceLimits{DenyInteractive: true})
		tok := rig.stored(rig.mint(rig.helper, ""))
		if tok.Role != oidc.RoleUser {
			t.Fatalf("ticket role = %q, want a member stamp", tok.Role)
		}
		if c, status := rig.dial(rig.ticket(supportHelperSub, true)); c != nil || status != http.StatusForbidden {
			t.Fatalf("terminal under deny_interactive = %d (open %v), want 403", status, c != nil)
		}
		w := rig.exec(rig.helper, "id")
		if w.Code != http.StatusForbidden || wireReason(t, w) != string(authz.ReasonGovernanceUnit) || rig.fr.execs.Load() != 0 {
			t.Fatalf("POST /exec under deny_interactive = %d %s with %d runs, want 403 %s and none", w.Code, w.Body, rig.fr.execs.Load(), authz.ReasonGovernanceUnit)
		}
	})
	t.Run("no profile admits both", func(t *testing.T) {
		rig := newSupportTermRig(t, "terminal", "commands")
		rig.open(rig.ticket(supportHelperSub, true), false)
		if w := rig.exec(rig.helper, "id"); w.Code != http.StatusOK || rig.fr.execs.Load() != 1 {
			t.Fatalf("POST /exec = %d %s with %d runs, want 200 and one", w.Code, w.Body, rig.fr.execs.Load())
		}
	})
}

// A helper's entry rows name the grant and the entry, on the terminal and on a command.
func TestSupportTerminalRowsNameTheEntry(t *testing.T) {
	rig := newSupportTermRig(t, "terminal", "commands")
	rig.open(rig.ticket(supportHelperSub, true), false)
	if w := rig.exec(rig.helper, "id"); w.Code != http.StatusOK {
		t.Fatalf("POST /exec = %d %s", w.Code, w.Body)
	}
	for _, action := range []string{"session.attach", "run.command.exec"} {
		var rows []types.AuditEvent
		waitFor(t, action+" rows", func() bool { rows = rig.rows(action); return len(rows) > 0 })
		for _, ev := range rows {
			if !strings.Contains(string(ev.Data), `"entry":"support"`) || !strings.Contains(string(ev.Data), rig.grant.ID.String()) {
				t.Errorf("%s = %s, want the grant %s and entry support", action, ev.Data, rig.grant.ID)
			}
		}
	}
}

// (e) and (f): a revoke or an expiry (the test clock) closes an open terminal with 4401 within an authority
// tick, and so does a grant read that fails (fail closed). A grant that stays live leaves it open.
func TestSupportTerminalClosesWhenTheGrantStopsAuthorizing(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(*supportTermRig)
	}{
		{"revoked", func(r *supportTermRig) { r.endGrant("revoked") }},
		{"expired", func(r *supportTermRig) { r.st.tick(31 * time.Minute) }},
		{"a failed grant read", func(r *supportTermRig) { r.st.failOn("SupportGrantLive", errors.New("database down")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newSupportTermRig(t, "terminal")
			end := rig.open(rig.ticket(supportHelperSub, true), false)
			select {
			case err := <-end:
				t.Fatalf("a live grant's terminal ended: %v", err)
			case <-time.After(5 * authorityTick):
			}
			tc.stop(rig)
			if code, _ := wantClosed(t, end); code != attachAuthEndedCode {
				t.Fatalf("closed with %d, want %d", code, attachAuthEndedCode)
			}
		})
	}
}

// A ticket whose grant ended before the socket was opened is refused at the upgrade, as is a ticket for a
// door the grant never ticked.
func TestSupportTerminalEntryNeedsALiveGrantThatTicksTheDoor(t *testing.T) {
	rig := newSupportTermRig(t, "terminal")
	tok := rig.ticket(supportHelperSub, true)
	rig.endGrant("revoked")
	if c, status := rig.dial(tok); c != nil || status != http.StatusForbidden {
		t.Fatalf("a revoked grant's ticket = %d (open %v), want 403", status, c != nil)
	}
	files := newSupportTermRig(t, "files")
	if c, status := files.dial(files.ticket(supportHelperSub, true)); c != nil || status != http.StatusForbidden {
		t.Fatalf("a files-only grant's terminal ticket = %d (open %v), want 403", status, c != nil)
	}
}

// (g) a running POST /exec is cancelled within a tick of the revoke or of a grant that cannot be read, and its row
// says the support session ended or could not be verified, not that the caller left.
func TestSupportTerminalCommandEndsWithTheGrant(t *testing.T) {
	for _, tc := range []struct {
		name string
		cut  func(*supportTermRig)
		why  string
	}{
		{"revoked", func(r *supportTermRig) { r.endGrant("revoked") }, runExecSupportEnded},
		{"unreadable", func(r *supportTermRig) { r.st.failOn("SupportGrantLive", errors.New("database down")) }, runExecSupportUnverified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newSupportTermRig(t, "commands")
			rig.srv.sessionBudgets.test = ptr(time.Duration(0)) // a grant that cannot be read ends it at the first check
			var closed atomic.Bool
			rig.fr.execFn = func(runner.ExecSpec) (*runner.ExecSession, error) { return hangingExecSession(&closed), nil }
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- rig.exec(rig.helper, "sleep 100") }()
			waitFor(t, "the command to start", func() bool { return rig.fr.execs.Load() == 1 })
			tc.cut(rig)
			waitFor(t, "the command to be cut", closed.Load)
			select {
			case w := <-done:
				if res := execResult(t, w); res.ExitCode != -1 {
					t.Errorf("exit code = %d, want -1 for a cut command", res.ExitCode)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("POST /exec did not answer after the cut")
			}
			rows := rig.rows("run.command.exec")
			if len(rows) != 1 || rows[0].Outcome != "failure" || !strings.Contains(string(rows[0].Data), tc.why) ||
				strings.Contains(string(rows[0].Data), runExecCallerLeft) || !strings.Contains(string(rows[0].Data), `"entry":"support"`) {
				t.Fatalf("run.command.exec rows = %+v, want one failure naming %q and the support entry, not the caller leaving", rows, tc.why)
			}
		})
	}
}

// A grant read that fails rides the session budget: the command runs on, and a rode_through row names the command.
// Past the budget it ends with the support session's unverified words.
func TestRunExecSupport_UnreadableGrantRidesTheBudget(t *testing.T) {
	start := func(t *testing.T, budget time.Duration) (*supportTermRig, *atomic.Bool, chan *httptest.ResponseRecorder) {
		rig := newSupportTermRig(t, "commands")
		rig.srv.sessionBudgets.test = ptr(budget)
		rig.srv.sessionBudgets.retry = 10 * time.Millisecond
		var closed atomic.Bool
		rig.fr.execFn = func(runner.ExecSpec) (*runner.ExecSession, error) { return hangingExecSession(&closed), nil }
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- rig.exec(rig.helper, "sleep 100") }()
		waitFor(t, "the command to start", func() bool { return rig.fr.execs.Load() == 1 })
		return rig, &closed, done
	}
	t.Run("one failed read rides", func(t *testing.T) {
		rig, closed, _ := start(t, time.Hour)
		rig.st.failOn("SupportGrantLive", errors.New("database down"))
		time.Sleep(10 * authorityTick)
		rig.st.failOn("SupportGrantLive", nil)
		_, data := wantUnconfirmedRows(t, rig.aud, 1)
		if data[0]["result"] != "rode_through" || data[0]["surface"] != sessionSurfaceCommand {
			t.Fatalf("row %v, want rode_through on the command", data[0])
		}
		if closed.Load() {
			t.Fatal("the command was cut by a grant read that failed and then answered")
		}
	})
	t.Run("past the budget it ends", func(t *testing.T) {
		rig, closed, done := start(t, 50*time.Millisecond)
		rig.st.failOn("SupportGrantLive", errors.New("database down"))
		waitFor(t, "the command to be cut", closed.Load)
		select {
		case w := <-done:
			if res := execResult(t, w); res.ExitCode != -1 {
				t.Errorf("exit code = %d, want -1 for a cut command", res.ExitCode)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("POST /exec did not answer after the budget ran out")
		}
		rows := rig.rows("run.command.exec")
		if len(rows) != 1 || !strings.Contains(string(rows[0].Data), runExecSupportUnverified) {
			t.Fatalf("run.command.exec rows = %+v, want one naming %q", rows, runExecSupportUnverified)
		}
	})
}

// A helper's web attach refused at the run's session cap, through the real attach handler and a ticket the helper's
// grant minted, answers 503 and writes the refusal row naming the grant and the support entry (attach.go hands the
// ticket actor's grant to webAttachSlot); the owner's row, in the same rig, carries neither.
func TestWebAttach_HelperRefusedAtTheCapNamesItsGrant(t *testing.T) {
	rig := newSupportTermRig(t, "terminal")
	for range maxAttachSessionsPerRun {
		release, ok := rig.srv.reserveAttachSlot(rig.run, "holder")
		if !ok {
			t.Fatal("a slot was refused before the cap")
		}
		t.Cleanup(release)
	}
	for _, tc := range []struct {
		principal string
		helper    bool
	}{{supportHelperSub, true}, {supportOwnerSub, false}} {
		w := do(t, rig.srv, http.MethodGet, rig.path("/attach?ticket="+rig.ticket(tc.principal, tc.helper)), "", "")
		if w.Code != http.StatusServiceUnavailable || wireReason(t, w) != reasonAttachSessionCapReached {
			t.Fatalf("%s: attach = %d %s, want 503 %s", tc.principal, w.Code, w.Body, reasonAttachSessionCapReached)
		}
		var row types.AuditEvent
		waitFor(t, "the refusal row of "+tc.principal, func() bool {
			for _, ev := range rig.rows("session.attach") {
				if ev.Actor == tc.principal {
					row = ev
					return true
				}
			}
			return false
		})
		var d map[string]any
		if err := json.Unmarshal(row.Data, &d); err != nil {
			t.Fatal(err)
		}
		if row.Outcome != "failure" || d["reason"] != reasonAttachSessionCapReached {
			t.Fatalf("%s: row = %s %s, want a failure with reason %s", tc.principal, row.Outcome, row.Data, reasonAttachSessionCapReached)
		}
		_, hasGrant := d["support_grant"]
		_, hasEntry := d["entry"]
		switch {
		case tc.helper && (d["support_grant"] != rig.grant.ID.String() || d["entry"] != supportEntryHelper):
			t.Errorf("the helper's row = %s, want support_grant %s and entry %s", row.Data, rig.grant.ID, supportEntryHelper)
		case !tc.helper && (hasGrant || hasEntry):
			t.Errorf("the owner's row = %s, want no support_grant or entry", row.Data)
		}
	}
}

// (j) take-over by the helper is refused: handleAttachTakeover reads no grant. The cookie lane of the
// terminal stays closed to the helper too.
func TestSupportTerminalHelperCannotTakeOverOrUseTheCookieLane(t *testing.T) {
	rig := newSupportTermRig(t, "terminal", "commands")
	w := doSSO(t, rig.srv, http.MethodPost, rig.path("/attach/takeover"), rig.helper, "")
	if w.Code != http.StatusForbidden || wireReason(t, w) != runOwnerOnlyReason {
		t.Fatalf("take-over by the helper = %d %s, want 403 %s", w.Code, w.Body, runOwnerOnlyReason)
	}
	url := "ws" + strings.TrimPrefix(rig.ts.URL, "http") + rig.path("/attach")
	if c, status := rig.dialURL(url, &websocket.DialOptions{HTTPHeader: http.Header{"Cookie": {rig.helper.String()}}}); c != nil || status != http.StatusForbidden {
		t.Fatalf("the helper's cookie on the terminal = %d (open %v), want 403", status, c != nil)
	}
}

// (h) the owner attaching while the helper writes takes the seat: the helper's socket is closed with "The
// run's owner joined" and a re-attach observes; one session.takeover row has reason owner_joined.
func TestSupportTerminalOwnerWinsTheWriterSeat(t *testing.T) {
	rig := newSupportTermRig(t, "terminal")
	helperEnd := rig.open(rig.ticket(supportHelperSub, true), false)
	waitFor(t, "the helper to hold the seat", func() bool { w := rig.srv.attachHolderFor(rig.run.ID); return w != nil && w.support == rig.grant.ID })

	rig.open(rig.ticket(supportOwnerSub, false), false)
	code, reason := wantClosed(t, helperEnd)
	if code != websocket.StatusPolicyViolation || reason != supportOwnerJoinedReason {
		t.Fatalf("the helper's socket closed with %d %q, want %d %q", code, reason, websocket.StatusPolicyViolation, supportOwnerJoinedReason)
	}
	if w := rig.srv.attachHolderFor(rig.run.ID); w == nil || w.principal != supportOwnerSub || w.support != uuid.Nil {
		t.Fatalf("writer = %+v, want the owner's", w)
	}
	rows := rig.rows("session.takeover")
	if len(rows) != 1 || rows[0].Actor != supportOwnerSub ||
		!strings.Contains(string(rows[0].Data), `"reason":"owner_joined"`) || !strings.Contains(string(rows[0].Data), `"previous_holder":"`+supportHelperSub+`"`) {
		t.Fatalf("session.takeover rows = %+v, want one by the owner with reason owner_joined naming the helper", rows)
	}
	rig.open(rig.ticket(supportHelperSub, true), true) // the helper may attach again, and observes
	if w := rig.srv.attachHolderFor(rig.run.ID); w == nil || w.principal != supportOwnerSub {
		t.Fatalf("writer after the helper re-attached = %+v, want the owner still", w)
	}
}

func supportClient(principal string, grant uuid.UUID) *leaseCovClient {
	c := newLeaseCovClient(principal, attachSourceWeb)
	c.h.support = grant
	return c
}

// (h) on one replica with leases: the owner joining behind a helper takes the seat and the lease (support
// false); a second helper tab stays an observer; a second owner tab observes; the lease is the owner's.
func TestSupportSeatOwnerJoiningTakesSeatAndLease(t *testing.T) {
	leases := newLeaseCovLeases()
	srv, aud, _ := leaseCovServer(t, leases, leaseCovServerOpts{})
	grant := uuid.New()
	helper, helperTab2, owner := supportClient("helper", grant), supportClient("helper", grant), newLeaseCovClient("owner", attachSourceWeb)

	if readOnly, _ := srv.registerAttachHolder(leaseCovRun, helper.h); readOnly {
		t.Fatal("a helper alone was read-only")
	}
	srv.registerAttachHolder(leaseCovRun, helperTab2.h)
	if row, _ := leases.row(leaseCovRun); !row.Support || row.HolderID != helper.h.id {
		t.Fatalf("lease = %+v, want the helper's, marked support", row)
	}

	readOnly, _ := srv.registerAttachHolder(leaseCovRun, owner.h)

	if readOnly || !owner.h.writable.Load() {
		t.Fatalf("owner joined read-only (%v, writable %v), want the seat", readOnly, owner.h.writable.Load())
	}
	if w, obs := leaseCovState(srv, leaseCovRun); w != owner.h || len(obs) != 1 || obs[0] != helperTab2.h || helperTab2.h.writable.Load() {
		t.Fatalf("writer=%v observers=%v, want the owner writing and the helper's other tab observing", w, obs)
	}
	if reason := helper.displacedWith(t); reason != supportOwnerJoinedReason || !helper.h.evicted.Load() {
		t.Fatalf("helper displaced with %q (evicted %v), want %q", reason, helper.h.evicted.Load(), supportOwnerJoinedReason)
	}
	if ev := aud.next(t); ev.Action != "session.takeover" || ev.Actor != "owner" ||
		!strings.Contains(string(ev.Data), `"reason":"owner_joined"`) || !strings.Contains(string(ev.Data), `"previous_holder":"helper"`) {
		t.Fatalf("first row = %s by %s %s, want the session.takeover with reason owner_joined before anything else", ev.Action, ev.Actor, ev.Data)
	}
	if ev := aud.next(t); ev.Action != "session.promote" || ev.Actor != "owner" {
		t.Fatalf("second row = %s by %s, want the owner's session.promote", ev.Action, ev.Actor)
	}
	owner.promotedNotice(t)
	if row, _ := leases.row(leaseCovRun); row.HolderID != owner.h.id || row.Support {
		t.Fatalf("lease = %+v, want the owner's, support false", row)
	}
	if got := len(aud.snapshot()); got != 2 {
		t.Fatalf("%d audit rows, want the takeover and the promotion", got)
	}

	owner2 := newLeaseCovClient("owner", attachSourceWeb)
	if readOnly, _ := srv.registerAttachHolder(leaseCovRun, owner2.h); !readOnly {
		t.Fatal("a second owner tab took the seat from the first")
	}
}

// (h) across two replicas: the owner on B takes the lease the helper holds on A (epoch +1, support false);
// A's keeper finds the lease gone and displaces the helper with "The run's owner joined"; a helper that
// attaches again on A observes, and so does another owner tab, which the lease no longer admits.
func TestSupportSeatAcrossReplicas(t *testing.T) {
	leases := newLeaseCovLeases()
	srvA, _, _ := leaseCovServer(t, leases, leaseCovServerOpts{})
	srvB, _, _ := leaseCovServer(t, leases, leaseCovServerOpts{})
	grant := uuid.New()
	helper, owner := supportClient("helper", grant), newLeaseCovClient("owner", attachSourceWeb)

	srvA.registerAttachHolder(leaseCovRun, helper.h)
	before, _ := leases.row(leaseCovRun)
	if !before.Support {
		t.Fatalf("lease = %+v, want the helper's, marked support", before)
	}

	if readOnly, _ := srvB.registerAttachHolder(leaseCovRun, owner.h); readOnly {
		t.Fatal("the owner on replica B was read-only behind a helper's lease")
	}
	if row, _ := leases.row(leaseCovRun); row.HolderID != owner.h.id || row.Support || row.Epoch != before.Epoch+1 {
		t.Fatalf("lease = %+v, want the owner's, support false, epoch %d", row, before.Epoch+1)
	}

	srvA.attachLeaseTick(t.Context())
	if reason := helper.displacedWith(t); reason != supportOwnerJoinedReason {
		t.Fatalf("helper displaced with %q, want %q", reason, supportOwnerJoinedReason)
	}

	again := supportClient("helper", grant)
	if readOnly, _ := srvA.registerAttachHolder(leaseCovRun, again.h); !readOnly {
		t.Fatal("a helper that attached again became the writer behind the owner's lease")
	}
	tab := newLeaseCovClient("owner", attachSourceWeb)
	if readOnly, _ := srvA.registerAttachHolder(leaseCovRun, tab.h); !readOnly {
		t.Fatal("a second owner tab took a lease the owner already holds")
	}
}

// A helper takes a free seat only when no owner client waits for it, and the free seat goes to the oldest
// owner client before any helper, in the registry and in firstWaiting.
func TestSupportSeatQueueOrder(t *testing.T) {
	grant := uuid.New()
	t.Run("a helper behind a waiting owner client observes", func(t *testing.T) {
		srv, _, _ := leaseCovServer(t, newLeaseCovLeases(), leaseCovServerOpts{})
		owner, helper := newLeaseCovClient("owner", attachSourceWeb), supportClient("helper", grant)
		leaseCovSeed(srv, leaseCovRun, nil, owner.h)
		if readOnly, _ := srv.registerAttachHolder(leaseCovRun, helper.h); !readOnly {
			t.Fatal("a helper took the free seat with an owner client waiting")
		}
	})
	t.Run("a release promotes the oldest owner client, not an older helper", func(t *testing.T) {
		srv, aud, _ := leaseCovServer(t, newLeaseCovLeases(), leaseCovServerOpts{})
		first, helper, second := newLeaseCovClient("alice", attachSourceWeb), supportClient("helper", grant), newLeaseCovClient("alice", attachSourceWeb)
		_, release := srv.registerAttachHolder(leaseCovRun, first.h)
		srv.registerAttachHolder(leaseCovRun, helper.h)
		srv.registerAttachHolder(leaseCovRun, second.h)
		if announce := release(); announce != nil {
			announce()
		}
		if w, obs := leaseCovState(srv, leaseCovRun); w != second.h || len(obs) != 1 || obs[0] != helper.h {
			t.Fatalf("writer=%v observers=%v, want the second owner tab writing and the helper still queued", w, obs)
		}
		leaseCovExpectPromoteRow(t, aud, "alice", "alice")
	})
	t.Run("firstWaiting takes a helper only when no owner client waits", func(t *testing.T) {
		waiting := func(h *attachHolder) *attachHolder {
			h.lease = &attachLease{}
			h.waitRemote.Store(true)
			return h
		}
		helper, owner := waiting(supportClient("helper", grant).h), waiting(newLeaseCovClient("owner", attachSourceWeb).h)
		if got := firstWaiting(&runAttach{observers: []*attachHolder{helper, owner}}); got != owner {
			t.Errorf("firstWaiting with a helper ahead of an owner = %v, want the owner", got)
		}
		if got := firstWaiting(&runAttach{observers: []*attachHolder{helper}}); got != helper {
			t.Errorf("firstWaiting with only a helper = %v, want the helper", got)
		}
	})
}
