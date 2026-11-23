// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// GET /runs/{id}/attach/forward (attach_forward.go): one TCP connection to a port on the sandbox's own loopback,
// no wider than an SSH -L channel, by attach ticket only, off unless WARDYN_ATTACH_FORWARD is set.

const forwardOwner = "forward-owner-sub"

// forwardTick is the ping interval and the re-check interval of the tests that watch either.
const forwardTick = 50 * time.Millisecond

// forwardRelay is the in-sandbox relay as a test drives it: a copy of its stdin to its stdout (echo), a stdout that
// stays silent until it is closed (the default), a stdout that never stops (flood), and a stdin write that waits
// for a gate (stdinGate).
type forwardRelay struct {
	echo, flood bool
	stdinGate   chan struct{}
	outR        *io.PipeReader
	outW        *io.PipeWriter
	closed      chan struct{}
	once        sync.Once
	closes      atomic.Int32
	stdinBytes  atomic.Int64
	stdoutReads atomic.Int64
}

func newForwardRelay(echo bool) *forwardRelay {
	r, w := io.Pipe()
	return &forwardRelay{echo: echo, outR: r, outW: w, closed: make(chan struct{})}
}

type forwardStdin struct{ f *forwardRelay }

func (s forwardStdin) Write(p []byte) (int, error) {
	f := s.f
	if f.stdinGate != nil {
		select {
		case <-f.stdinGate:
		case <-f.closed:
			return 0, io.ErrClosedPipe
		}
	}
	f.stdinBytes.Add(int64(len(p)))
	if f.echo {
		return f.outW.Write(p)
	}
	return len(p), nil
}

func (s forwardStdin) Close() error {
	if s.f.echo {
		_ = s.f.outW.Close()
	}
	return nil
}

type forwardFlood struct{ f *forwardRelay }

func (r forwardFlood) Read(p []byte) (int, error) {
	select {
	case <-r.f.closed:
		return 0, io.EOF
	default:
	}
	r.f.stdoutReads.Add(1)
	n := min(len(p), 32<<10)
	clear(p[:n])
	return n, nil
}

func (f *forwardRelay) Close() error {
	f.closes.Add(1)
	f.once.Do(func() {
		close(f.closed)
		_ = f.outR.Close()
		_ = f.outW.Close()
	})
	return nil
}

func (f *forwardRelay) session() *runner.ExecSession {
	var out io.Reader = f.outR
	if f.flood {
		out = forwardFlood{f}
	}
	return &runner.ExecSession{
		Stdin:  forwardStdin{f},
		Stdout: out,
		Wait: func() (int, error) {
			if !f.echo {
				<-f.closed
			}
			return 0, nil
		},
		Close: f.Close,
	}
}

// forwardStore is the rig's store: the run's data_out rule and whether it can be read, and GetRun's failure, are
// switches a test flips while a forward is open.
type forwardStore struct {
	store.Store
	rule        atomic.Value // types.TransferRule
	ruleErr     atomic.Bool
	getRunErr   atomic.Bool
	getRunCalls atomic.Int32
}

func (s *forwardStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	s.getRunCalls.Add(1)
	if s.getRunErr.Load() {
		return types.AgentRun{}, errors.New("run store unavailable")
	}
	return s.Store.GetRun(ctx, id)
}

func (s *forwardStore) QueryAuditEvents(_ context.Context, runID uuid.UUID, _ int) ([]types.AuditEvent, error) {
	if s.ruleErr.Load() {
		return nil, errors.New("audit store unavailable")
	}
	rule, _ := s.rule.Load().(types.TransferRule)
	data, _ := json.Marshal(types.RunPolicySpec{DataOut: rule})
	return []types.AuditEvent{{ID: uuid.New(), RunID: &runID, Action: "run.policy.resolve", Outcome: "success", Data: data}}, nil
}

// forwardRig is an authority rig (real OIDC, sequenced revocations, a millisecond re-check) with a person's running
// run on a runner whose ExecStream hands out the rig's relays.
type forwardRig struct {
	*authorityRig
	st      *forwardStore
	runID   uuid.UUID
	mk      func() *forwardRelay
	execErr error
	execs   atomic.Int32
	mu      sync.Mutex
	relays  []*forwardRelay
	specs   []runner.ExecSpec
}

// newForwardRig builds the rig with the switch on (or off), and mk as the relay each ExecStream gets.
func newForwardRig(t *testing.T, on bool, mk func() *forwardRelay) *forwardRig {
	t.Helper()
	return newForwardRigOver(t, on, mk, nil)
}

// newForwardRigOver is newForwardRig with the store the rig's server reads wrapped by wrap (nil: as it is).
func newForwardRigOver(t *testing.T, on bool, mk func() *forwardRelay, wrap func(store.Store) store.Store) *forwardRig {
	t.Helper()
	if on {
		t.Setenv(envAttachForward, "on")
	} else {
		t.Setenv(envAttachForward, "")
	}
	fr := &forwardRig{mk: mk}
	fr.st = &forwardStore{}
	fr.st.rule.Store(types.TransferAllowed)
	fr.authorityRig = newAuthorityRigWith(t, 0, func(cfg *Config) {
		fr.st.Store = cfg.Store
		if wrap != nil {
			fr.st.Store = wrap(cfg.Store)
		}
		cfg.Store = fr.st
		cfg.Runner = &sshFakeRunner{execFn: fr.exec}
	})
	fr.runID = fr.addRun(forwardOwner, nil)
	return fr
}

