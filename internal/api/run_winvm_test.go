// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/imageadmit"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

var winvmRef = "registry.example/windows/server@sha256:" + strings.Repeat("ab", 32)

// winvmStore is the matrix store plus one Windows licence pool with one registered image. checkErr is what
// WindowsLaunchCheck answers; checks records each (ref, subject) it was asked.
type winvmStore struct {
	*authzStore
	wmu      sync.Mutex
	pool     types.WindowsLicencePool
	images   []types.WindowsImage
	checkErr error
	checks   []string
	removed  []string
	holders  []types.WindowsLicenceHolder
}

var _ store.WindowsLicenceStore = (*winvmStore)(nil)

func (s *winvmStore) ListWindowsLicencePools(context.Context) ([]types.WindowsLicencePool, error) {
	return []types.WindowsLicencePool{s.pool}, nil
}

func (s *winvmStore) GetWindowsLicencePool(_ context.Context, id uuid.UUID) (types.WindowsLicencePool, error) {
	if id != s.pool.ID {
		return types.WindowsLicencePool{}, store.ErrNotFound
	}
	return s.pool, nil
}

func (s *winvmStore) CreateWindowsLicencePool(_ context.Context, p types.WindowsLicencePool) (types.WindowsLicencePool, error) {
	return p, nil
}

func (s *winvmStore) UpdateWindowsLicencePool(context.Context, uuid.UUID, int64, *string, *int) (types.WindowsLicencePool, error) {
	return s.pool, nil
}

func (s *winvmStore) DeleteWindowsLicencePool(context.Context, uuid.UUID) (types.WindowsLicencePool, error) {
	return s.pool, nil
}

func (s *winvmStore) ListWindowsLicenceHolders(context.Context, uuid.UUID) ([]types.WindowsLicenceHolder, error) {
	return s.holders, nil
}

func (s *winvmStore) AddWindowsLicenceHolder(_ context.Context, pool uuid.UUID, subject, by string) (types.WindowsLicenceHolder, error) {
	return types.WindowsLicenceHolder{ID: uuid.New(), PoolID: pool, Subject: subject, AddedBy: by}, nil
}

func (s *winvmStore) RemoveWindowsLicenceHolder(_ context.Context, pool uuid.UUID, subject string) (types.WindowsLicenceHolder, error) {
	s.wmu.Lock()
	s.removed = append(s.removed, subject)
	s.wmu.Unlock()
	return types.WindowsLicenceHolder{PoolID: pool, Subject: subject}, nil
}

func (s *winvmStore) ListWindowsLicenceLeases(context.Context, uuid.UUID) ([]types.WindowsLicenceLease, error) {
	return nil, nil
}

func (s *winvmStore) ListWindowsImages(context.Context) ([]types.WindowsImage, error) {
	return s.images, nil
}

func (s *winvmStore) GetWindowsImage(_ context.Context, id uuid.UUID) (types.WindowsImage, error) {
	for _, img := range s.images {
		if img.ID == id {
			return img, nil
		}
	}
	return types.WindowsImage{}, store.ErrNotFound
}

func (s *winvmStore) RegisterWindowsImage(_ context.Context, img types.WindowsImage) (types.WindowsImage, error) {
	return img, nil
}

func (s *winvmStore) UpdateWindowsImage(_ context.Context, id uuid.UUID, _ int64, _ *string, _ *uuid.UUID) (types.WindowsImage, error) {
	return s.GetWindowsImage(context.Background(), id)
}

func (s *winvmStore) RetireWindowsImage(_ context.Context, id uuid.UUID) (types.WindowsImage, error) {
	return s.GetWindowsImage(context.Background(), id)
}

func (s *winvmStore) WindowsLaunchCheck(_ context.Context, ref, subject string) (store.WindowsLaunch, error) {
	s.wmu.Lock()
	s.checks = append(s.checks, ref+"|"+subject)
	s.wmu.Unlock()
	if s.checkErr != nil {
		return store.WindowsLaunch{Pool: s.pool}, s.checkErr
	}
	for _, img := range s.images {
		if img.Ref == ref {
			return store.WindowsLaunch{Image: img, Pool: s.pool, Holder: types.WindowsLicenceHolder{ID: uuid.New(), PoolID: s.pool.ID, Subject: subject}}, nil
		}
	}
	return store.WindowsLaunch{}, store.ErrWindowsImageUnregistered
}

func (s *winvmStore) DeleteWindowsLicenceHoldersOf(context.Context, []string) (int64, error) {
	return 0, nil
}

// winvmServer is a server whose store keeps one pool, Engineering, with one active image, Server, and has
// features.windows_vm on when on is true.
func winvmServer(t *testing.T, on bool) (*Server, *winvmStore, *harness) {
	t.Helper()
	ast := newAuthzStore()
	ast.siteCfg = types.SiteConfig{FeatureSwitches: &types.SiteFeatureSwitches{WindowsVM: &types.WindowsVMFeature{Enabled: &on}}}
	pool := types.WindowsLicencePool{ID: uuid.New(), Name: "Engineering", Seats: 1, State: types.WindowsLicencePoolActive}
	ws := &winvmStore{authzStore: ast, pool: pool, images: []types.WindowsImage{
		{ID: uuid.New(), Name: "Server", Ref: winvmRef, LicencePoolID: pool.ID, State: types.WindowsImageActive},
		{ID: uuid.New(), Name: "Old", Ref: "registry.example/windows/old@sha256:" + strings.Repeat("cd", 32), LicencePoolID: pool.ID, State: "retired"},
	}}
	h := newHarness(t)
	cfg := baseTestConfig(h, ws)
	cfg.OIDC = &oidc.Authenticator{}
	return New(cfg), ws, h
}

