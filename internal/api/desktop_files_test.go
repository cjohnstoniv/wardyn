// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// envelopeStore answers the run's run.policy.resolve envelope, the source desktopRun reads, or
// readErr, or (noEnvelope) a log with no envelope in it. With answered > 0 it answers that many
// reads and fails every later one.
type envelopeStore struct {
	store.Store
	spec            types.RunPolicySpec
	readErr         error
	noEnvelope      bool
	reads, answered int
}

func (s *envelopeStore) QueryAuditEvents(_ context.Context, runID uuid.UUID, _ int) ([]types.AuditEvent, error) {
	s.reads++
	if s.answered > 0 && s.reads > s.answered {
		return nil, errors.New("audit store unavailable")
	}
	if s.readErr != nil || s.noEnvelope {
		return nil, s.readErr
	}
	data, _ := json.Marshal(s.spec)
	return []types.AuditEvent{{ID: uuid.New(), RunID: &runID, Action: "run.policy.resolve", Outcome: "success", Data: data}}, nil
}

// fakeTransferFolder plays the three in-sandbox scripts against a map. A nonzero failCode replaces the
// exit code after the script has run, its output included; waitErr is what Wait reports, as when the exec
// stream is lost; stdoutErr fails the first read of stdout.
type fakeTransferFolder struct {
	mu        sync.Mutex
	files     map[string][]byte
	execs     int
	failCode  int
	waitErr   error
	stdoutErr error
}

func (f *fakeTransferFolder) exec(spec runner.ExecSpec) (*runner.ExecSession, error) {
	f.mu.Lock()
	f.execs++
	failCode, waitErr, stdoutErr := f.failCode, f.waitErr, f.stdoutErr
	f.mu.Unlock()
	script, args := spec.Argv[2], spec.Argv[4:]
	var out bytes.Buffer
	code := 0
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		in, _ := io.ReadAll(pr)
		f.mu.Lock()
		defer f.mu.Unlock()
		switch script {
		case desktopListScript:
			for name, b := range f.files {
				out.WriteString(strconv.Itoa(len(b)) + "/" + name + "\n")
			}
		case desktopUploadScript:
			if strconv.Itoa(len(in)) != args[1] {
				code = 4
				return
			}
			f.files[args[0]] = in
		case desktopDownloadScript:
			b, ok := f.files[args[0]]
			if !ok {
				code = 3
				return
			}
			out.Write(b)
		}
	}()
	stdout, ow := io.Pipe()
	go func() { <-done; _, _ = ow.Write(out.Bytes()); ow.Close() }()
	var read io.Reader = stdout
	if stdoutErr != nil {
		_ = stdout.Close()
		read = iotest.ErrReader(stdoutErr)
	}
	return &runner.ExecSession{
		Stdin: pw, Stdout: read, Stderr: strings.NewReader(""),
		Wait: func() (int, error) {
			<-done
			if failCode != 0 {
				return failCode, waitErr
			}
			return code, waitErr
		},
		Close: func() error { return nil },
	}, nil
}

type desktopFilesEnv struct {
	srv    *Server
	run    types.AgentRun
	ses    map[string]*http.Cookie
	audit  *sshTestRecorder
	folder *fakeTransferFolder
}

func newDesktopFilesEnv(t *testing.T, desktop *types.DesktopControls, apps ...types.UIApp) *desktopFilesEnv {
	t.Helper()
	srv, run, rec, ses := entryServer(t, false)
	if apps == nil {
		apps = []types.UIApp{{Name: "novnc", Port: 6080}}
	}
	srv.cfg.Store = &envelopeStore{Store: srv.cfg.Store, spec: types.RunPolicySpec{MinConfinementClass: types.CC2, UIApps: apps, Desktop: desktop}}
	folder := &fakeTransferFolder{files: map[string][]byte{"agent-report.csv": []byte("agent,wrote,this\n")}}
	srv.cfg.Runner = &sshFakeRunner{execFn: folder.exec}
	return &desktopFilesEnv{srv: srv, run: run, ses: ses, audit: rec, folder: folder}
}

func (e *desktopFilesEnv) path(name string) string {
	p := "/api/v1/runs/" + e.run.ID.String() + "/desktop/files"
	if name != "" {
		p += "/" + name
	}
	return p
}

func (e *desktopFilesEnv) transferRows(direction string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range e.audit.snapshot() {
		if ev.Action == "desktop.file.transfer" && strings.Contains(string(ev.Data), `"direction":"`+direction+`"`) {
			out = append(out, ev)
		}
	}
	return out
}

const fileSentinel = "SENTINEL-file-body-5e21"

// The defaults: an upload lands in the transfer folder and is audited by size and hash only; the
// folder is not listed and a download is refused, audited too, because download is blocked.
func TestDesktopFiles_DefaultsUploadAllowedDownloadBlocked(t *testing.T) {
	e := newDesktopFilesEnv(t, nil)
	w := doSSO(t, e.srv, http.MethodPut, e.path("notes.csv"), e.ses["owner"], fileSentinel)
	if w.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	if got := string(e.folder.files["notes.csv"]); got != fileSentinel {
		t.Fatalf("the desktop got %q", got)
	}
	rows := e.transferRows("upload")
	if len(rows) != 1 || rows[0].Outcome != "success" || !strings.Contains(string(rows[0].Data), `"sha256":"`+digest(fileSentinel)+`"`) {
		t.Fatalf("upload audit rows = %+v", rows)
	}
	w = doSSO(t, e.srv, http.MethodGet, e.path(""), e.ses["owner"], "")
	var view desktopFilesView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	if len(view.Files) != 0 || view.Rules != (*types.DesktopControls)(nil).Effective() || view.Folder != "~/Transfers" {
		t.Fatalf("a blocked download must list nothing and show the defaults: %+v", view)
	}
	w = doSSO(t, e.srv, http.MethodGet, e.path("agent-report.csv"), e.ses["owner"], "")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), reasonDesktopTransferBlocked) || strings.Contains(w.Body.String(), "agent,wrote") {
		t.Fatalf("download under the defaults: %d %s", w.Code, w.Body.String())
	}
	if rows := e.transferRows("download"); len(rows) != 1 || rows[0].Outcome != "denied" {
		t.Fatalf("a refused download is audited: %+v", rows)
	}
	for _, ev := range e.audit.snapshot() {
		if bytes.Contains(ev.Data, []byte(fileSentinel)) || bytes.Contains(ev.Data, []byte("notes.csv")) || strings.Contains(ev.Target, "notes.csv") {
			t.Fatalf("a file's content or name reached an audit row: %s %s", ev.Action, ev.Data)
		}
	}
}