func (fr *forwardRig) exec(spec runner.ExecSpec) (*runner.ExecSession, error) {
	fr.execs.Add(1)
	if fr.execErr != nil {
		return nil, fr.execErr
	}
	mk := fr.mk
	if mk == nil {
		mk = func() *forwardRelay { return newForwardRelay(false) }
	}
	f := mk()
	fr.mu.Lock()
	fr.relays = append(fr.relays, f)
	fr.specs = append(fr.specs, spec)
	fr.mu.Unlock()
	return f.session(), nil
}

func (fr *forwardRig) relay(i int) *forwardRelay {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if i >= len(fr.relays) {
		return nil
	}
	return fr.relays[i]
}

// addRun stores a running run created by createdBy, after mutate.
func (fr *forwardRig) addRun(createdBy string, mutate func(*types.AgentRun)) uuid.UUID {
	run := types.AgentRun{ID: uuid.New(), CreatedBy: createdBy, State: types.RunRunning, SandboxRef: "sbx-1"}
	if mutate != nil {
		mutate(&run)
	}
	fr.ast.mu.Lock()
	fr.ast.runs[run.ID] = run
	fr.ast.mu.Unlock()
	return run.ID
}

func (fr *forwardRig) setRun(id uuid.UUID, mutate func(*types.AgentRun)) {
	fr.ast.mu.Lock()
	defer fr.ast.mu.Unlock()
	run := fr.ast.runs[id]
	mutate(&run)
	fr.ast.runs[id] = run
}

// ticketFor mints an attach ticket for principal on run, stamped role, at the revocation sequence now.
func (fr *forwardRig) ticketFor(t *testing.T, run uuid.UUID, principal, role string) string {
	t.Helper()
	tok, err := mintAttachTicket(context.Background(), fr.srv.cfg.Store, run, types.ActorHuman, principal, role,
		oidc.Issue{At: time.Now(), Seq: fr.rev.current()})
	if err != nil {
		t.Fatalf("mint ticket: %v", err)
	}
	return tok
}

// mint is the owner's ticket for the rig's run, through POST /attach/ticket.
func (fr *forwardRig) mint(t *testing.T) string {
	t.Helper()
	return fr.ticketFor(t, fr.runID, forwardOwner, oidc.RoleUser)
}

func forwardPath(run uuid.UUID) string { return "/api/v1/runs/" + run.String() + "/attach/forward" }

// get is the route through the handler, with no socket: every refusal answers before the upgrade.
func (fr *forwardRig) get(t *testing.T, run uuid.UUID, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, forwardPath(run)+"?"+rawQuery, nil)
	w := httptest.NewRecorder()
	panicFails(t, fr.srv.Handler()).ServeHTTP(w, req)
	return w
}

// refusedAs is the owner's ticket spent on port, and the answer.
func (fr *forwardRig) refusedAs(t *testing.T, port string, extra string) *httptest.ResponseRecorder {
	t.Helper()
	return fr.get(t, fr.runID, "port="+port+extra+"&ticket="+fr.mint(t))
}

func (fr *forwardRig) wsURL(run uuid.UUID, rawQuery string) string {
	return "ws" + strings.TrimPrefix(fr.ts.URL, "http") + forwardPath(run) + "?" + rawQuery
}

func (fr *forwardRig) dialQuery(t *testing.T, rawQuery string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return websocket.Dial(ctx, fr.wsURL(fr.runID, rawQuery), nil)
}

// dial opens a forward to port with a fresh owner ticket.
func (fr *forwardRig) dial(t *testing.T, port string) *websocket.Conn {
	t.Helper()
	c, resp, err := fr.dialQuery(t, "port="+port+"&ticket="+fr.mint(t))
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("dial forward: %v (status %d)", err, code)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

func (fr *forwardRig) forwardRows() []types.AuditEvent {
	return execRows(fr.audit.snapshot(), "run.port.forward")
}

func (fr *forwardRig) waitRows(t *testing.T, n int) []types.AuditEvent {
	t.Helper()
	var rows []types.AuditEvent
	waitFor(t, fmt.Sprintf("%d run.port.forward rows", n), func() bool {
		rows = fr.forwardRows()
		return len(rows) >= n
	})
	return rows
}

func (fr *forwardRig) waitRelayOpen(t *testing.T) *forwardRelay {
	t.Helper()
	waitFor(t, "the relay to start", func() bool { return fr.relay(0) != nil })
	return fr.relay(0)
}

func rowFields(t *testing.T, ev types.AuditEvent) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(ev.Data, &m); err != nil {
		t.Fatalf("decode row %s: %v", ev.Data, err)
	}
	return m
}

// slotsFree reports whether the run holds none of its SSH channel slots: every one can be taken, then they are given back.
func (fr *forwardRig) slotsFree() bool {
	taken := 0
	for fr.srv.sshAcquireSession(fr.runID) {
		taken++
	}
	for range taken {
		fr.srv.sshReleaseSession(fr.runID)
	}
	return taken == fr.srv.cfg.SSHMaxSessionsPerRun
}

func (fr *forwardRig) wantReason(t *testing.T, w *httptest.ResponseRecorder, status int, reason string) {
	t.Helper()
	if w.Code != status || wireReason(t, w) != reason {
		t.Errorf("GET forward = %d %s, want %d %s", w.Code, w.Body, status, reason)
	}
}

func (fr *forwardRig) wantNoExec(t *testing.T) {
	t.Helper()
	if n := fr.execs.Load(); n != 0 {
		t.Errorf("ExecStream was called %d times, want 0", n)
	}
}