// winvmReq is a Windows VM create of the image Server, with mutations applied.
func winvmReq(muts ...func(*createRunRequest)) createRunRequest {
	req := createRunRequest{MachineKind: string(types.MachineWindowsVM), WindowsImage: "Server"}
	for _, m := range muts {
		m(&req)
	}
	return req
}

func winvmHTTP(ctx context.Context) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx)
}

// wantWinRefusal fails unless rf is the refusal status/reason, naming field when field is not "".
func wantWinRefusal(t *testing.T, name string, rf *runRefusal, status int, reason, field string) {
	t.Helper()
	switch {
	case rf == nil:
		t.Errorf("%s: admitted, want %d %s", name, status, reason)
	case rf.status != status || rf.body.Reason != reason:
		t.Errorf("%s: %d %s (%q), want %d %s", name, rf.status, rf.body.Reason, rf.body.Error, status, reason)
	case field != "" && rf.body.Field != field:
		t.Errorf("%s: field %q, want %q", name, rf.body.Field, field)
	}
}

// A Windows VM run starts only from an active registered image, named in windows_image: an empty name, a raw
// image, an unknown name and a retired one are each refused, and the registered ref is what the seat check is
// asked about, for the signed-in person.
func TestWindowsVMNeedsARegisteredImage(t *testing.T) {
	srv, ws, _ := winvmServer(t, true)
	r := winvmHTTP(memberContext())
	for name, tc := range map[string]struct {
		req           createRunRequest
		status        int
		reason, field string
	}{
		"no image":           {winvmReq(func(q *createRunRequest) { q.WindowsImage = " " }), http.StatusBadRequest, reasonWindowsImageRequired, "windows_image"},
		"a raw image":        {winvmReq(func(q *createRunRequest) { q.Image = "ubuntu:24.04" }), http.StatusBadRequest, reasonWindowsImageRequired, "image"},
		"an allowed image":   {winvmReq(func(q *createRunRequest) { q.AllowedImage = "ghcr.io/acme/x" }), http.StatusBadRequest, reasonWindowsImageRequired, "allowed_image"},
		"a devcontainer":     {winvmReq(func(q *createRunRequest) { q.DevcontainerRepo = "acme/widgets" }), http.StatusBadRequest, reasonWindowsImageRequired, "devcontainer_repo"},
		"an unknown name":    {winvmReq(func(q *createRunRequest) { q.WindowsImage = "Desktop" }), http.StatusUnprocessableEntity, reasonWindowsImageUnregistered, "windows_image"},
		"a retired image":    {winvmReq(func(q *createRunRequest) { q.WindowsImage = "Old" }), http.StatusUnprocessableEntity, reasonWindowsImageUnregistered, "windows_image"},
		"a name in the case": {winvmReq(func(q *createRunRequest) { q.WindowsImage = "server" }), http.StatusUnprocessableEntity, reasonWindowsImageUnregistered, "windows_image"},
	} {
		_, rf := srv.windowsImageDecision(r, tc.req, types.RunPolicySpec{})
		wantWinRefusal(t, name, rf, tc.status, tc.reason, tc.field)
	}
	if len(ws.checks) != 0 {
		t.Fatalf("a refused image reached the seat check: %v", ws.checks)
	}
	launch, rf := srv.windowsImageDecision(r, winvmReq(), types.RunPolicySpec{})
	if rf != nil {
		t.Fatalf("the registered image was refused: %d %s", rf.status, rf.body.Error)
	}
	if launch.Image.Ref != winvmRef || launch.Pool.ID != ws.pool.ID {
		t.Errorf("launch = %+v, want the registered ref in Engineering", launch)
	}
	if want := []string{winvmRef + "|sub-admit-member"}; !slices.Equal(ws.checks, want) {
		t.Errorf("seat checks = %v, want %v", ws.checks, want)
	}
}