// A policy that opens download lists the folder and streams a file, hashed into its audit row.
func TestDesktopFiles_DownloadFollowsThePolicy(t *testing.T) {
	e := newDesktopFilesEnv(t, &types.DesktopControls{Download: types.TransferAllowed, Upload: types.TransferBlocked})
	w := doSSO(t, e.srv, http.MethodGet, e.path(""), e.ses["owner"], "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `{"name":"agent-report.csv","bytes":17}`) {
		t.Fatalf("list with download allowed: %d %s", w.Code, w.Body.String())
	}
	w = doSSO(t, e.srv, http.MethodGet, e.path("agent-report.csv"), e.ses["owner"], "")
	if w.Code != http.StatusOK || w.Body.String() != "agent,wrote,this\n" || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("download: %d %q %v", w.Code, w.Body.String(), w.Header())
	}
	if rows := e.transferRows("download"); len(rows) != 1 || !strings.Contains(string(rows[0].Data), digest("agent,wrote,this\n")) {
		t.Fatalf("download audit rows = %+v", rows)
	}
	if w := doSSO(t, e.srv, http.MethodGet, e.path("missing.txt"), e.ses["owner"], ""); w.Code != http.StatusNotFound {
		t.Fatalf("a missing file: %d %s", w.Code, w.Body.String())
	}
	execs := e.folder.execs
	w = doSSO(t, e.srv, http.MethodPut, e.path("in.txt"), e.ses["owner"], "x")
	if w.Code != http.StatusForbidden || e.folder.execs != execs {
		t.Fatalf("a blocked upload must reach nothing in the sandbox: %d %s (%d execs)", w.Code, w.Body.String(), e.folder.execs-execs)
	}
}

// What is refused before the sandbox is reached: a name that is not plain, a file over the limit,
// a run with no desktop, and anyone but the person who may enter the run.
func TestDesktopFiles_RefusalsBeforeTheSandbox(t *testing.T) {
	e := newDesktopFilesEnv(t, nil)
	if w := doSSO(t, e.srv, http.MethodPut, e.path(".hidden"), e.ses["owner"], "x"); w.Code != http.StatusBadRequest {
		t.Fatalf("a dot name: %d %s", w.Code, w.Body.String())
	}
	r := httptest.NewRequest(http.MethodPut, e.path("big.iso"), strings.NewReader("x"))
	r.ContentLength = maxDesktopUploadBytes + 1
	r.AddCookie(e.ses["owner"])
	w := httptest.NewRecorder()
	panicFails(t, e.srv.Handler()).ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "Nothing was sent.") {
		t.Fatalf("an upload over the limit: %d %s", w.Code, w.Body.String())
	}
	if w := doSSO(t, e.srv, http.MethodGet, e.path(""), e.ses["admin"], ""); !refusedOwnerOnly(w.Code, w.Body.String()) {
		t.Fatalf("a super admin on a person's run: %d %s", w.Code, w.Body.String())
	}
	if w := doSSO(t, e.srv, http.MethodGet, e.path(""), e.ses["security"], ""); w.Code != http.StatusNotFound {
		t.Fatalf("the security tier: %d %s", w.Code, w.Body.String())
	}
	if e.folder.execs != 0 {
		t.Fatalf("%d execs reached the sandbox", e.folder.execs)
	}
	none := newDesktopFilesEnv(t, nil, []types.UIApp{}...)
	none.srv.cfg.Store.(*envelopeStore).spec.UIApps = nil
	if w := doSSO(t, none.srv, http.MethodGet, none.path(""), none.ses["owner"], ""); w.Code != http.StatusConflict ||
		!strings.Contains(w.Body.String(), reasonDesktopTransferUnavailable) {
		t.Fatalf("a run with no desktop: %d %s", w.Code, w.Body.String())
	}
}

// noSandboxRefStore answers the run with no sandbox_ref, as the seeded e2e run has.
type noSandboxRefStore struct{ store.Store }

func (s *noSandboxRefStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	run, err := s.Store.GetRun(ctx, id)
	run.SandboxRef = ""
	return run, err
}

// A RUNNING run with no sandbox_ref has no desktop to move files to: list, upload and download
// all answer 409 attach_not_running, and nothing reaches the sandbox.
func TestDesktopFiles_NoSandboxRefRefusesEveryEndpoint(t *testing.T) {
	e := newDesktopFilesEnv(t, nil)
	e.srv.cfg.Store = &noSandboxRefStore{Store: e.srv.cfg.Store}
	for _, c := range []struct{ method, name, body string }{
		{http.MethodGet, "", ""},
		{http.MethodPut, "notes.csv", "x"},
		{http.MethodGet, "agent-report.csv", ""},
	} {
		w := doSSO(t, e.srv, c.method, e.path(c.name), e.ses["owner"], c.body)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), reasonAttachNotRunning) {
			t.Fatalf("%s %q with no sandbox_ref: %d %s", c.method, c.name, w.Code, w.Body.String())
		}
	}
	if e.folder.execs != 0 {
		t.Fatalf("%d execs reached the sandbox", e.folder.execs)
	}
}

// sponsorFilesStore makes the env's run a service identity's, sponsored by sub-sponsor.
type sponsorFilesStore struct {
	*envelopeStore
	store.ServiceIdentityStore // never called: only GetServiceIdentity is asked
}