func TestAttachForward_OffByDefault(t *testing.T) {
	fr := newForwardRig(t, false, nil)
	for _, extra := range []string{"", "&check=1"} {
		w := fr.refusedAs(t, "3000", extra)
		fr.wantReason(t, w, http.StatusConflict, reasonRunForwardOff)
		if !strings.Contains(w.Body.String(), "WARDYN_ATTACH_FORWARD") {
			t.Errorf("the refusal %s does not name WARDYN_ATTACH_FORWARD", w.Body)
		}
	}
	rows := fr.forwardRows()
	if len(rows) != 2 {
		t.Fatalf("run.port.forward rows = %d, want one per request", len(rows))
	}
	for _, ev := range rows {
		if ev.Outcome != "failure" || rowFields(t, ev)["reason"] != reasonRunForwardOff {
			t.Errorf("row = %s %s, want a failure with reason %s", ev.Outcome, ev.Data, reasonRunForwardOff)
		}
	}
	if n := len(execRows(fr.audit.snapshot(), "ssh.forward")); n != 0 {
		t.Errorf("ssh.forward rows = %d, want 0", n)
	}
	fr.wantNoExec(t)
}

func TestAttachForward_OffAnswersBeforeThePort(t *testing.T) {
	fr := newForwardRig(t, false, nil)
	fr.wantReason(t, fr.refusedAs(t, "abc", ""), http.StatusConflict, reasonRunForwardOff)
}

func TestAttachForward_OffValueIsOff(t *testing.T) {
	for _, v := range []string{"off", "0"} {
		fr := newForwardRig(t, false, nil)
		t.Setenv(envAttachForward, v)
		fr.wantReason(t, fr.refusedAs(t, "3000", ""), http.StatusConflict, reasonRunForwardOff)
	}
}

func TestAttachForward_OffTellsANonOwnerNothing(t *testing.T) {
	fr := newForwardRig(t, false, nil)
	tk := fr.ticketFor(t, fr.runID, "some-admin", oidc.RoleAdmin)
	w := fr.get(t, fr.runID, "port=3000&ticket="+tk)
	fr.wantReason(t, w, http.StatusForbidden, runOwnerOnlyReason)
	if n := len(fr.forwardRows()); n != 0 {
		t.Errorf("run.port.forward rows = %d, want none: a caller who may not enter learns nothing from the switch", n)
	}
}

// TestAttachForward_SwitchReadIsOneFunction: envAttachForward is read by exactly one envEnabled call, inside attachForwardOn.
func TestAttachForward_SwitchReadIsOneFunction(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var calls, literals []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Value == `"`+envAttachForward+`"` {
				literals = append(literals, name)
			}
			return true
		})
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "envEnabled" {
					return true
				}
				for _, a := range call.Args {
					if id, ok := a.(*ast.Ident); ok && id.Name == "envAttachForward" {
						calls = append(calls, name+":"+fd.Name.Name)
					}
				}
				return true
			})
		}
	}
	if want := []string{"attach_forward.go:attachForwardOn"}; !slices.Equal(calls, want) {
		t.Errorf("envEnabled(envAttachForward) is called in %v, want %v", calls, want)
	}
	// The name is spelled once, as the const: any other literal could read the switch around attachForwardOn.
	if want := []string{"runs_dispatch_mounts.go"}; !slices.Equal(literals, want) {
		t.Errorf("the literal %q appears in %v, want only %v", envAttachForward, literals, want)
	}
}

func TestAttachForward_ChannelsLeaveOutForward(t *testing.T) {
	fr := newForwardRig(t, true, nil)
	fr.srv.cfg.SSHChannels = map[string]bool{"shell": true}
	w := fr.refusedAs(t, "3000", "")
	fr.wantReason(t, w, http.StatusConflict, reasonRunForwardOff)
	if !strings.Contains(w.Body.String(), "WARDYN_SSH_CHANNELS") {
		t.Errorf("the refusal %s does not name WARDYN_SSH_CHANNELS", w.Body)
	}
	rows := execRows(fr.audit.snapshot(), "ssh.channel.reject")
	if len(rows) != 1 || rowFields(t, rows[0])["reason"] != "channel_off" {
		t.Errorf("ssh.channel.reject rows = %v, want one with reason channel_off", rows)
	}
	fr.wantNoExec(t)
}