// A Windows VM run is desktop-only: every agent, terminal, folder, drive, credential and inspection feature
// is refused by name, and so is a caller who is not a signed-in person (a token, a service, a schedule).
func TestWindowsVMRefusesEveryAgentFeature(t *testing.T) {
	srv, ws, _ := winvmServer(t, true)
	r := winvmHTTP(memberContext())
	ws1 := uuid.New()
	for name, tc := range map[string]struct {
		mut   func(*createRunRequest)
		spec  types.RunPolicySpec
		field string
	}{
		"a background run":  {mut: func(q *createRunRequest) { q.Experience = client.ExperienceBackground }, field: "experience"},
		"a task":            {mut: func(q *createRunRequest) { q.Task = "fix it" }, field: "experience"},
		"an agent":          {mut: func(q *createRunRequest) { q.Agent = "claude-code" }, field: "agent"},
		"tools":             {mut: func(q *createRunRequest) { q.Tools = []client.IncludedTool{{}} }, field: "agent"},
		"an agent start":    {mut: func(q *createRunRequest) { q.InteractiveStart = "agent" }, field: "interactive_start"},
		"a repo":            {mut: func(q *createRunRequest) { q.Repo = "acme/widgets" }, field: "workspaces"},
		"a workspace":       {mut: func(q *createRunRequest) { q.WorkspaceID = &ws1 }, field: "workspaces"},
		"a drive":           {mut: func(q *createRunRequest) { q.Drives = []client.DriveAttachment{{}} }, field: "drives"},
		"a computer folder": {mut: func(q *createRunRequest) { q.LocalFolder = &types.RunLocalFolder{} }, field: "local_folder"},
		"a kept folder":     {mut: func(q *createRunRequest) { q.KeepWorkFolder = true }, field: "keep_work_folder"},
		"a kept machine":    {mut: func(q *createRunRequest) { q.Machine = &client.RunMachine{} }, field: "machine"},
		"a component":       {mut: func(q *createRunRequest) { q.Components = []client.ComponentRef{{}} }, field: "components"},
		"a model provider":  {mut: func(q *createRunRequest) { q.ModelProvider = "p1" }, field: "components"},
		"a grant":           {spec: types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{}}}, field: "eligible_grants"},
		"TLS inspection":    {spec: types.RunPolicySpec{LLMInspection: &types.LLMInspectionSpec{InterceptTLS: true}}, field: "intercept_tls"},
	} {
		req := winvmReq()
		if tc.mut != nil {
			tc.mut(&req)
		}
		_, rf := srv.windowsImageDecision(r, req, tc.spec)
		wantWinRefusal(t, name, rf, http.StatusForbidden, reasonWindowsVMFeatureRefused, tc.field)
	}
	for name, ctx := range map[string]context.Context{
		"the admin token": context.Background(),
		"a service":       withServiceIdentity(memberContext(), "builder", "sub-admit-member"),
	} {
		_, rf := srv.windowsImageDecision(winvmHTTP(ctx), winvmReq(), types.RunPolicySpec{})
		wantWinRefusal(t, name, rf, http.StatusForbidden, reasonWindowsVMFeatureRefused, "")
	}
	_, rf := srv.windowsImageDecision(r, winvmReq(func(q *createRunRequest) { q.StartAt = "2026-12-01T09:00:00Z" }), types.RunPolicySpec{})
	wantWinRefusal(t, "a scheduled start", rf, http.StatusForbidden, reasonWindowsVMFeatureRefused, "")
	if len(ws.checks) != 0 {
		t.Fatalf("a refused shape reached the seat check: %v", ws.checks)
	}
}

// A recording requirement refuses a Windows VM run with the requirement's sentence, and so does any level but
// off; off is admitted.
func TestWindowsVMRecordingRequiredRefuses(t *testing.T) {
	rf := windowsRecordingChoiceRefusal(types.RecordingChoice{Required: true, Policy: types.RecordingOff})
	wantWinRefusal(t, "required", rf, http.StatusForbidden, reasonWindowsVMFeatureRefused, "recording")
	if rf != nil && rf.body.Error != windowsRecordingMsg {
		t.Errorf("required: %q, want %q", rf.body.Error, windowsRecordingMsg)
	}
	for _, level := range []types.RecordingLevel{"", types.RecordingTerminal} {
		rf := windowsRecordingChoiceRefusal(types.RecordingChoice{Policy: level})
		wantWinRefusal(t, "level "+string(level), rf, http.StatusForbidden, reasonWindowsVMFeatureRefused, "recording")
	}
	if rf := windowsRecordingChoiceRefusal(types.RecordingChoice{Policy: types.RecordingOff}); rf != nil {
		t.Errorf("recording off was refused: %s", rf.body.Error)
	}
}