func (s *sponsorFilesStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	run, err := s.envelopeStore.GetRun(ctx, id)
	run.CreatedBy = types.ServicePrincipalPrefix + "ci-bot"
	return run, err
}

func (s *sponsorFilesStore) GetServiceIdentity(context.Context, string) (types.ServiceIdentity, error) {
	return types.ServiceIdentity{Name: "ci-bot", Sponsor: "sub-sponsor"}, nil
}

// A service identity's sponsor opens its terminal and apps, but moves no files: every Files
// endpoint refuses the sponsor before the sandbox is reached.
func TestDesktopFiles_SponsorIsRefusedOnEveryEndpoint(t *testing.T) {
	e := newDesktopFilesEnv(t, &types.DesktopControls{Upload: types.TransferAllowed, Download: types.TransferAllowed})
	e.srv.cfg.Store = &sponsorFilesStore{envelopeStore: e.srv.cfg.Store.(*envelopeStore)}
	sponsor := ssoSession(t, "sub-sponsor", "sponsor@corp.example", oidc.RoleUser)
	for _, c := range []struct{ method, name, body string }{
		{http.MethodGet, "", ""}, {http.MethodPut, "in.txt", "x"}, {http.MethodGet, "agent-report.csv", ""},
	} {
		if w := doSSO(t, e.srv, c.method, e.path(c.name), sponsor, c.body); w.Code != http.StatusNotFound {
			t.Errorf("the sponsor on %s %s: %d %s, want 404", c.method, e.path(c.name), w.Code, w.Body.String())
		}
	}
	if e.folder.execs != 0 || len(e.transferRows("upload"))+len(e.transferRows("download")) != 0 {
		t.Fatalf("a sponsor's request reached the sandbox (%d execs) or wrote transfer rows", e.folder.execs)
	}
}

// backgroundFilesStore makes the env's run a background-only one.
type backgroundFilesStore struct{ *envelopeStore }

func (s *backgroundFilesStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	run, err := s.envelopeStore.GetRun(ctx, id)
	run.Experience = types.ExperienceBackground
	return run, err
}

// Only the person who may enter the desktop reaches its files, on every Files endpoint: another
// person and the security tier get a missing run's 404, a super admin on a person's run the 403,
// and the owner of a background-only run the desktop door's 409. Nothing reaches the sandbox.
func TestDesktopFiles_OnlyWhoMayEnterTheDesktopReachesIt(t *testing.T) {
	allowed := &types.DesktopControls{Upload: types.TransferAllowed, Download: types.TransferAllowed}
	e := newDesktopFilesEnv(t, allowed)
	bg := newDesktopFilesEnv(t, allowed)
	bg.srv.cfg.Store = &backgroundFilesStore{bg.srv.cfg.Store.(*envelopeStore)}
	notFound := func(w *httptest.ResponseRecorder) bool { return w.Code == http.StatusNotFound }
	for _, c := range []struct {
		who  string
		env  *desktopFilesEnv
		ses  *http.Cookie
		want func(*httptest.ResponseRecorder) bool
	}{
		{"another person", e, ssoSession(t, "sub-other", "other@corp.example", oidc.RoleUser), notFound},
		{"the security tier, who can view the run", e, e.ses["security"], notFound},
		{"a super admin on a person's run", e, e.ses["admin"], func(w *httptest.ResponseRecorder) bool { return refusedOwnerOnly(w.Code, w.Body.String()) }},
		{"the owner of a background-only run", bg, bg.ses["owner"], func(w *httptest.ResponseRecorder) bool {
			return w.Code == http.StatusConflict && reasonOf(t, w) == reasonRunBackgroundOnly
		}},
	} {
		for _, r := range []struct{ method, name, body string }{
			{http.MethodGet, "", ""}, {http.MethodPut, "in.txt", "x"}, {http.MethodGet, "agent-report.csv", ""},
		} {
			if w := doSSO(t, c.env.srv, r.method, c.env.path(r.name), c.ses, r.body); !c.want(w) {
				t.Errorf("%s on %s %s: %d %s", c.who, r.method, c.env.path(r.name), w.Code, w.Body.String())
			}
		}
	}
	for _, env := range []*desktopFilesEnv{e, bg} {
		if env.folder.execs != 0 || len(env.transferRows("upload"))+len(env.transferRows("download")) != 0 {
			t.Fatalf("a refused request reached the sandbox (%d execs) or wrote transfer rows", env.folder.execs)
		}
	}
}

// When the run's desktop rules cannot be read (the read fails, or the log holds no envelope), every
// Files endpoint refuses before the sandbox, whatever the rules would have said: never the defaults,
// which allow upload. Once the read answers again, the stored rules decide.
func TestDesktopFiles_UnreadableRulesRefuseEveryTransfer(t *testing.T) {
	refused := func(w *httptest.ResponseRecorder) bool {
		return w.Code == http.StatusServiceUnavailable && reasonOf(t, w) == reasonDesktopRulesUnreadable
	}
	for _, c := range []struct {
		name string
		fail func(*envelopeStore)
	}{
		{"the read fails", func(s *envelopeStore) { s.readErr = errors.New("audit store unavailable") }},
		{"the run has no envelope", func(s *envelopeStore) { s.noEnvelope = true }},
	} {
		for _, rule := range []types.TransferRule{types.TransferBlocked, types.TransferAllowed} {
			e := newDesktopFilesEnv(t, &types.DesktopControls{PasteIn: rule, Upload: rule, Download: rule})
			st := e.srv.cfg.Store.(*envelopeStore)
			c.fail(st)
			for _, r := range []struct{ method, name, body string }{
				{http.MethodGet, "", ""}, {http.MethodPut, "in.txt", fileSentinel}, {http.MethodGet, "agent-report.csv", ""},
			} {
				if w := doSSO(t, e.srv, r.method, e.path(r.name), e.ses["owner"], r.body); !refused(w) {
					t.Errorf("%s, upload %s: %s %s = %d %s, want 503 %s", c.name, rule, r.method, r.name, w.Code, w.Body.String(), reasonDesktopRulesUnreadable)
				}
			}
			if e.folder.execs != 0 || e.folder.files["in.txt"] != nil {
				t.Fatalf("%s, upload %s: %d execs reached the sandbox", c.name, rule, e.folder.execs)
			}
			st.readErr, st.noEnvelope = nil, false
			w := doSSO(t, e.srv, http.MethodPut, e.path("in.txt"), e.ses["owner"], fileSentinel)
			if rule == types.TransferBlocked && (w.Code != http.StatusForbidden || reasonOf(t, w) != reasonDesktopTransferBlocked) {
				t.Errorf("%s, recovered: the stored upload block must decide: %d %s", c.name, w.Code, w.Body.String())
			}
			if rule == types.TransferAllowed && (w.Code != http.StatusOK || string(e.folder.files["in.txt"]) != fileSentinel) {
				t.Errorf("%s, recovered: an allowed upload must land: %d %s", c.name, w.Code, w.Body.String())
			}
		}
	}
}