func TestAttachForward_Refusals(t *testing.T) {
	for _, port := range []string{"", "0", "65536", "80a", "%2B80", "0x50"} {
		t.Run("port_"+port, func(t *testing.T) {
			fr := newForwardRig(t, true, nil)
			fr.wantReason(t, fr.refusedAs(t, port, ""), http.StatusBadRequest, reasonRunForwardInvalid)
			fr.wantNoExec(t)
			if n := len(fr.forwardRows()); n != 0 {
				t.Errorf("a bad port wrote %d run.port.forward rows, want none", n)
			}
		})
	}
	for _, tc := range []struct {
		name    string
		rule    types.TransferRule
		readErr bool
		want    types.TransferRule
	}{
		{"review", types.TransferReview, false, types.TransferReview},
		{"blocked", types.TransferBlocked, false, types.TransferBlocked},
		{"unreadable", types.TransferAllowed, true, types.TransferBlocked},
	} {
		t.Run("data_out_"+tc.name, func(t *testing.T) {
			fr := newForwardRig(t, true, nil)
			fr.st.rule.Store(tc.rule)
			fr.st.ruleErr.Store(tc.readErr)
			fr.wantReason(t, fr.refusedAs(t, "3000", ""), http.StatusConflict, reasonDataOutBlocked)
			fr.wantNoExec(t)
			rows := execRows(fr.audit.snapshot(), "run.data_out.deny")
			if len(rows) != 1 || !strings.Contains(string(rows[0].Data), `"path":"port_forward"`) ||
				!strings.Contains(string(rows[0].Data), `"rule":"`+string(tc.want)+`"`) {
				t.Errorf("run.data_out.deny rows = %v, want one with path port_forward and rule %s", rows, tc.want)
			}
		})
	}
	t.Run("ended_run", func(t *testing.T) {
		fr := newForwardRig(t, true, nil)
		tk := fr.mint(t)
		fr.setRun(fr.runID, func(r *types.AgentRun) { r.State = types.RunCompleted })
		fr.wantReason(t, fr.get(t, fr.runID, "port=3000&ticket="+tk), http.StatusConflict, reasonRunForwardUnavailable)
		fr.wantNoExec(t)
		if rows := fr.forwardRows(); len(rows) != 1 || rows[0].Outcome != "failure" {
			t.Errorf("run.port.forward rows = %v, want one failure", rows)
		}
	})
	t.Run("background_run", func(t *testing.T) {
		fr := newForwardRig(t, true, nil)
		fr.setRun(fr.runID, func(r *types.AgentRun) { r.Experience = types.ExperienceBackground })
		fr.wantReason(t, fr.refusedAs(t, "3000", ""), http.StatusConflict, reasonRunBackgroundOnly)
		fr.wantNoExec(t)
	})
	t.Run("macos_screen_sharing_port", func(t *testing.T) {
		fr := newForwardRig(t, true, nil)
		fr.setRun(fr.runID, func(r *types.AgentRun) { r.MachineKind = types.MachineMacOSVM })
		fr.wantReason(t, fr.refusedAs(t, "5900", ""), http.StatusForbidden, reasonRunForwardPortRefused)
		fr.wantNoExec(t)
		if n := len(fr.forwardRows()); n != 0 {
			t.Errorf("a refused Screen Sharing port wrote %d rows, want none", n)
		}
	})
	t.Run("deny_interactive", func(t *testing.T) {
		fr := newForwardRig(t, true, nil)
		profile := types.GovernanceUnit{ID: uuid.New(), Name: "ci", Limits: types.GovernanceLimits{DenyInteractive: true}}
		fr.units.set(profile)
		fr.setRun(fr.runID, func(r *types.AgentRun) { r.UnitID = &profile.ID })
		w := fr.refusedAs(t, "3000", "")
		if w.Code != http.StatusForbidden || wireReason(t, w) != string(authz.ReasonGovernanceUnit) {
			t.Errorf("GET forward under deny_interactive = %d %s, want 403 %s", w.Code, w.Body, authz.ReasonGovernanceUnit)
		}
		fr.wantNoExec(t)
	})
	t.Run("all_slots_held", func(t *testing.T) {
		fr := newForwardRig(t, true, nil)
		fr.srv.cfg.SSHMaxSessionsPerRun = 1
		if !fr.srv.sshAcquireSession(fr.runID) {
			t.Fatal("could not take the run's one slot")
		}
		fr.wantReason(t, fr.refusedAs(t, "3000", ""), http.StatusTooManyRequests, reasonRunForwardBusy)
		fr.wantNoExec(t)
		if rows := fr.forwardRows(); len(rows) != 1 || !strings.Contains(string(rows[0].Data), sshCapReasonChannels) {
			t.Errorf("run.port.forward rows = %v, want one naming %s", rows, sshCapReasonChannels)
		}
	})
	t.Run("exec_stream_unsupported", func(t *testing.T) {
		fr := newForwardRig(t, true, nil)
		fr.execErr = runner.ErrExecStreamUnsupported
		fr.wantReason(t, fr.refusedAs(t, "3000", ""), http.StatusNotImplemented, reasonRunInspectExecStreamUnsupported)
		if n := fr.execs.Load(); n != 1 {
			t.Errorf("ExecStream calls = %d, want 1", n)
		}
		if !fr.slotsFree() {
			t.Error("a refused start left a channel slot held")
		}
	})
	t.Run("start_error", func(t *testing.T) {
		fr := newForwardRig(t, true, nil)
		fr.execErr = errors.New("the runner is down")
		fr.wantReason(t, fr.refusedAs(t, "3000", ""), http.StatusBadGateway, reasonRunForwardFailed)
		if n := fr.execs.Load(); n != 1 {
			t.Errorf("ExecStream calls = %d, want 1", n)
		}
		if rows := fr.forwardRows(); len(rows) != 1 || rows[0].Outcome != "failure" {
			t.Errorf("run.port.forward rows = %v, want one failure", rows)
		}
	})
	t.Run("local_mode_peer", func(t *testing.T) {
		fr := newForwardRig(t, true, nil)
		fr.srv.cfg.LocalMode = true
		req := httptest.NewRequest(http.MethodGet, forwardPath(fr.runID)+"?port=3000&ticket="+fr.mint(t), nil)
		req.RemoteAddr = "192.0.2.10:4444"
		w := httptest.NewRecorder()
		panicFails(t, fr.srv.Handler()).ServeHTTP(w, req)
		fr.wantReason(t, w, http.StatusForbidden, reasonLocalModePeerNotLoopback)
	})
	t.Run("local_mode_host", func(t *testing.T) {
		fr := newForwardRig(t, true, nil)
		fr.srv.cfg.LocalMode = true
		req := httptest.NewRequest(http.MethodGet, forwardPath(fr.runID)+"?port=3000&ticket="+fr.mint(t), nil)
		req.RemoteAddr = "127.0.0.1:4444"
		req.Host = "rebind.example"
		w := httptest.NewRecorder()
		panicFails(t, fr.srv.Handler()).ServeHTTP(w, req)
		fr.wantReason(t, w, http.StatusForbidden, reasonLocalModeHostNotLoopback)
	})
	t.Run("cross_origin", func(t *testing.T) {
		fr := newForwardRig(t, true, nil)
		req := httptest.NewRequest(http.MethodGet, forwardPath(fr.runID)+"?port=3000&ticket="+fr.mint(t), nil)
		req.Header.Set("Origin", "https://evil.example")
		w := httptest.NewRecorder()
		panicFails(t, fr.srv.Handler()).ServeHTTP(w, req)
		fr.wantReason(t, w, http.StatusForbidden, csrfAuditReason)
		if rows := execRows(fr.audit.snapshot(), "auth.fail"); len(rows) != 1 {
			t.Errorf("auth.fail rows = %d, want 1", len(rows))
		}
		fr.wantNoExec(t)
	})
	t.Run("admin_without_a_ticket", func(t *testing.T) {
		fr := newForwardRig(t, true, nil)
		before := fr.st.getRunCalls.Load()
		req := httptest.NewRequest(http.MethodGet, forwardPath(fr.runID)+"?port=3000", nil)
		req.AddCookie(fr.cookie(t, "some-admin", time.Now().Add(time.Hour)))
		w := httptest.NewRecorder()
		panicFails(t, fr.srv.Handler()).ServeHTTP(w, req)
		fr.wantReason(t, w, http.StatusBadRequest, reasonRunForwardTicketRequired)
		if n := fr.st.getRunCalls.Load() - before; n != 0 {
			t.Errorf("the ticket-less lane read the run %d times, want 0", n)
		}
		if n := len(fr.forwardRows()); n != 0 {
			t.Errorf("run.port.forward rows = %d, want none", n)
		}
	})
}

