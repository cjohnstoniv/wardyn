// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
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

// fakeTransferFolder plays the three in-sandbox scripts against a map.
type fakeTransferFolder struct {
	mu    sync.Mutex
	files map[string][]byte
	execs int
}

func (f *fakeTransferFolder) exec(spec runner.ExecSpec) (*runner.ExecSession, error) {
	f.mu.Lock()
	f.execs++
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
	return &runner.ExecSession{
		Stdin: pw, Stdout: stdout, Stderr: strings.NewReader(""),
		Wait:  func() (int, error) { <-done; return code, nil },
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