// One read decides a transfer: when the read that found the run's desktop is the last one that
// answers, the upload is still judged by the stored block, never by defaults a later failed read
// would fall back to. Nothing reaches the sandbox.
func TestDesktopFiles_OneReadDecidesTheTransfer(t *testing.T) {
	e := newDesktopFilesEnv(t, &types.DesktopControls{Upload: types.TransferBlocked})
	st := e.srv.cfg.Store.(*envelopeStore)
	st.answered = 1
	w := doSSO(t, e.srv, http.MethodPut, e.path("in.txt"), e.ses["owner"], fileSentinel)
	if w.Code != http.StatusForbidden || reasonOf(t, w) != reasonDesktopTransferBlocked || e.folder.execs != 0 {
		t.Fatalf("an upload the stored rules block: %d %s (%d execs), want 403 %s and no exec", w.Code, w.Body.String(), e.folder.execs, reasonDesktopTransferBlocked)
	}
}

// desktopDoorUnitStore gives the env's run the profile, and the machine kind, a row asks for.
type desktopDoorUnitStore struct {
	*envelopeStore
	unit types.GovernanceUnit
	kind types.MachineKind
}

func (s *desktopDoorUnitStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	run, err := s.envelopeStore.GetRun(ctx, id)
	run.UnitID, run.MachineKind = &s.unit.ID, s.kind
	return run, err
}

func (s *desktopDoorUnitStore) GetGovernanceUnitChain(_ context.Context, id uuid.UUID) ([]types.GovernanceUnit, error) {
	return unitChainOf([]types.GovernanceUnit{s.unit}, nil, id)
}

// The profile's deny_interactive closes the Files door as it closes the terminal and the desktop:
// the owner gets the governance refusal on all three routes, whatever the machine kind and even
// though the rules allow both directions. Nothing reaches the sandbox and no transfer is recorded.
func TestDesktopFiles_DenyInteractiveClosesEveryEndpoint(t *testing.T) {
	unit := types.GovernanceUnit{ID: uuid.New(), Name: "ci", Limits: types.GovernanceLimits{DenyInteractive: true}}
	allowed := &types.DesktopControls{Upload: types.TransferAllowed, Download: types.TransferAllowed}
	for _, kind := range []types.MachineKind{"", types.MachineMacOSVM} {
		e := newDesktopFilesEnv(t, allowed)
		e.srv.cfg.Store = &desktopDoorUnitStore{envelopeStore: e.srv.cfg.Store.(*envelopeStore), unit: unit, kind: kind}
		for _, r := range []struct{ method, name, body string }{
			{http.MethodGet, "", ""}, {http.MethodPut, "in.txt", "x"}, {http.MethodGet, "agent-report.csv", ""},
		} {
			w := doSSO(t, e.srv, r.method, e.path(r.name), e.ses["owner"], r.body)
			if w.Code != http.StatusForbidden || reasonOf(t, w) != string(authz.ReasonGovernanceUnit) {
				t.Errorf("machine kind %q, %s %q: %d %s, want 403 %s", kind, r.method, r.name, w.Code, w.Body.String(), authz.ReasonGovernanceUnit)
			}
		}
		if e.folder.execs != 0 || len(e.transferRows("upload"))+len(e.transferRows("download")) != 0 {
			t.Fatalf("machine kind %q: %d execs reached the sandbox, or a transfer row was written", kind, e.folder.execs)
		}
	}
}

// desktopDoorPausedStore answers the Files door's envelope read on a pauseStore, which has none.
type desktopDoorPausedStore struct {
	*pauseStore
	envelope *envelopeStore
}

func (s *desktopDoorPausedStore) QueryAuditEvents(ctx context.Context, runID uuid.UUID, n int) ([]types.AuditEvent, error) {
	return s.envelope.QueryAuditEvents(ctx, runID, n)
}

// desktopDoorPausedRunner refuses an exec into a paused sandbox, as the daemon does.
type desktopDoorPausedRunner struct {
	*flakyThaw
	st     *pauseStore
	folder *fakeTransferFolder
}

func (r *desktopDoorPausedRunner) ExecStream(_ context.Context, _ string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	if at, _ := r.st.paused(); at != nil {
		return nil, errors.New("synthetic exec into a paused sandbox")
	}
	return r.folder.exec(spec)
}