// forwardAs calls the handler with ta already redeemed into the context, as ticketOrHumanAuth leaves it.
func (fr *forwardRig) forwardAs(t *testing.T, ta ticketActor) *httptest.ResponseRecorder {
	t.Helper()
	rc := chi.NewRouteContext()
	rc.URLParams.Add("id", fr.runID.String())
	req := httptest.NewRequest(http.MethodGet, forwardPath(fr.runID)+"?port=3000", nil)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rc)
	req = req.WithContext(withTicketActor(ctx, ta))
	w := httptest.NewRecorder()
	fr.srv.handleAttachForward(w, req)
	return w
}

func TestAttachForward_TicketKinds(t *testing.T) {
	fr := newForwardRig(t, true, nil)
	base := ticketActor{actorType: types.ActorHuman, principal: forwardOwner, role: oidc.RoleUser, authorizedAt: time.Now()}
	via := &types.DelegationVia{Grant: uuid.New(), Delegate: uuid.New()}
	for name, mod := range map[string]func(*ticketActor){
		"helper":        func(a *ticketActor) { a.support = uuid.New() },
		"portal":        func(a *ticketActor) { a.via = via },
		"issuer_portal": func(a *ticketActor) { a.admitBy = time.Now().Add(time.Minute) },
		"app":           func(a *ticketActor) { a.app = "notebook" },
	} {
		t.Run(name, func(t *testing.T) {
			ta := base
			mod(&ta)
			fr.wantReason(t, fr.forwardAs(t, ta), http.StatusForbidden, reasonAttachTicketNotYourRun)
		})
	}
	fr.wantNoExec(t)
	t.Run("member_cookie_without_a_ticket", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, forwardPath(fr.runID)+"?port=3000", nil)
		req.AddCookie(fr.memberCookie(t, forwardOwner))
		w := httptest.NewRecorder()
		panicFails(t, fr.srv.Handler()).ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("a member's cookie with no ticket = %d %s, want 403", w.Code, w.Body)
		}
	})
}

// memberCookie is a member's session cookie for sub: authorityRig.cookie with the user role.
func (fr *forwardRig) memberCookie(t *testing.T, sub string) *http.Cookie {
	t.Helper()
	payload, err := json.Marshal(oidc.Session{
		V: oidc.SessionCodecVersion, Sub: sub, Email: sub, Role: oidc.RoleUser, UserType: types.UserTypeStandard,
		Expiry: time.Now().UTC().Add(time.Hour), IssuedAt: time.Now().UTC(), RevocationSeq: fr.rev.current(), Groups: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, accessTestHMACKey)
	mac.Write(payload)
	return &http.Cookie{Name: "wardyn_session",
		Value: base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))}
}

func TestAttachForward_TicketLaneOwnerRule(t *testing.T) {
	fr := newForwardRig(t, true, nil)
	// A ticket the owner minted for their own run B and presented on their own run A is refused at redemption: the
	// entry rule would let the same person into A, so only the ticket's run binding refuses it.
	otherOwn := fr.addRun(forwardOwner, nil)
	tk := fr.ticketFor(t, otherOwn, forwardOwner, oidc.RoleUser)
	fr.wantReason(t, fr.get(t, fr.runID, "port=3000&check=1&ticket="+tk), http.StatusForbidden, reasonUIGatewayTicketInvalid)
	// An admin-stamped ticket on a person's run is refused by the owner rule.
	adminTk := fr.ticketFor(t, fr.runID, "some-admin", oidc.RoleAdmin)
	fr.wantReason(t, fr.get(t, fr.runID, "port=3000&ticket="+adminTk), http.StatusForbidden, runOwnerOnlyReason)
	fr.wantNoExec(t)
}