// A Windows VM runs only on an organisation's registered runner that reports Windows VM caps: no runner, the
// deployment's executor, a person's runner, a runner with no Windows caps are each refused.
func TestWindowsVMPlacementRefusals(t *testing.T) {
	srv := capsServer(&fakeRunner{})
	winCaps := laptopCaps(types.CC1)
	winCaps.WindowsVM = &runnerwire.VMCaps{MachineType: "pc-q35-10.0", CPUModel: "host"}
	org := installRunnerOf(t, srv, "org-owner", true, winCaps)
	person := installRunnerOf(t, srv, "ana", true, winCaps)
	noCaps := installRunnerOf(t, srv, "org-owner", true, laptopCaps(types.CC1))
	rows := srv.cfg.Store.(ownedRunners).rows
	for _, id := range []uuid.UUID{org, noCaps} {
		rn := rows[id]
		rn.OwnerKind = types.RunnerOwnerOrg
		rows[id] = rn
	}
	type placed struct {
		place runPlacement
		req   createRunRequest
	}
	fold := func(place runPlacement, req createRunRequest) placed { return placed{place, req} }
	for name, tc := range map[string]struct {
		f    placed
		want bool // admitted
	}{
		"no runner":            {f: fold(runPlacement{}, winvmReq())},
		"a remote pool":        {f: fold(runPlacement{pooled: true, placement: types.PlacementRemote, eligible: []poolTarget{{executor: "k8s"}}}, winvmReq())},
		"a person's runner":    {f: fold(runPlacement{pooled: true, placement: types.PlacementLocal, eligible: []poolTarget{{runnerID: &person}}}, winvmReq())},
		"no Windows caps":      {f: fold(runPlacement{pooled: true, placement: types.PlacementLocal, eligible: []poolTarget{{runnerID: &noCaps}}}, winvmReq())},
		"one bad among good":   {f: fold(runPlacement{pooled: true, placement: types.PlacementLocal, eligible: []poolTarget{{runnerID: &org}, {runnerID: &noCaps}}}, winvmReq())},
		"the org runner":       {f: fold(runPlacement{pooled: true, placement: types.PlacementLocal, eligible: []poolTarget{{runnerID: &org}}}, winvmReq()), want: true},
		"named, not pooled":    {f: fold(runPlacement{}, winvmReq(func(q *createRunRequest) { q.Placement = types.PlacementLocal; q.RunnerID = org.String() })), want: true},
		"a person's, unpooled": {f: fold(runPlacement{}, winvmReq(func(q *createRunRequest) { q.Placement = types.PlacementLocal; q.RunnerID = person.String() }))},
	} {
		rf := srv.windowsPlacementRefusal(winvmHTTP(memberContext()), tc.f.req, types.RunPolicySpec{}, tc.f.place, governanceCeiling{}, true)
		if tc.want {
			if rf != nil {
				t.Errorf("%s: refused %d %s (%s)", name, rf.status, rf.body.Reason, rf.body.Error)
			}
			continue
		}
		wantWinRefusal(t, name, rf, http.StatusUnprocessableEntity, reasonWindowsVMRunnerUnsupported, "")
	}
}

// Web apps beside the desktop are refused by name.
func TestWindowsVMRefusesUIApps(t *testing.T) {
	srv := capsServer(&fakeRunner{})
	spec := types.RunPolicySpec{UIApps: []types.UIApp{{}}}
	rf := srv.windowsPlacementRefusal(winvmHTTP(memberContext()), winvmReq(), spec, runPlacement{}, governanceCeiling{}, true)
	wantWinRefusal(t, "ui_apps", rf, http.StatusForbidden, reasonWindowsVMFeatureRefused, "ui_apps")
}

// While features.windows_vm is off, a Windows VM launch is refused 403 windows_vms_off; the licence routes
// answer reads and removals and refuse adds. A store that keeps no pools answers 501.
func TestWindowsVMsOff(t *testing.T) {
	srv, ws, _ := winvmServer(t, false)
	_, rf := srv.windowsImageDecision(winvmHTTP(memberContext()), winvmReq(), types.RunPolicySpec{})
	wantWinRefusal(t, "launch", rf, http.StatusForbidden, reasonWindowsVMsOff, "")
	pool := "/api/v1/windows-licence-pools/" + ws.pool.ID.String()
	img := "/api/v1/windows-images/" + ws.images[0].ID.String()
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodGet, "/api/v1/windows-licence-pools", "", http.StatusOK},
		{http.MethodPost, "/api/v1/windows-licence-pools", `{"name":"Eng","seats":2}`, http.StatusForbidden},
		{http.MethodPost, pool + "/holders", `{"subject":"ana@example.com"}`, http.StatusForbidden},
		{http.MethodPost, "/api/v1/windows-images", `{"name":"S2","ref":"` + winvmRef + `","licence_pool_id":"` + ws.pool.ID.String() + `"}`, http.StatusForbidden},
		{http.MethodDelete, pool + "/holders/ana@example.com", "", http.StatusOK},
		{http.MethodDelete, img, "", http.StatusOK},
		{http.MethodDelete, pool, "", http.StatusOK},
		{http.MethodGet, pool + "/seats", "", http.StatusOK},
	} {
		w := do(t, srv, tc.method, tc.path, adminToken, tc.body)
		if w.Code != tc.status {
			t.Errorf("%s %s = %d %s, want %d", tc.method, tc.path, w.Code, w.Body, tc.status)
		}
		if tc.status == http.StatusForbidden && errorReasonOf(w.Body.String()) != reasonWindowsVMsOff {
			t.Errorf("%s %s: reason %q, want %s", tc.method, tc.path, errorReasonOf(w.Body.String()), reasonWindowsVMsOff)
		}
	}
	bare := New(baseTestConfig(newHarness(t), newAuthzStore()))
	if w := do(t, bare, http.MethodGet, "/api/v1/windows-licence-pools", adminToken, ""); w.Code != http.StatusNotImplemented ||
		errorReasonOf(w.Body.String()) != reasonWindowsLicencesUnavailable {
		t.Errorf("a store with no pools: %d %s, want 501 %s", w.Code, w.Body, reasonWindowsLicencesUnavailable)
	}
}