// Only a transfer the rules allow wakes a paused run. A list, however the rules read, a denied
// upload or download and a bad file name leave it paused with no presence and no run.resume row;
// a list that would have to read the folder answers the 409 the run page's other reads do.
func TestDesktopFiles_PausedRunThawsOnlyForATransfer(t *testing.T) {
	blocked := &types.DesktopControls{Upload: types.TransferBlocked, Download: types.TransferBlocked}
	allowed := &types.DesktopControls{Upload: types.TransferAllowed, Download: types.TransferAllowed}
	rig := func(t *testing.T, desktop *types.DesktopControls) (*pauseFixture, *flakyThaw, *fakeTransferFolder) {
		t.Helper()
		f, rn, _ := pausedRunClock(t, 0)
		env := &envelopeStore{spec: types.RunPolicySpec{MinConfinementClass: types.CC2, UIApps: []types.UIApp{{Name: "novnc", Port: 6080}}, Desktop: desktop}}
		f.srv.cfg.Store = &desktopDoorPausedStore{pauseStore: f.st, envelope: env}
		folder := &fakeTransferFolder{files: map[string][]byte{"agent-report.csv": []byte("agent,wrote,this\n")}}
		f.srv.cfg.Runner = &desktopDoorPausedRunner{flakyThaw: rn, st: f.st, folder: folder}
		return f, rn, folder
	}
	owner := ssoSession(t, pauseOwner, "owner@corp.example", oidc.RoleUser)
	files := func(f *pauseFixture, name string) string {
		p := "/api/v1/runs/" + f.run.ID.String() + "/desktop/files"
		if name != "" {
			p += "/" + name
		}
		return p
	}

	for _, c := range []struct {
		what         string
		desktop      *types.DesktopControls
		method, name string
		code         int
		reason       string
	}{
		{"a list that reads the folder", allowed, http.MethodGet, "", http.StatusConflict, reasonRunInspectPaused},
		{"a list that runs no exec", blocked, http.MethodGet, "", http.StatusOK, ""},
		{"a denied upload", blocked, http.MethodPut, "in.txt", http.StatusForbidden, reasonDesktopTransferBlocked},
		{"a denied download", blocked, http.MethodGet, "agent-report.csv", http.StatusForbidden, reasonDesktopTransferBlocked},
		{"an upload with a bad name", allowed, http.MethodPut, ".hidden", http.StatusBadRequest, reasonDesktopTransferBadRequest},
		{"a download with a bad name", allowed, http.MethodGet, ".hidden", http.StatusBadRequest, reasonDesktopTransferBadRequest},
	} {
		t.Run(c.what, func(t *testing.T) {
			f, rn, folder := rig(t, c.desktop)
			w := doSSO(t, f.srv, c.method, files(f, c.name), owner, "x")
			if w.Code != c.code || (c.reason != "" && reasonOf(t, w) != c.reason) {
				t.Fatalf("%d %s, want %d %s", w.Code, w.Body.String(), c.code, c.reason)
			}
			if at, _ := f.st.paused(); at == nil || rn.tries != 0 || f.st.stamps != 0 || folder.execs != 0 {
				t.Errorf("paused_at %v, %d thaws, %d presence stamps, %d execs; want it left paused and untouched", at, rn.tries, f.st.stamps, folder.execs)
			}
			if rows := f.rows("run.resume", "success"); len(rows) != 0 {
				t.Errorf("run.resume rows = %+v, want none", rows)
			}
		})
	}

	for _, c := range []struct {
		what         string
		method, name string
		body         string
	}{
		{"an allowed upload", http.MethodPut, "in.txt", fileSentinel},
		{"an allowed download", http.MethodGet, "agent-report.csv", ""},
	} {
		t.Run(c.what, func(t *testing.T) {
			f, rn, folder := rig(t, allowed)
			if w := doSSO(t, f.srv, c.method, files(f, c.name), owner, c.body); w.Code != http.StatusOK {
				t.Fatalf("%d %s, want 200", w.Code, w.Body.String())
			}
			if at, _ := f.st.paused(); at != nil || rn.tries != 1 || folder.execs != 1 {
				t.Fatalf("paused_at %v, %d thaws, %d execs; want it resumed once and the transfer sent", at, rn.tries, folder.execs)
			}
			rows := f.rows("run.resume", "success")
			if len(rows) != 1 || rows[0].Actor != pauseOwner || leaseAuditData(t, rows[0])["reason"] != "presence" {
				t.Fatalf("run.resume rows = %+v, want one for the owner with reason presence", rows)
			}
		})
	}

	// A thaw that fails sends nothing, and says so as the old door did.
	t.Run("a failed thaw", func(t *testing.T) {
		f, rn, folder := rig(t, allowed)
		rn.fail = -1
		w := doSSO(t, f.srv, http.MethodPut, files(f, "in.txt"), owner, "x")
		if w.Code != http.StatusBadGateway || reasonOf(t, w) != reasonDesktopTransferFailed || folder.execs != 0 {
			t.Fatalf("%d %s, %d execs; want 502 %s and nothing sent", w.Code, w.Body.String(), folder.execs, reasonDesktopTransferFailed)
		}
	})
}

// A name with one of ( ) ! * ' next to a space or a non-ASCII letter reaches the handler with Go's
// RawPath set, and chi then hands the segment back still percent-encoded. The upload, the download
// and the review request must all see the decoded name, and a decoded slash stays refused.
func TestDesktopFiles_PercentEncodedNamesAreDecodedOnce(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"report%20(1).pdf", "report (1).pdf"},
		{"caf%C3%A9%20(2).txt", "café (2).txt"},
		{"report%2520(1).pdf", "report%20(1).pdf"}, // a real name with a literal percent sequence: decoded once, not twice
	} {
		e := newReviewEnv(t, types.TransferReview)
		if w := doSSO(t, e.srv, http.MethodPut, e.path(tc.raw), e.ses["owner"], fileSentinel); w.Code != http.StatusOK ||
			!strings.Contains(w.Body.String(), `"name":"`+tc.want+`"`) || string(e.folder.files[tc.want]) != fileSentinel {
			t.Fatalf("upload %s: %d %s (folder has %v)", tc.raw, w.Code, w.Body.String(), e.folder.files)
		}
		if w := doSSO(t, e.srv, http.MethodPost, e.path(tc.raw)+"/review", e.ses["owner"], ""); w.Code != http.StatusAccepted {
			t.Fatalf("review %s: %d %s", tc.raw, w.Code, w.Body.String())
		}
		d := newDesktopFilesEnv(t, &types.DesktopControls{Download: types.TransferAllowed})
		d.folder.files[tc.want] = []byte(fileSentinel)
		if w := doSSO(t, d.srv, http.MethodGet, d.path(tc.raw), d.ses["owner"], ""); w.Code != http.StatusOK || w.Body.String() != fileSentinel {
			t.Fatalf("download %s: %d %q", tc.raw, w.Code, w.Body.String())
		}
	}
	e := newDesktopFilesEnv(t, nil)
	if w := doSSO(t, e.srv, http.MethodPut, e.path("a%2Fb(1)"), e.ses["owner"], "x"); w.Code != http.StatusBadRequest || len(e.folder.files) != 1 {
		t.Fatalf("an encoded slash: %d %s (folder has %v)", w.Code, w.Body.String(), e.folder.files)
	}
}