// TestAttachForward_AcceptFailureWritesTheStartFailureRow: a request that is not a WebSocket upgrade passes every
// check and starts the relay, then Accept refuses it: the relay is closed, the slot is free and one failure row says so.
func TestAttachForward_AcceptFailureWritesTheStartFailureRow(t *testing.T) {
	fr := newForwardRig(t, true, nil)
	w := fr.refusedAs(t, "3000", "")
	if w.Code < 400 {
		t.Errorf("a plain GET = %d %s, want Accept's refusal", w.Code, w.Body)
	}
	if n := fr.execs.Load(); n != 1 {
		t.Fatalf("ExecStream calls = %d, want 1", n)
	}
	if relay := fr.relay(0); relay == nil || relay.closes.Load() == 0 {
		t.Error("the relay was left open after Accept failed")
	}
	rows := fr.waitRows(t, 1)
	f := rowFields(t, rows[0])
	if len(rows) != 1 || rows[0].Outcome != "failure" || f["port"] != float64(3000) || f["error"] != attachForwardFailedMsg {
		t.Errorf("rows = %v, want one failure on port 3000 with error %q", rows, attachForwardFailedMsg)
	}
	if !fr.slotsFree() {
		t.Error("a failed Accept left a channel slot held")
	}
}

// TestAttachForward_RecheckReadFailures: with no credential check (base nil, a super admin's ticket with no
// revocation store), a run that cannot be read ends the forward as a failed read, never as an ended run.
func TestAttachForward_RecheckReadFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		prep func(*forwardRig)
		want error
	}{
		{"run_read_fails", func(fr *forwardRig) { fr.st.getRunErr.Store(true) }, errAttachForwardReadFailed},
		{"run_completed", func(fr *forwardRig) { fr.setRun(fr.runID, func(r *types.AgentRun) { r.State = types.RunCompleted }) }, errAttachForwardRunEnded},
		{"rule_read_fails", func(fr *forwardRig) { fr.st.ruleErr.Store(true) }, errAttachForwardReadFailed},
		{"rule_blocked", func(fr *forwardRig) { fr.st.rule.Store(types.TransferBlocked) }, errAttachForwardDataOut},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr := newForwardRig(t, true, nil)
			tc.prep(fr)
			var cause error
			stop := func(c error) error { cause = c; return errAttachAuthorityEnded }
			if err := fr.srv.attachForwardRecheck(fr.runID, nil, stop)(context.Background()); err == nil {
				t.Fatal("the re-check passed")
			}
			if !errors.Is(cause, tc.want) {
				t.Errorf("the re-check ended with %v, want %v", cause, tc.want)
			}
		})
	}
}

func TestAttachForward_CheckOpensNothing(t *testing.T) {
	fr := newForwardRig(t, true, nil)
	w := fr.refusedAs(t, "3000", "&check=1")
	if w.Code != http.StatusNoContent {
		t.Fatalf("GET forward &check=1 = %d %s, want 204", w.Code, w.Body)
	}
	fr.wantNoExec(t)
	if n := len(fr.forwardRows()); n != 0 {
		t.Errorf("run.port.forward rows = %d, want none", n)
	}
	if !fr.slotsFree() {
		t.Error("a check left a channel slot held")
	}
}

func TestAttachForward_RelaysBytesAndAudits(t *testing.T) {
	fr := newForwardRig(t, true, func() *forwardRelay { return newForwardRelay(true) })
	c := fr.dial(t, "3000")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageBinary, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	typ, got, err := c.Read(ctx)
	if err != nil || typ != websocket.MessageBinary || string(got) != "ping" {
		t.Fatalf("read = %v %q %v, want binary ping", typ, got, err)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
	rows := fr.waitRows(t, 1)
	fields := rowFields(t, rows[0])
	if rows[0].Outcome != "success" || fields["port"] != float64(3000) || fields["bytes"].(float64) < 4 {
		t.Errorf("row = %s %s, want a success on port 3000 with at least 4 bytes", rows[0].Outcome, rows[0].Data)
	}
	fr.mu.Lock()
	argv := fr.specs[0].Argv
	fr.mu.Unlock()
	if want := sshRelayArgv("", 3000); !slices.Equal(argv, want) {
		t.Errorf("relay argv = %v, want %v", argv, want)
	}
	if fr.units.touches() == 0 {
		t.Error("the run's idle clock was never touched")
	}
	waitFor(t, "the slot to be released", fr.slotsFree)
}

func TestAttachForward_ClientCloseEndsTheRelay(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(*websocket.Conn)
	}{
		{"close_frame", func(c *websocket.Conn) { _ = c.Close(websocket.StatusNormalClosure, "") }},
		{"tcp_drop", func(c *websocket.Conn) { _ = c.CloseNow() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr := newForwardRig(t, true, nil)
			fr.srv.keepaliveEvery = 5 * time.Millisecond
			c := fr.dial(t, "3000")
			relay := fr.waitRelayOpen(t)
			waitFor(t, "the keepalive to touch the run", func() bool { return fr.units.touches() > 1 })
			tc.end(c)
			waitFor(t, "the relay to be closed", func() bool { return relay.closes.Load() > 0 })
			rows := fr.waitRows(t, 1)
			if rows[0].Outcome != "success" {
				t.Errorf("row = %s %s, want a success (the client closed first)", rows[0].Outcome, rows[0].Data)
			}
			waitFor(t, "the slot to be released", fr.slotsFree)
			at := fr.units.touches()
			time.Sleep(10 * fr.srv.keepaliveEvery)
			if now := fr.units.touches(); now != at {
				t.Errorf("the run was touched %d more times after the forward ended", now-at)
			}
		})
	}
}