// A launched run's run.create row names the registered image, its digest and the licence pool.
func TestWindowsRunCreateAudit(t *testing.T) {
	pool := types.WindowsLicencePool{ID: uuid.New(), Name: "Engineering"}
	data := map[string]any{}
	stampWindowsLicence(data, &store.WindowsLaunch{Image: types.WindowsImage{Name: "Server", Ref: winvmRef}, Pool: pool})
	want := map[string]any{"windows_image": "Server", "windows_image_digest": "sha256:" + strings.Repeat("ab", 32),
		"windows_licence_pool_id": pool.ID.String(), "windows_licence_pool": "Engineering"}
	for k, v := range want {
		if data[k] != v {
			t.Errorf("%s = %v, want %v", k, data[k], v)
		}
	}
	other := map[string]any{}
	stampWindowsLicence(other, nil)
	if len(other) != 0 {
		t.Errorf("a run with no Windows launch stamped %v", other)
	}
}

// A refused launch writes a run.create denied row with the reason; a non-holder is 403
// windows_licence_not_holder and a holder whose seat is held is 409 windows_licence_in_use naming the run.
func TestWindowsRefusalAuditedDenied(t *testing.T) {
	held := uuid.New()
	for name, tc := range map[string]struct {
		err    error
		status int
		reason string
		says   string
	}{
		"not a holder": {store.ErrWindowsLicenceNotHolder, http.StatusForbidden, reasonWindowsLicenceNotHolder, "You do not hold a seat in the Windows licence pool Engineering. Ask an admin to add you."},
		"seat in use":  {&store.WindowsLicenceInUseError{RunID: held}, http.StatusConflict, reasonWindowsLicenceInUse, "Your seat in the Windows licence pool Engineering is in use by run " + held.String() + ". End that run first."},
		"pool full":    {store.ErrWindowsLicencePoolFull, http.StatusConflict, reasonWindowsLicencePoolFull, "Engineering"},
	} {
		t.Run(name, func(t *testing.T) {
			srv, ws, h := winvmServer(t, true)
			ws.checkErr = tc.err
			req := winvmReq()
			rec := httptest.NewRecorder()
			f := &runFold{mode: foldCreate, w: rec, r: winvmHTTP(memberContext()), req: &req}
			if srv.stepWindowsImage(f, &foldRecorder{}) {
				t.Fatal("the launch was admitted")
			}
			if rec.Code != tc.status || errorReasonOf(rec.Body.String()) != tc.reason || !strings.Contains(rec.Body.String(), tc.says) {
				t.Fatalf("answered %d %s, want %d %s saying %q", rec.Code, rec.Body, tc.status, tc.reason, tc.says)
			}
			var rows []types.AuditEvent
			for _, ev := range h.audit.snapshot() {
				if ev.Action == "run.create" && ev.Outcome == "denied" {
					rows = append(rows, ev)
				}
			}
			if len(rows) != 1 {
				t.Fatalf("run.create denied rows = %d, want 1", len(rows))
			}
			var data map[string]any
			if err := json.Unmarshal(rows[0].Data, &data); err != nil {
				t.Fatal(err)
			}
			if data["reason"] != tc.reason || data["windows_image"] != "Server" || data["machine_kind"] != "windows_vm" || data["subject"] != "sub-admit-member" {
				t.Errorf("denied row data = %v", data)
			}
		})
	}
	// A preview refuses the same way and writes no row.
	srv, ws, h := winvmServer(t, true)
	ws.checkErr = store.ErrWindowsLicenceNotHolder
	req := winvmReq()
	f := &runFold{mode: foldPreflight, w: httptest.NewRecorder(), r: winvmHTTP(memberContext()), req: &req}
	if srv.stepWindowsImage(f, &foldRecorder{}) {
		t.Fatal("the preflight was admitted")
	}
	for _, ev := range h.audit.snapshot() {
		if ev.Action == "run.create" {
			t.Errorf("a preflight wrote %s %s", ev.Action, ev.Outcome)
		}
	}
}

// winvmExecHarness is newExecHarness's run made a Windows VM run.
func winvmExecHarness(t *testing.T) *execHarness {
	t.Helper()
	e := newExecHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) { return kvExecSession(""), nil })
	e.ast.mu.Lock()
	run := e.ast.runs[e.id]
	run.MachineKind, run.Interactive = types.MachineWindowsVM, true
	e.ast.runs[e.id] = run
	e.ast.mu.Unlock()
	return e
}

func (e *execHarness) ownerDo(t *testing.T, method, suffix, body string) *httptest.ResponseRecorder {
	t.Helper()
	owner := ssoSession(t, execOwner, "owner@corp.example", oidc.RoleUser)
	return doSSO(t, e.srv, method, "/api/v1/runs/"+e.id.String()+suffix, owner, body)
}