// writeShim puts an executable script named name in dir, for a PATH that runs it instead of the real tool.
func writeShim(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// The upload script on a real shell: a write that fails part way, a rename that fails and a short body each
// exit 4 with no dot file and nothing under the name, and a whole upload lands with no dot file. A folder that
// can no longer be searched cannot show the dot file is gone, so that failure exits 1, never 4. Catches a failure
// that exits before removing the dot file, or reports 4 while the dot file may still be there.
func TestDesktopUploadScript_EveryFailureRemovesItsDotFileBeforeExit4(t *testing.T) {
	for _, tc := range []struct {
		name, shim, body, shimBody string
		size, want                 int
	}{
		{name: "a write that fails part way", shim: "cat", shimBody: "dd bs=1 count=3 2>/dev/null; exit 1", body: "abcdef", size: 6, want: 4},
		{name: "a folder that cannot be searched", shim: "cat", shimBody: `dd bs=1 count=3 2>/dev/null; chmod 000 "$HOME/Transfers"; exit 1`, body: "abcdef", size: 6, want: 1},
		{name: "a rename that fails", shim: "mv", shimBody: "exit 1", body: "abcdef", size: 6, want: 4},
		{name: "a short body", body: "abc", size: 6, want: 4},
		{name: "a whole upload", body: "abcdef", size: 6, want: 0},
	} {
		if tc.want == 1 && os.Geteuid() == 0 {
			continue // root searches any folder, so the unsearchable case cannot happen
		}
		home, shims := t.TempDir(), t.TempDir()
		folder := filepath.Join(home, "Transfers")
		t.Cleanup(func() { _ = os.Chmod(folder, 0o755) }) // before TempDir's removal
		if tc.shim != "" {
			writeShim(t, shims, tc.shim, tc.shimBody)
		}
		cmd := exec.Command("/bin/sh", "-c", desktopUploadScript, "sh", "in.txt", strconv.Itoa(tc.size))
		cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+shims+string(os.PathListSeparator)+os.Getenv("PATH"))
		cmd.Stdin = strings.NewReader(tc.body)
		code := 0
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("%s: %v", tc.name, err)
			}
			code = ee.ExitCode()
		}
		_ = os.Chmod(folder, 0o755)
		left, _ := filepath.Glob(filepath.Join(folder, ".wardyn-upload-*"))
		got, readErr := os.ReadFile(filepath.Join(folder, "in.txt"))
		switch {
		case code != tc.want || (code == 4 || code == 0) && len(left) != 0:
			t.Errorf("%s: exit %d with dot files %v, want exit %d (none left on 0 or 4)", tc.name, code, left, tc.want)
		case tc.want == 0 && string(got) != tc.body:
			t.Errorf("%s: the file holds %q (%v), want %q", tc.name, got, readErr, tc.body)
		case tc.want != 0 && readErr == nil:
			t.Errorf("%s: a failed upload left %q under its name", tc.name, got)
		}
	}
}

// The console says "Nothing was kept" only on the script's own exit 4, which comes after the dot file is
// gone. Any other exit, or an exec stream lost before its exit status, says some bytes may remain.
func TestDesktopFiles_UploadSaysNothingWasKeptOnlyWhenTheScriptConfirmedIt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failCode int
		waitErr  error
		kept     bool
	}{
		{name: "exit 4", failCode: 4},
		{name: "exit 1", failCode: 1, kept: true},
		{name: "a lost exit status", waitErr: errors.New("exec stream lost"), kept: true},
	} {
		e := newDesktopFilesEnv(t, nil)
		e.folder.failCode, e.folder.waitErr = tc.failCode, tc.waitErr
		w := doSSO(t, e.srv, http.MethodPut, e.path("notes.csv"), e.ses["owner"], fileSentinel)
		body := w.Body.String()
		if w.Code != http.StatusBadGateway || reasonOf(t, w) != reasonDesktopTransferFailed {
			t.Fatalf("%s: %d %s, want 502 %s", tc.name, w.Code, body, reasonDesktopTransferFailed)
		}
		if tc.kept != !strings.Contains(body, "Nothing was kept") || tc.kept != strings.Contains(body, "some bytes may remain") {
			t.Errorf("%s: %s", tc.name, body)
		}
		if rows := e.transferRows("upload"); len(rows) != 1 || rows[0].Outcome != "failure" {
			t.Errorf("%s: upload audit rows = %+v", tc.name, rows)
		}
	}
}