// TestAttachForward_StalledTransferIsNotCut: a window in which either copy waits on a write is not a ping timeout.
func TestAttachForward_StalledTransferIsNotCut(t *testing.T) {
	t.Run("reader_waiting", func(t *testing.T) {
		fr := newForwardRig(t, true, func() *forwardRelay {
			f := newForwardRelay(false)
			f.stdinGate = make(chan struct{})
			return f
		})
		fr.srv.pingEvery = forwardTick
		fr.srv.cfg.SSHMaxSessionsPerRun = 1
		c := fr.dial(t, "3000")
		relay := fr.waitRelayOpen(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := c.Write(ctx, websocket.MessageBinary, []byte("x")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * forwardTick)
		if relay.closes.Load() != 0 || len(fr.forwardRows()) != 0 || fr.srv.sshAcquireSession(fr.runID) {
			t.Fatalf("the forward was cut while the sandbox's stdin write waited (closes %d, rows %d)", relay.closes.Load(), len(fr.forwardRows()))
		}
		go func() {
			for {
				if _, _, err := c.Read(context.Background()); err != nil {
					return
				}
			}
		}()
		close(relay.stdinGate)
		time.Sleep(5 * forwardTick)
		if relay.closes.Load() != 0 {
			t.Fatal("the forward was cut after the stdin write was released")
		}
		_ = c.Close(websocket.StatusNormalClosure, "")
		rows := fr.waitRows(t, 1)
		if _, has := rowFields(t, rows[0])["error"]; rows[0].Outcome != "success" || has {
			t.Errorf("row = %s %s, want a success with no error", rows[0].Outcome, rows[0].Data)
		}
	})
	t.Run("writer_waiting", func(t *testing.T) {
		fr := newForwardRig(t, true, func() *forwardRelay {
			f := newForwardRelay(false)
			f.flood = true
			return f
		})
		fr.srv.pingEvery = forwardTick
		c := fr.dial(t, "3000")
		relay := fr.waitRelayOpen(t)
		last, still := relay.stdoutReads.Load(), 0
		waitFor(t, "the writer to fill the socket buffers and wait", func() bool {
			time.Sleep(forwardTick)
			now := relay.stdoutReads.Load()
			if now == last {
				still++
			} else {
				still, last = 0, now
			}
			return still >= 2
		})
		time.Sleep(10 * forwardTick)
		if relay.closes.Load() != 0 || len(fr.forwardRows()) != 0 {
			t.Fatalf("the forward was cut while its writer waited on the client (closes %d, rows %d)", relay.closes.Load(), len(fr.forwardRows()))
		}
		_ = c.CloseNow()
		rows := fr.waitRows(t, 1)
		if rows[0].Outcome != "success" || strings.Contains(string(rows[0].Data), attachForwardEndPing) {
			t.Errorf("row = %s %s, want a success, never %q", rows[0].Outcome, rows[0].Data, attachForwardEndPing)
		}
	})
}

// TestAttachForward_GonePeerIsReaped: no TCP read, no pong, both copies free: the ping ends the forward.
func TestAttachForward_GonePeerIsReaped(t *testing.T) {
	for _, tc := range []string{"no_byte_moved", "after_one_byte_each_way"} {
		t.Run(tc, func(t *testing.T) {
			fr := newForwardRig(t, true, func() *forwardRelay { return newForwardRelay(tc != "no_byte_moved") })
			fr.srv.pingEvery = forwardTick
			c := fr.dial(t, "3000")
			relay := fr.waitRelayOpen(t)
			start := time.Now() // the dial, or the last byte below
			if tc != "no_byte_moved" {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := c.Write(ctx, websocket.MessageBinary, []byte("a")); err != nil {
					t.Fatal(err)
				}
				if _, _, err := c.Read(ctx); err != nil {
					t.Fatal(err)
				}
				start = time.Now()
			}
			// The client reads nothing from here on, so it answers no ping.
			waitFor(t, "the relay to be closed by the ping", func() bool { return relay.closes.Load() > 0 })
			if took := time.Since(start); took > 20*forwardTick {
				t.Errorf("the gone peer was reaped after %s, want within 20 intervals (%s)", took, 20*forwardTick)
			}
			rows := fr.waitRows(t, 1)
			if rows[0].Outcome != "failure" || rowFields(t, rows[0])["error"] != attachForwardEndPing {
				t.Errorf("row = %s %s, want a failure with error %q", rows[0].Outcome, rows[0].Data, attachForwardEndPing)
			}
			waitFor(t, "the slot to be released", fr.slotsFree)
		})
	}
}

func TestAttachForward_PingVerdict(t *testing.T) {
	timeout := fmt.Errorf("ping: %w", context.DeadlineExceeded)
	for _, tc := range []struct {
		name             string
		err, pErr, ctxEr error
		parked, want     bool
	}{
		{"no error", nil, context.DeadlineExceeded, nil, false, false},
		{"own deadline", timeout, context.DeadlineExceeded, nil, false, true},
		{"own deadline, parked", timeout, context.DeadlineExceeded, nil, true, false},
		{"closed socket racing the deadline", net.ErrClosed, context.DeadlineExceeded, nil, false, false},
		{"control write's own wait", timeout, nil, nil, false, false},
		{"forward already ending", timeout, context.DeadlineExceeded, context.Canceled, false, false},
	} {
		if got := forwardPingCounts(tc.err, tc.pErr, tc.ctxEr, tc.parked); got != tc.want {
			t.Errorf("%s: forwardPingCounts = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestAttachForward_CleanCloseIsNotAPingTimeout: a client that closes with a ping in flight is a success.
func TestAttachForward_CleanCloseIsNotAPingTimeout(t *testing.T) {
	fr := newForwardRig(t, true, nil)
	fr.srv.pingEvery = 200 * time.Millisecond
	for i := range 20 {
		c := fr.dial(t, "3000")
		time.Sleep(300 * time.Millisecond)
		if err := c.Close(websocket.StatusNormalClosure, ""); err != nil {
			t.Logf("run %d: close: %v", i, err)
		}
		rows := fr.waitRows(t, i+1)
		ev := rows[i]
		if _, has := rowFields(t, ev)["error"]; ev.Outcome != "success" || has {
			t.Fatalf("run %d: row = %s %s, want a success with no error", i, ev.Outcome, ev.Data)
		}
	}
}

func TestAttachForward_ParkWindow(t *testing.T) {
	var p forwardPark
	p.park()
	p.free()
	if p.parkedSince(forwardNow()) {
		t.Error("a free park reads as parked after the window")
	}
	p.park()
	if !p.parkedSince(forwardNow()) {
		t.Error("a parked copy reads as free")
	}
	before := forwardNow()
	p.free()
	after := forwardNow() + 1
	if !p.parkedSince(before) {
		t.Error("a window that held a wait reads as free")
	}
	if p.parkedSince(after) {
		t.Error("a window after the last wait reads as parked")
	}
}

func TestAttachForward_EndsWhenAuthorityEnds(t *testing.T) {
	fr := newForwardRig(t, true, nil)
	c := fr.dial(t, "3000")
	relay := fr.waitRelayOpen(t)
	if err := fr.rev.RevokeSub(context.Background(), forwardOwner); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the relay to be closed", func() bool { return relay.closes.Load() > 0 })
	rows := fr.waitRows(t, 1)
	if rows[0].Outcome != "failure" || rowFields(t, rows[0])["error"] != attachForwardEndSession {
		t.Errorf("row = %s %s, want a failure with error %q", rows[0].Outcome, rows[0].Data, attachForwardEndSession)
	}
	if _, _, err := c.Read(context.Background()); err == nil {
		t.Error("the socket stayed open")
	}
}

func TestAttachForward_EndsWhenTheRunStops(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stop  func(*forwardRig)
		words string
	}{
		{"completed", func(fr *forwardRig) { fr.setRun(fr.runID, func(r *types.AgentRun) { r.State = types.RunCompleted }) }, attachForwardEndRun},
		{"kept", func(fr *forwardRig) {
			now := time.Now()
			fr.setRun(fr.runID, func(r *types.AgentRun) { r.LostAt = &now })
		}, attachForwardEndRun},
		{"run_unreadable", func(fr *forwardRig) { fr.st.getRunErr.Store(true) }, attachForwardEndReadFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr := newForwardRig(t, true, nil)
			fr.dial(t, "3000")
			relay := fr.waitRelayOpen(t)
			tc.stop(fr)
			waitFor(t, "the relay to be closed", func() bool { return relay.closes.Load() > 0 })
			rows := fr.waitRows(t, 1)
			if rows[0].Outcome != "failure" || rowFields(t, rows[0])["error"] != tc.words {
				t.Errorf("row = %s %s, want a failure with error %q", rows[0].Outcome, rows[0].Data, tc.words)
			}
		})
	}
}

func TestAttachForward_EndsWhenDataOutTightens(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flip  func(*forwardStore)
		words string
	}{
		{"rule_blocked", func(s *forwardStore) { s.rule.Store(types.TransferBlocked) }, attachForwardEndDataOut},
		{"rule_unreadable", func(s *forwardStore) { s.ruleErr.Store(true) }, attachForwardEndReadFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr := newForwardRig(t, true, nil)
			fr.dial(t, "3000")
			relay := fr.waitRelayOpen(t)
			tc.flip(fr.st)
			waitFor(t, "the relay to be closed", func() bool { return relay.closes.Load() > 0 })
			rows := fr.waitRows(t, 1)
			if rows[0].Outcome != "failure" || rowFields(t, rows[0])["error"] != tc.words {
				t.Errorf("row = %s %s, want a failure with error %q", rows[0].Outcome, rows[0].Data, tc.words)
			}
		})
	}
}

func TestAttachForward_SharesTheSSHChannelCap(t *testing.T) {
	fr := newForwardRig(t, true, nil)
	fr.srv.cfg.SSHMaxSessionsPerRun = 1
	fr.dial(t, "3000")
	fr.waitRelayOpen(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+fr.runID.String()+"/exec", strings.NewReader(`{"command":"true"}`))
	req.Header.Set("Origin", "http://"+req.Host)
	req.AddCookie(fr.memberCookie(t, forwardOwner))
	w := httptest.NewRecorder()
	panicFails(t, fr.srv.Handler()).ServeHTTP(w, req)
	fr.wantReason(t, w, http.StatusTooManyRequests, reasonRunExecBusy)
}