// Every door but the desktop's refuses a live Windows VM run with its own sentence, before any exec reaches
// the sandbox: exec, resource usage, the file listing, SSH and the terminal.
func TestWindowsVMDoorsRefused(t *testing.T) {
	e := winvmExecHarness(t)
	for _, tc := range []struct{ method, suffix, body, door string }{
		{http.MethodPost, "/exec", `{"command":"id"}`, "exec"},
		{http.MethodGet, "/resources", "", "resource usage"},
		{http.MethodGet, "/files", "", "file listing"},
	} {
		w := e.ownerDo(t, tc.method, tc.suffix, tc.body)
		if w.Code != http.StatusConflict || errorReasonOf(w.Body.String()) != reasonWindowsVMFeatureRefused ||
			!strings.Contains(w.Body.String(), windowsDeskOnlyMsg(tc.door)) {
			t.Errorf("%s %s = %d %s, want 409 %s", tc.method, tc.suffix, w.Code, w.Body, reasonWindowsVMFeatureRefused)
		}
	}
	if n := e.execs.Load(); n != 0 {
		t.Errorf("%d execs reached the sandbox", n)
	}
	run := types.AgentRun{ID: e.id, MachineKind: types.MachineWindowsVM, Interactive: true}
	for _, door := range []string{"SSH access", "interactive exec", "a terminal", "a later door"} {
		rf := backgroundOnlyRefusal(run, door)
		wantWinRefusal(t, door, rf, http.StatusConflict, reasonWindowsVMFeatureRefused, "")
		if rf != nil && !strings.HasPrefix(sshDoorRefusalErr(rf).Error(), "ssh: A Windows VM run is desktop-only") {
			t.Errorf("%s: ssh error %q", door, sshDoorRefusalErr(rf))
		}
	}
}

// Revive is refused on a Windows VM run before anything is read or launched.
func TestWindowsVMReviveRefused(t *testing.T) {
	e := winvmExecHarness(t)
	w := e.ownerDo(t, http.MethodPost, "/revive", `{}`)
	if w.Code != http.StatusConflict || errorReasonOf(w.Body.String()) != reasonWindowsVMFeatureRefused ||
		!strings.Contains(w.Body.String(), windowsDeskOnlyMsg("revive")) {
		t.Errorf("revive = %d %s, want 409 %s", w.Code, w.Body, reasonWindowsVMFeatureRefused)
	}
}

// The desktop is a Windows VM run's one door: the web application gateway passes it, and a desktop-only
// request with a registered image and a free seat is admitted and carries the registered ref.
func TestWindowsVMDesktopAdmitted(t *testing.T) {
	run := types.AgentRun{MachineKind: types.MachineWindowsVM, Interactive: true}
	if rf := windowsVMDoorRefusal(run, "web application gateway"); rf != nil {
		t.Errorf("the desktop's door refused: %s", rf.body.Error)
	}
	if rf := backgroundOnlyRefusal(run, "web application gateway"); rf != nil {
		t.Errorf("the desktop's door refused: %s", rf.body.Error)
	}
	if rf := windowsVMDoorRefusal(types.AgentRun{MachineKind: types.MachineLinuxVM}, "exec"); rf != nil {
		t.Errorf("a Linux VM's exec door refused: %s", rf.body.Error)
	}
	srv, _, _ := winvmServer(t, true)
	req := winvmReq()
	rec := httptest.NewRecorder()
	f := &runFold{mode: foldCreate, w: rec, r: winvmHTTP(memberContext()), req: &req}
	if !srv.stepWindowsImage(f, &foldRecorder{}) {
		t.Fatalf("a desktop-only launch was refused: %d %s", rec.Code, rec.Body)
	}
	if f.windowsRef != winvmRef || f.windowsLaunch == nil || f.windowsImageInput("ignored") != "Server" {
		t.Errorf("fold = %q %+v, want the registered ref and the image's name as input", f.windowsRef, f.windowsLaunch)
	}
}

// A support session cannot enter a Windows VM run: a helper would take its one desktop from its owner.
func TestSupportRefusedOnWindowsVM(t *testing.T) {
	srv := &Server{}
	run := types.AgentRun{State: types.RunRunning, MachineKind: types.MachineWindowsVM, Interactive: true}
	if got := srv.supportRunIneligible(run); got != supportIneligibleWindowsVM {
		t.Fatalf("supportRunIneligible = %q, want %q", got, supportIneligibleWindowsVM)
	}
	if msg := supportIneligibleMsg(supportIneligibleWindowsVM); !strings.Contains(msg, "Windows VM") {
		t.Errorf("the refusal says %q, want it to name the Windows VM", msg)
	}
	run.MachineKind = types.MachineLinuxVM
	if got := srv.supportRunIneligible(run); got == supportIneligibleWindowsVM {
		t.Errorf("a Linux VM run read as a Windows VM")
	}
}

// Image admission asks nothing for a Windows VM run, whose image is the registered ref, and records
// not_applicable.
func TestWindowsVMImageAdmissionNotApplicable(t *testing.T) {
	a := &fakeAdmitter{require: func(bool, []string) (*imageadmit.Verdict, error) {
		return &imageadmit.Verdict{Reason: imageadmit.NoPolicy}, nil
	}}
	got := runAdmission(admissionServer(admissionSite(types.ImageAdmissionEnforce), a), memberCtx(nil), foldCreate, admitTagRef, nil,
		func(f *runFold) { f.req.MachineKind = string(types.MachineWindowsVM) })
	if r, ad := a.calls(); !got.ok || r != 0 || ad != 0 {
		t.Errorf("Windows VM: ok=%v requires=%d admits=%d", got.ok, r, ad)
	}
	if v := got.f.admission; v == nil || v.Verdict != types.ImageAdmissionNotApplicable || v.Mode != types.ImageAdmissionEnforce || v.Digest != "" {
		t.Errorf("Windows VM verdict %+v, want enforce not_applicable", v)
	}
	if types.MachineWindowsVM.RunsMemberImage() {
		t.Error("a Windows VM reads as running the member image")
	}
}