// A listed name keeps a space at its end, so two names that differ only by it both list and each
// downloads its own bytes. Catches a listing that trims the line before it splits the size off.
func TestDesktopFiles_ListingKeepsASpaceAtTheEndOfAName(t *testing.T) {
	if got := parseDesktopListing("6/report \n5/report\n   7/ lead\n"); len(got) != 3 ||
		got[0] != (desktopFile{Name: "report ", Bytes: 6}) || got[1] != (desktopFile{Name: "report", Bytes: 5}) || got[2] != (desktopFile{Name: " lead", Bytes: 7}) {
		t.Fatalf("parseDesktopListing = %+v", got)
	}
	e := newDesktopFilesEnv(t, &types.DesktopControls{Download: types.TransferAllowed})
	e.folder.files = map[string][]byte{"report ": []byte("spaced"), "report": []byte("plain")}
	w := doSSO(t, e.srv, http.MethodGet, e.path(""), e.ses["owner"], "")
	var view desktopFilesView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || len(view.Files) != 2 {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	for _, f := range view.Files {
		w := doSSO(t, e.srv, http.MethodGet, e.path(url.PathEscape(f.Name)), e.ses["owner"], "")
		if w.Code != http.StatusOK || w.Body.String() != string(e.folder.files[f.Name]) {
			t.Errorf("download %q: %d %q", f.Name, w.Code, w.Body.String())
		}
	}
}

// downloadOverHTTP runs a download through a real listener, so the client sees whether the transfer ended
// cleanly or was cut off.
func downloadOverHTTP(t *testing.T, e *desktopFilesEnv, name string) (int, []byte, error) {
	t.Helper()
	ts := httptest.NewServer(panicFails(t, e.srv.Handler()))
	t.Cleanup(ts.Close)
	req, err := http.NewRequest(http.MethodGet, ts.URL+e.path(name), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(e.ses["owner"])
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("download request: %v", err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	return resp.StatusCode, body, readErr
}

// A download that fails after its first byte, by the script's exit or a lost exec stream, is cut off, so
// the client's read fails and cannot take the bytes so far for the whole file; a whole one ends cleanly.
func TestDesktopFiles_ADownloadThatFailsAfterItBeganIsCutOff(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failCode int
		waitErr  error
	}{
		{name: "a nonzero exit", failCode: 1},
		{name: "a lost exec stream", waitErr: errors.New("exec stream lost")},
		{name: "a whole file"},
	} {
		e := newDesktopFilesEnv(t, &types.DesktopControls{Download: types.TransferAllowed})
		e.folder.failCode, e.folder.waitErr = tc.failCode, tc.waitErr
		status, body, readErr := downloadOverHTTP(t, e, "agent-report.csv")
		rows := e.transferRows("download")
		if tc.failCode == 0 && tc.waitErr == nil {
			if status != http.StatusOK || readErr != nil || string(body) != "agent,wrote,this\n" || len(rows) != 1 || rows[0].Outcome != "success" {
				t.Errorf("%s: %d %q %v, rows %+v", tc.name, status, body, readErr, rows)
			}
			continue
		}
		if status != http.StatusOK || !errors.Is(readErr, io.ErrUnexpectedEOF) {
			t.Errorf("%s: status %d, read error %v after %d bytes, want the committed 200 and io.ErrUnexpectedEOF", tc.name, status, readErr, len(body))
		}
		if len(rows) != 1 || rows[0].Outcome != "failure" {
			t.Errorf("%s: download audit rows = %+v", tc.name, rows)
		}
	}
}

// stuckStdin is an exec's stdin whose write blocks until the session closes and then fails late. writing closes
// as the write starts, and done just before it returns.
type stuckStdin struct{ writing, closed, done chan struct{} }

func (s stuckStdin) Write([]byte) (int, error) {
	close(s.writing)
	<-s.closed
	time.Sleep(100 * time.Millisecond)
	close(s.done)
	return 0, errors.New("exec stream closed")
}

func (s stuckStdin) Close() error { return nil }

func newStuckStdin() stuckStdin {
	return stuckStdin{writing: make(chan struct{}), closed: make(chan struct{}), done: make(chan struct{})}
}

// stuckExec is an exec whose stdin is in: its exit status is lost once the stdin copy is writing, and closes
// counts its session's Close calls.
func stuckExec(in stuckStdin, closes *atomic.Int32) func(runner.ExecSpec) (*runner.ExecSession, error) {
	return func(runner.ExecSpec) (*runner.ExecSession, error) {
		return &runner.ExecSession{
			Stdin: in, Stdout: strings.NewReader(""), Stderr: strings.NewReader(""),
			Wait: func() (int, error) { <-in.writing; return 0, errors.New("exec stream lost") },
			Close: func() error {
				if closes.Add(1) == 1 {
					close(in.closed)
				}
				return nil
			},
		}, nil
	}
}

// desktopExec never returns while its stdin copy runs, a lost exit status included: the copy reads the caller's
// body (an upload hashes it as it goes), so it must have ended before the caller reads the hash. It closes the
// session once. Catches a return on a Wait error that leaves the copy running.
func TestDesktopExec_JoinsItsStdinCopyWhenTheExitStatusIsLost(t *testing.T) {
	e := newDesktopFilesEnv(t, nil)
	in := newStuckStdin()
	var closes atomic.Int32
	e.srv.cfg.Runner = &sshFakeRunner{execFn: stuckExec(in, &closes)}
	code, err := e.srv.desktopExec(context.Background(), e.run, desktopUploadScript, strings.NewReader("abcdef"), nil, "in.txt", "6")
	if code != -1 || err == nil {
		t.Fatalf("desktopExec = %d, %v; want -1 and the lost exit status", code, err)
	}
	select {
	case <-in.done:
	default:
		t.Fatal("desktopExec returned while its stdin copy was still running")
	}
	if n := closes.Load(); n != 1 {
		t.Errorf("the session was closed %d times, want once", n)
	}
}

// A panic while desktopExec reads the exec's output still closes the session.
func TestDesktopExec_ClosesItsSessionWhenItsReaderPanics(t *testing.T) {
	e := newDesktopFilesEnv(t, nil)
	var closes atomic.Int32
	e.srv.cfg.Runner = &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) {
		_, pw := io.Pipe()
		return &runner.ExecSession{
			Stdin: pw, Stdout: strings.NewReader("x"), Stderr: strings.NewReader(""),
			Wait:  func() (int, error) { return 0, nil },
			Close: func() error { closes.Add(1); return nil },
		}, nil
	}}
	func() {
		defer func() { _ = recover() }()
		_, _ = e.srv.desktopExec(context.Background(), e.run, desktopListScript, nil, func(io.Reader) error { panic("reader failed") }, "1")
	}()
	if n := closes.Load(); n != 1 {
		t.Fatalf("the session was closed %d times after a panic, want once", n)
	}
}

// uploadOverConn sends an upload on a raw HTTP/1.1 connection: the headers for a size-byte body, then only sent,
// so a browser that stops sending part way can be played. It fails unless a reply comes within 5 seconds.
func uploadOverConn(t *testing.T, e *desktopFilesEnv, size int, sent string) (*http.Response, string) {
	t.Helper()
	ts := httptest.NewServer(panicFails(t, e.srv.Handler()))
	t.Cleanup(ts.Close)
	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() }) // before ts.Close, which waits for the handler
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	c := e.ses["owner"]
	if _, err := fmt.Fprintf(conn, "PUT %s HTTP/1.1\r\nHost: %s\r\nCookie: %s=%s\r\nContent-Length: %d\r\n\r\n%s",
		e.path("notes.csv"), ts.Listener.Addr(), c.Name, c.Value, size, sent); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no reply within 5 seconds: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

// stalledExec is an exec that answers code and waitErr once the first sent bytes have reached its stdin, while
// the browser still owes the rest: the stdin copy is left waiting on the request body.
func stalledExec(code int, waitErr error) func(runner.ExecSpec) (*runner.ExecSession, error) {
	return func(runner.ExecSpec) (*runner.ExecSession, error) {
		pr, pw := io.Pipe()
		arrived := make(chan struct{})
		go func() {
			if _, err := io.ReadFull(pr, make([]byte, 1)); err == nil {
				close(arrived)
			}
			_, _ = io.Copy(io.Discard, pr)
		}()
		return &runner.ExecSession{
			Stdin: pw, Stdout: strings.NewReader(""), Stderr: strings.NewReader(""),
			Wait:  func() (int, error) { <-arrived; return code, waitErr },
			Close: func() error { return pr.Close() },
		}, nil
	}
}

// An upload that fails while the browser has stopped sending answers at once, with the outcome's sentence and its
// failure row, and closes the connection. So does one whose whole body was read while its exec was gone: stopping
// the read there leaves the connection unfit for another request. A whole upload keeps its connection. Catches a
// join that waits on the browser, a connection kept after a stopped read, and a stopped read on a whole upload.
func TestDesktopFiles_AnUploadThatFailsWhileTheBrowserStopsSendingAnswers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		fake       func(runner.ExecSpec) (*runner.ExecSession, error) // nil: the transfer folder
		size       int
		sent, says string
		status     int
		closes     bool
	}{
		{name: "a lost exit status", fake: stalledExec(0, errors.New("exec stream lost")), size: 1 << 20, sent: "0123456789",
			status: http.StatusBadGateway, says: "some bytes may remain", closes: true},
		{name: "exit 4", fake: stalledExec(4, nil), size: 1 << 20, sent: "0123456789",
			status: http.StatusBadGateway, says: "Nothing was kept", closes: true},
		{name: "a whole body stuck on a gone exec", fake: stuckExec(newStuckStdin(), new(atomic.Int32)), size: 6, sent: "abcdef",
			status: http.StatusBadGateway, says: "some bytes may remain", closes: true},
		{name: "a whole upload", size: 6, sent: "abcdef", status: http.StatusOK, says: `"sha256"`},
	} {
		e := newDesktopFilesEnv(t, nil)
		if tc.fake != nil {
			e.srv.cfg.Runner = &sshFakeRunner{execFn: tc.fake}
		}
		resp, body := uploadOverConn(t, e, tc.size, tc.sent)
		if resp.StatusCode != tc.status || !strings.Contains(body, tc.says) || resp.Close != tc.closes {
			t.Errorf("%s: %d %s (connection closes: %v), want %d with %q (connection closes: %v)", tc.name, resp.StatusCode, body, resp.Close, tc.status, tc.says, tc.closes)
		}
		want := "success"
		if tc.status != http.StatusOK {
			want = "failure"
		}
		if rows := e.transferRows("upload"); len(rows) != 1 || rows[0].Outcome != want {
			t.Errorf("%s: upload audit rows = %+v, want one %s", tc.name, rows, want)
		}
	}
}

// A download whose stream fails before its first byte is a failed download, never an empty file, while an empty
// file still downloads. Catches a first-read error taken for an empty file.
func TestDesktopFiles_ADownloadWhoseStreamFailsBeforeItsFirstByteFails(t *testing.T) {
	e := newDesktopFilesEnv(t, &types.DesktopControls{Download: types.TransferAllowed})
	e.folder.stdoutErr = errors.New("exec stream reset")
	w := doSSO(t, e.srv, http.MethodGet, e.path("agent-report.csv"), e.ses["owner"], "")
	if w.Code != http.StatusBadGateway || w.Header().Get("Content-Disposition") != "" {
		t.Errorf("failed stream: %d %v %s, want 502 with no attachment", w.Code, w.Header(), w.Body.String())
	}
	if rows := e.transferRows("download"); len(rows) != 1 || rows[0].Outcome != "failure" {
		t.Errorf("failed stream: download audit rows = %+v", rows)
	}
	e = newDesktopFilesEnv(t, &types.DesktopControls{Download: types.TransferAllowed})
	e.folder.files["empty.txt"] = nil
	w = doSSO(t, e.srv, http.MethodGet, e.path("empty.txt"), e.ses["owner"], "")
	if w.Code != http.StatusOK || w.Body.Len() != 0 || w.Header().Get("Content-Disposition") == "" {
		t.Errorf("empty file: %d %v %q, want an empty 200 attachment", w.Code, w.Header(), w.Body.String())
	}
	if rows := e.transferRows("download"); len(rows) != 1 || rows[0].Outcome != "success" {
		t.Errorf("empty file: download audit rows = %+v", rows)
	}
}