// A Windows VM's disk is a qcow2 overlay, sized as a Linux VM's: a written size is refused, a requested one
// is disclosed, and the container's disk check is not asked.
func TestWindowsVMDiskSizedAsOverlay(t *testing.T) {
	unlimited := types.EffectiveLimit{Cap: types.LimitAmount{Unlimited: true}, Default: types.LimitAmount{Unlimited: true}}
	for name, tc := range map[string]struct {
		res     types.ResourceLimits
		request int
		want    string
		warn    bool
	}{
		"a policy disk size": {res: types.ResourceLimits{DiskMiB: 4096}, want: reasonMainDriveUnenforceable},
		"a requested size":   {request: 2048, warn: true},
		"nothing":            {},
	} {
		t.Run(name, func(t *testing.T) {
			srv := &Server{cfg: Config{Runner: &fakeRunner{capsDisk: types.StorageEnforcementFilesystem}}}
			rec := httptest.NewRecorder()
			req := &createRunRequest{MachineKind: string(types.MachineWindowsVM)}
			if tc.request > 0 {
				req.Resources = &client.RunResources{DiskMiB: tc.request}
			}
			f := &runFold{mode: foldPreflight, w: rec, r: httptest.NewRequest(http.MethodPost, "/api/v1/runs/preflight", nil), req: req,
				spec: types.RunPolicySpec{Resources: &tc.res}, place: runPlacement{limits: types.EffectiveRunLimits{DiskMiB: unlimited}}}
			ok := srv.stepMainDrive(f, &foldRecorder{})
			if tc.want == "" {
				if !ok {
					t.Fatalf("stepMainDrive refused: %d %s", rec.Code, rec.Body)
				}
			} else if ok || rec.Code != http.StatusUnprocessableEntity || errorReasonOf(rec.Body.String()) != tc.want {
				t.Fatalf("stepMainDrive = %v, %d %s, want 422 %s", ok, rec.Code, rec.Body, tc.want)
			}
			if got := slices.Contains(f.policyWarns, warnLinuxVMDiskNotCapped); got != tc.warn {
				t.Errorf("disclosure = %v, want %v (warns %v)", got, tc.warn, f.policyWarns)
			}
		})
	}
}

// Neither the waiting nor the idle pause freezes a Windows VM run, a freeze would stop the VM under a person at
// its desktop; a Linux VM run in the same state is paused, the control.
func TestWindowsVMNotIdlePaused(t *testing.T) {
	for _, kind := range []types.MachineKind{types.MachineLinuxVM, types.MachineWindowsVM} {
		for _, branch := range []string{"waiting", "idle"} {
			f := newPauseFixture(t, time.Hour)
			if branch == "waiting" {
				f.st.open, f.st.waiting = true, true
			} else {
				f.st.run.RunLimits.PauseIdleAfterSec = 60
				f.rn.cpu = pct(2)
			}
			f.st.run.MachineKind = kind
			f.sweep(t)
			freezes, _ := f.rn.counts()
			pausedAt, _ := f.st.paused()
			if paused := freezes != 0 || pausedAt != nil; paused != (kind == types.MachineLinuxVM) {
				t.Errorf("%s, %s: freezes %d, paused at %v; want a pause only for the Linux VM control", kind, branch, freezes, pausedAt)
			}
		}
	}
}

// spyImageBuilder counts every build it is asked for.
type spyImageBuilder struct {
	fakeImageBuilder
	calls atomic.Int32
}

func (b *spyImageBuilder) BuildDevcontainer(ctx context.Context, repo, ref, tag string, w io.Writer) (BuiltImage, error) {
	b.calls.Add(1)
	return b.fakeImageBuilder.BuildDevcontainer(ctx, repo, ref, tag, w)
}

func (b *spyImageBuilder) BuildFromDevcontainerFiles(ctx context.Context, files map[string]string, tag string, w io.Writer) (BuiltImage, error) {
	b.calls.Add(1)
	return b.fakeImageBuilder.BuildFromDevcontainerFiles(ctx, files, tag, w)
}

func (b *spyImageBuilder) FinalizeBase(ctx context.Context, image, tag string, w io.Writer, force bool) (BuiltImage, error) {
	b.calls.Add(1)
	return b.fakeImageBuilder.FinalizeBase(ctx, image, tag, w, force)
}

// A Windows VM launch dispatches the registered ref as inserted: no build, wrap, admission or image write; a
// Linux run's launch is resolveCreateRunImage's, unchanged.
func TestWindowsVMLaunchDispatchesTheRegisteredRef(t *testing.T) {
	ast := newAuthzStore()
	srv := New(baseTestConfig(newHarness(t), ast))
	spy := &spyImageBuilder{}
	srv.cfg.ImageBuilder = spy
	ctx := context.Background()
	win := types.AgentRun{ID: uuid.New(), MachineKind: types.MachineWindowsVM, Image: winvmRef}
	image, arches, failed := srv.launchRunImage(ctx, createRunRequest{MachineKind: string(types.MachineWindowsVM), Image: "ubuntu:24.04"}, win, nil)
	if image != winvmRef || arches != nil || failed {
		t.Errorf("windows launch = %q %v %v, want the registered ref, no arches, not failed", image, arches, failed)
	}
	if n := spy.calls.Load(); n != 0 {
		t.Errorf("a Windows launch asked the builder %d times", n)
	}
	linux := types.AgentRun{ID: uuid.New(), MachineKind: types.MachineLinuxVM, Agent: "claude-code"}
	ast.mu.Lock()
	ast.runs[linux.ID] = linux
	ast.mu.Unlock()
	req := createRunRequest{Agent: "claude-code", MachineKind: string(types.MachineLinuxVM)}
	gotImage, gotArches, gotFailed := srv.launchRunImage(ctx, req, linux, nil)
	wantImage, wantArches, wantFailed := srv.resolveCreateRunImage(ctx, req, linux, nil)
	if gotImage != wantImage || !slices.Equal(gotArches, wantArches) || gotFailed != wantFailed || gotImage == winvmRef {
		t.Errorf("linux launch = %q %v %v, want resolveCreateRunImage's %q %v %v", gotImage, gotArches, gotFailed, wantImage, wantArches, wantFailed)
	}
}

// Every door that inserts a run row is a caller of createRun or createQueuedRun (TestRunCreationRoutesThroughTheRevocationGate
// holds that). Each either reaches the fold, whose stepWindowsImage refuses or admits windows_vm and whose
// newRunRow inserts the registered ref, or names no machine kind anywhere in its file, so its run is always a
// container. A new caller fails here until it is classified.
func TestWindowsRunInsertDoorsCensus(t *testing.T) {
	folded := map[string]string{
		"runs.go:handleCreateRun": "POST /runs: the fold, then newRunRow's row with the registered ref",
		"run_queue.go:answer":     "a launch that waits for a pool slot: the same fold and row; the promotion refolds",
	}
	container := map[string]string{
		"harnesscred.go:launchHarnessLoginRun":    "a harness sign-in run",
		"site_config_probe.go:runSiteConfigProbe": "a site config probe run",
		"source_scan.go:launchSourceScanRun":      "a source scan run",
		"workspace_run_launch.go:launchRecordRun": "a workspace record run",
	}
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		namesKind := false
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == "MachineKind" {
				namesKind = true
			}
			return true
		})
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name.Name == "createRun" || fn.Name.Name == "createQueuedRun" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "createRun" || sel.Sel.Name == "createQueuedRun") {
					key := name + ":" + fn.Name.Name
					found[key] = true
					if _, ok := container[key]; ok && namesKind {
						t.Errorf("%s inserts a run and its file names MachineKind: refuse windows_vm there, or route it through the fold", key)
					}
				}
				return true
			})
		}
	}
	for key := range found {
		if folded[key] == "" && container[key] == "" {
			t.Errorf("%s inserts a run and is not classified: it must reach stepWindowsImage or name no machine kind", key)
		}
	}
	for key := range folded {
		if !found[key] {
			t.Errorf("the census lists %s, which no longer inserts a run", key)
		}
	}
	for key := range container {
		if !found[key] {
			t.Errorf("the census lists %s, which no longer inserts a run", key)
		}
	}
	if len(found) != len(folded)+len(container) {
		t.Errorf("found %d insert doors, want %d", len(found), len(folded)+len(container))
	}
}

// A holder's subject is read decoded: a client that percent-encodes the email removes the same holder.
func TestWindowsHolderRemoveDecodesSubject(t *testing.T) {
	srv, ws, _ := winvmServer(t, true)
	for _, path := range []string{"ana%40example.com", "ana@example.com"} {
		w := do(t, srv, http.MethodDelete, "/api/v1/windows-licence-pools/"+ws.pool.ID.String()+"/holders/"+path, adminToken, "")
		if w.Code != http.StatusOK {
			t.Fatalf("remove %s = %d %s", path, w.Code, w.Body)
		}
	}
	if want := []string{"ana@example.com", "ana@example.com"}; !slices.Equal(ws.removed, want) {
		t.Errorf("subjects removed = %v, want %v", ws.removed, want)
	}
}

// An image name matches whatever its case, as the lower(name) unique index does, and the registered ref is
// what the seat check is asked about.
func TestWindowsVMImageNameAnyCase(t *testing.T) {
	srv, ws, _ := winvmServer(t, true)
	launch, rf := srv.windowsImageDecision(winvmHTTP(memberContext()), winvmReq(func(q *createRunRequest) { q.WindowsImage = "SERVER" }), types.RunPolicySpec{})
	if rf != nil || launch.Image.Ref != winvmRef {
		t.Fatalf("SERVER = %+v, %v; want the image registered as Server", launch.Image, rf)
	}
	if !slices.Equal(ws.checks, []string{winvmRef + "|sub-admit-member"}) {
		t.Errorf("seat checks = %v, want the registered ref for the member", ws.checks)
	}
}
