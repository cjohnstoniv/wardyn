// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Runner-bound folders over a real store: where a run with one goes, and what the runner's lifecycle
// does to the binding (it stays; the run is refused by name).

// boundFolderWorkspace creates, as c through the workspace door, a workspace whose one source is path
// bound to runnerID, and returns the stored workspace and its library source.
func (e *placementEnv) boundFolderWorkspace(c *http.Cookie, name, path string, runnerID uuid.UUID) (types.Workspace, types.Source) {
	e.t.Helper()
	w := e.call(c, http.MethodPost, "/api/v1/workspaces", workspaceBody(e.t, name, boundSource(path, runnerID)))
	if w.Code != http.StatusCreated {
		e.t.Fatalf("create the bound workspace = %d %s", w.Code, w.Body)
	}
	created := decodeJSON[types.Workspace](e.t, w)
	ctx := context.Background()
	ws, err := e.pg.GetWorkspace(ctx, created.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	if len(ws.Attachments) != 1 || ws.Attachments[0].SourceID == nil {
		e.t.Fatalf("attachments = %+v, want the one library source", ws.Attachments)
	}
	src, err := e.pg.GetSource(ctx, *ws.Attachments[0].SourceID)
	if err != nil {
		e.t.Fatal(err)
	}
	return ws, src
}

// boundRunBody is a run body attaching wsID, with extra fields.
func boundRunBody(wsID uuid.UUID, extra string) string {
	body := `{"agent":"claude-code","task":"t","workspace_id":"` + wsID.String() + `"`
	if extra != "" {
		body += "," + extra
	}
	return body + "}"
}

// A folder that was never scanned runs on the runner it is bound to, at every door: placement keeps that
// runner alone of a mixed pool, and the mount is verified there inside the roots it advertises.
func TestRunCreate_RunnerBoundFolderPlacedOnItsRunner(t *testing.T) {
	mine, other := uuid.New(), uuid.New()
	own, second, alpha := poolTarget{id: mine.String(), runnerID: &mine}, poolTarget{id: other.String(), runnerID: &other}, poolTarget{id: "alpha", executor: "alpha"}
	kinds, refusal := (&Server{}).kindTargets(context.Background(), placementInput{req: &createRunRequest{}, mountRunner: &mine},
		types.RunnerPool{Name: "Mixed", Scope: types.RunnerPoolUser}, []poolTarget{own, second, alpha})
	if refusal != nil || len(kinds) != 1 || len(kinds[types.PlacementLocal]) != 1 || *kinds[types.PlacementLocal][0].runnerID != mine {
		t.Fatalf("kindTargets = %v, %+v; want the bound folder's runner alone", kinds, refusal)
	}
	s := &Server{}
	advertiseRunnerRoots(t, s, mine, boundLaptopRoot)
	advertiseRunnerRoots(t, s, other, boundLaptopRoot)
	mounts := []runner.Mount{{Source: boundFolder, Target: composerWorkspaceTarget, ReadOnly: true, MemberAuthored: true, BoundRunner: mine.String()}}
	if got := s.mountPathsOn(mine, mounts); !got["SandboxSpec.Mounts[0]"] {
		t.Errorf("mountPathsOn(its runner) = %v, want the folder verified", got)
	}
	if got := s.mountPathsOn(other, mounts); got["SandboxSpec.Mounts[0]"] {
		t.Errorf("mountPathsOn(another runner) = %v, want the folder unverified", got)
	}

	e := newPlacementEnv(t)
	e.srv.cfg.UserMounts = runner.UserMountPolicy{Roots: []string{boundLaptopRoot}}
	pool := e.createPool("Laptops", types.RunnerPoolUser)
	laptop := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2)
	e.addToPool(e.person, pool.ID, laptop)
	advertiseRunnerRoots(t, e.srv, laptop, boundLaptopRoot)
	ws, lib := e.boundFolderWorkspace(e.person, "laptop-folder", boundFolder, laptop)
	if lib.Status != types.WorkspacePendingScan || lib.RunnerID == nil || *lib.RunnerID != laptop {
		t.Fatalf("library source = status %q runner %v; want a never-scanned source (pending_scan) bound to %s", lib.Status, lib.RunnerID, laptop)
	}

	answers := e.answers(e.person, boundRunBody(ws.ID, `"runner_pool_id":"`+pool.ID.String()+`"`))
	for door, w := range answers {
		if w.Code >= http.StatusBadRequest {
			t.Errorf("%s = %d %s, want the bound folder admitted on its runner", door, w.Code, w.Body)
		}
	}
	var created types.AgentRun
	if err := json.Unmarshal(answers["/api/v1/runs"].Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	run, err := e.pg.GetRun(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Placement != types.PlacementLocal || run.RunnerID == nil || *run.RunnerID != laptop {
		t.Errorf("run row = placement %q runner %v, want local on %s", run.Placement, run.RunnerID, laptop)
	}
	if after, err := e.pg.GetSource(context.Background(), lib.ID); err != nil || after.Status != types.WorkspacePendingScan {
		t.Errorf("library source after the run = %q (%v), want it still pending_scan: the run needs no scan", after.Status, err)
	}
}

// A run with a runner-bound folder goes to that runner or nowhere: a pin to another runner, a remote
// pool, no pool at all, a folder outside the runner's advertised roots and a runner with no --drive-root
// are each refused as placement_local_path, at every door.
func TestRunCreate_RunnerBoundFolderRefusedElsewhere(t *testing.T) {
	mine, other := uuid.New(), uuid.New()
	own, second, alpha := poolTarget{id: mine.String(), runnerID: &mine}, poolTarget{id: other.String(), runnerID: &other}, poolTarget{id: "alpha", executor: "alpha"}
	_, refusal := (&Server{}).kindTargets(context.Background(), placementInput{req: &createRunRequest{RunnerID: other.String()}, mountRunner: &mine},
		types.RunnerPool{Name: "Mixed", Scope: types.RunnerPoolUser}, []poolTarget{own, second, alpha})
	if refusal == nil || refusal.body.Reason != string(placement.ReasonPlacementLocalPath) {
		t.Errorf("a pin to another runner = %+v, want %s", refusal, placement.ReasonPlacementLocalPath)
	}
	s := &Server{}
	advertiseRunnerRoots(t, s, mine, boundLaptopRoot+"-elsewhere")
	advertiseRunnerRoots(t, s, other)
	for _, id := range []uuid.UUID{mine, other} {
		mounts := []runner.Mount{{Source: boundFolder, Target: composerWorkspaceTarget, ReadOnly: true, MemberAuthored: true, BoundRunner: id.String()}}
		if got := s.mountPathsOn(id, mounts); got["SandboxSpec.Mounts[0]"] {
			t.Errorf("mountPathsOn(%s) = %v, want unverified outside the advertised roots", id, got)
		}
	}

	t.Run("unpooled", func(t *testing.T) {
		srv, st, fr := runnerDispatchHarness(t, runner.UserMountPolicy{Roots: []string{boundLaptopRoot}})
		laptop := st.add(ownerMemberSub, types.RunnerOwnerPerson, types.RunnerClaimed)
		wsID := st.put(types.Workspace{OwnedBy: ownerMemberSub, Status: types.WorkspaceScanned, Sources: []types.WorkspaceSource{boundSource(boundFolder, laptop)}})
		member := ssoSession(t, ownerMemberSub, "member@corp.example", oidc.RoleUser)
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", member, boundRunBody(wsID, ""))
		if w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != string(placement.ReasonPlacementLocalPath) {
			t.Errorf("unpooled = %d %s, want 422 %s", w.Code, w.Body.String(), placement.ReasonPlacementLocalPath)
		}
		if fr.createCalls != 0 {
			t.Errorf("CreateSandbox calls = %d, want 0", fr.createCalls)
		}
	})

	e := newPlacementEnv(t)
	e.srv.cfg.UserMounts = runner.UserMountPolicy{Roots: []string{boundLaptopRoot}}
	laptops := e.createPool("Laptops", types.RunnerPoolUser)
	laptop := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2)
	spare := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2)
	e.addToPool(e.person, laptops.ID, laptop)
	e.addToPool(e.person, laptops.ID, spare)
	advertiseRunnerRoots(t, e.srv, laptop, boundLaptopRoot)
	advertiseRunnerRoots(t, e.srv, spare, boundLaptopRoot)
	org := e.createPool("Org", types.RunnerPoolOrganization)
	e.addExecutors(org.ID, "alpha")
	ws, _ := e.boundFolderWorkspace(e.person, "bound", boundFolder, laptop)
	inLaptops := `"runner_pool_id":"` + laptops.ID.String() + `"`

	for _, c := range []struct {
		name    string
		arrange func()
		extra   string
		names   string // a sentence fragment the refusal must carry, "" for none
	}{
		{name: "pinned to another of their runners", extra: inLaptops + `,"placement":"local","runner_id":"` + spare.String() + `"`},
		{name: "a remote pool", extra: `"runner_pool_id":"` + org.ID.String() + `"`},
		{name: "outside the runner's advertised roots", arrange: func() { advertiseRunnerRoots(t, e.srv, laptop, boundLaptopRoot+"-elsewhere") }, extra: inLaptops},
		{name: "a runner with no --drive-root", arrange: func() { advertiseRunnerRoots(t, e.srv, laptop) }, extra: inLaptops, names: "--drive-root"},
	} {
		if c.arrange != nil {
			c.arrange()
		}
		for door, w := range e.answers(e.person, boundRunBody(ws.ID, c.extra)) {
			if w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != string(placement.ReasonPlacementLocalPath) || !strings.Contains(w.Body.String(), c.names) {
				t.Errorf("%s %s = %d %s, want 422 %s %q", c.name, door, w.Code, w.Body, placement.ReasonPlacementLocalPath, c.names)
			}
		}
	}
	if n := e.count(`SELECT count(*) FROM agent_runs`); n != 0 {
		t.Errorf("%d runs were written; every door refused", n)
	}
}

// runnerDispatchHarness is userDispatchHarness over ownerRunnerStore.
func runnerDispatchHarness(t *testing.T, mounts runner.UserMountPolicy) (*Server, *ownerRunnerStore, *fakeRunner) {
	t.Helper()
	st := &ownerRunnerStore{ownerStore: newOwnerStore(), boundRunners: boundRunners{rows: map[uuid.UUID]types.Runner{}}}
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Broker = h.broker
	cfg.UserMounts = mounts
	fr := &fakeRunner{}
	cfg.Runner = fr
	cfg.DefaultPolicy = types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, MinConfinementClass: types.CC2}
	return New(cfg), st, fr
}

// seedBoundFolder seeds a workspace of the fixture runner's owner whose one source is path bound to that
// runner, as the workspace door writes it: a library row carrying the binding and an attachment to it.
func (f *admissionFixture) seedBoundFolder(t *testing.T, path string) (types.Workspace, uuid.UUID) {
	t.Helper()
	now := time.Now().UTC()
	src, err := f.st.UpsertSource(f.ctx, types.Source{ID: uuid.New(), Kind: types.SourceLocalDir, Locator: path, Name: filepath.Base(path),
		Status: types.WorkspacePendingScan, RunnerID: &f.rn.ID, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := f.st.CreateWorkspace(f.ctx, types.Workspace{ID: uuid.New(), Name: filepath.Base(path), OwnedBy: f.rn.Owner, Status: types.WorkspaceScanned,
		Sources:     []types.WorkspaceSource{boundSource(path, f.rn.ID)},
		Attachments: []types.WorkspaceAttachment{{SourceID: &src.ID}}, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	return ws, src.ID
}

// sourceRunner is the stored binding of a library source.
func (f *admissionFixture) sourceRunner(t *testing.T, id uuid.UUID) *uuid.UUID {
	t.Helper()
	var got *uuid.UUID
	if err := f.pool.QueryRow(f.ctx, `SELECT runner_id FROM sources WHERE id = $1`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

// runGate is the run gate's answer for a run mounting path.
func (f *admissionFixture) runGate(path string) (int, string, error) {
	return f.srv.validateWorkspaceSources(f.ctx, readOnlyMount(path), runCarrierRule{})
}

// wantRefusedNamed requires the run gate to refuse path by the stale-binding reason, naming the folder's
// workspace (seedBoundFolder names it after the path's base) as the console's copy does.
func (f *admissionFixture) wantRefusedNamed(t *testing.T, when, path string) {
	t.Helper()
	code, reason, err := f.runGate(path)
	want := "The folder for " + filepath.Base(path) + " is on "
	if err == nil || code != http.StatusUnprocessableEntity || reason != "workspace_source_runner_unavailable" || !strings.Contains(err.Error(), want) {
		t.Errorf("%s: run gate = %d %q %v, want 422 workspace_source_runner_unavailable saying %q", when, code, reason, err, want)
	}
}

// Revoking the runner a folder is bound to is never blocked by the binding and never touches it; a run
// with the folder is then refused by name.
func TestRevokeRunner_BoundSourceStaysAndRunRefusedNamed(t *testing.T) {
	f := newAdmissionFixture(t)
	f.srv.cfg.UserMounts = runner.UserMountPolicy{Roots: []string{boundLaptopRoot}}
	_, srcID := f.seedBoundFolder(t, boundFolder)
	if code, reason, err := f.runGate(boundFolder); err != nil {
		t.Fatalf("before the revoke: run gate = %d %s %v, want the bound folder admitted", code, reason, err)
	}
	rev, _, err := f.srv.revokeRunner(f.ctx, f.rn.ID, types.ActorSystem, "security-admin", "")
	if rev.Runner.ID != f.rn.ID {
		t.Fatalf("revoke = %+v, %v; the binding must never block it", rev, err)
	}
	if got := f.sourceRunner(t, srcID); got == nil || *got != f.rn.ID {
		t.Errorf("binding after the revoke = %v, want it kept on %s", got, f.rn.ID)
	}
	f.wantRefusedNamed(t, "after the revoke", boundFolder)
}

// Handing a member's workspace to the operator, by the SCIM purge and by POST /workspaces/{id}/reassign,
// keeps the binding: the folder still names the person's runner, which the operator does not own, so a
// run with it is refused by name while the runner itself stays claimed.
func TestReassignWorkspace_KeepsBinding_RunRefusedNamed(t *testing.T) {
	f := newAdmissionFixture(t)
	f.srv.cfg.UserMounts = runner.UserMountPolicy{Roots: []string{boundLaptopRoot}}
	viaDoorPath, viaSCIMPath := boundLaptopRoot+"/door", boundLaptopRoot+"/scim"
	viaDoor, doorSrc := f.seedBoundFolder(t, viaDoorPath)
	_, scimSrc := f.seedBoundFolder(t, viaSCIMPath)

	if w := do(t, f.srv, http.MethodPost, "/api/v1/workspaces/"+viaDoor.ID.String()+"/reassign", adminToken, ""); w.Code >= http.StatusBadRequest {
		t.Fatalf("reassign = %d %s", w.Code, w.Body.String())
	}
	if _, err := f.srv.reassignWorkspaces(f.ctx, []string{f.rn.Owner}); err != nil {
		t.Fatalf("scim reassign: %v", err)
	}
	for path, id := range map[string]uuid.UUID{viaDoorPath: doorSrc, viaSCIMPath: scimSrc} {
		if got := f.sourceRunner(t, id); got == nil || *got != f.rn.ID {
			t.Errorf("%s: binding after the reassign = %v, want it kept on %s", path, got, f.rn.ID)
		}
		f.wantRefusedNamed(t, path+" after the reassign", path)
	}
	if rn, err := f.st.GetRunner(f.ctx, f.rn.ID); err != nil || rn.State != types.RunnerClaimed {
		t.Errorf("runner = %q (%v), want it still claimed: the reassign alone makes the binding stale", rn.State, err)
	}
}

// The leaver purge revokes the leaver's runners and hands their workspaces to the operator; neither step
// is blocked by a binding or touches it, and a run with the folder is refused by name.
func TestLeaverPurge_RevokesRunnerKeepsBinding(t *testing.T) {
	f := newAdmissionFixture(t)
	f.srv.cfg.UserMounts = runner.UserMountPolicy{Roots: []string{boundLaptopRoot}}
	_, srcID := f.seedBoundFolder(t, boundFolder)

	revoked, err := f.srv.revokeLeaverRunners(f.ctx, []string{f.rn.Owner})
	if err != nil || revoked["runners_revoked"] != 1 {
		t.Fatalf("leaver runners = %v, %v; want the bound runner revoked", revoked, err)
	}
	reassigned, err := f.srv.reassignWorkspaces(f.ctx, []string{f.rn.Owner})
	if err != nil || reassigned["workspaces_reassigned"] != 1 {
		t.Fatalf("leaver workspaces = %v, %v; want the bound workspace handed over", reassigned, err)
	}
	if got := f.sourceRunner(t, srcID); got == nil || *got != f.rn.ID {
		t.Errorf("binding after the purge = %v, want it kept on %s", got, f.rn.ID)
	}
	if rn, err := f.st.GetRunner(f.ctx, f.rn.ID); err != nil || rn.State != types.RunnerRevoked {
		t.Errorf("runner = %q (%v), want revoked", rn.State, err)
	}
	f.wantRefusedNamed(t, "after the purge", boundFolder)
}

// A runner whose wardyn-runnerd does not say bound_folders predates the checks that bind such a folder:
// every door refuses the run on it, saying to update the runner, and writes no run.
func TestRunCreate_RunnerBoundFolderRefusedOnAnOlderRunner(t *testing.T) {
	e := newPlacementEnv(t)
	e.srv.cfg.UserMounts = runner.UserMountPolicy{Roots: []string{boundLaptopRoot}}
	pool := e.createPool("Laptops", types.RunnerPoolUser)
	laptop := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2)
	e.addToPool(e.person, pool.ID, laptop)
	advertiseRunnerCaps(t, e.srv, laptop, false, boundLaptopRoot)
	ws, _ := e.boundFolderWorkspace(e.person, "older-runner", boundFolder, laptop)
	for door, w := range e.answers(e.person, boundRunBody(ws.ID, `"runner_pool_id":"`+pool.ID.String()+`"`)) {
		if w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != string(placement.ReasonPlacementLocalPath) || !strings.Contains(w.Body.String(), "update the runner") {
			t.Errorf("%s = %d %s, want 422 %s saying to update the runner", door, w.Code, w.Body, placement.ReasonPlacementLocalPath)
		}
	}
	if n := e.count(`SELECT count(*) FROM agent_runs`); n != 0 {
		t.Errorf("%d runs were written; every door refused", n)
	}
}

// A launch with a bound folder that waits keeps its folder's runner: the queue's verdict never offers it
// another runner of the pool with room, and offers it none once that runner is revoked.
func TestQueue_RunnerBoundFolderWaitsForItsRunnerOnly(t *testing.T) {
	e := newQueueEnv(t)
	ctx := context.Background()
	e.srv.cfg.UserMounts = runner.UserMountPolicy{Roots: []string{boundLaptopRoot}}
	pool := e.createPool("Laptops", types.RunnerPoolUser)
	bound := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2)
	spare := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2)
	e.addToPool(e.person, pool.ID, bound)
	e.addToPool(e.person, pool.ID, spare)
	advertiseRunnerRoots(t, e.srv, bound, boundLaptopRoot)
	advertiseRunnerRoots(t, e.srv, spare, boundLaptopRoot)
	e.maxRuns(bound, e.personP, 1)
	pool = e.reread(pool)
	e.occupyRunner(pool, bound, e.personP)
	ws, _ := e.boundFolderWorkspace(e.person, "bound", boundFolder, bound)

	waiting := e.waitsFor(boundRunBody(ws.ID, `"runner_pool_id":"`+pool.ID.String()+`"`), "its folder's runner is full and the spare is idle")
	launch := e.waitingLaunch(waiting.ID)
	if launch.MountRunner == nil || *launch.MountRunner != bound {
		t.Fatalf("the queued launch's mount_runner = %v, want %s", launch.MountRunner, bound)
	}
	room, err := e.srv.queueRunnerFree(withOIDCHuman(ctx, e.personP), e.reread(pool), launch)
	if err != nil || len(room.runners)+len(room.remotes) != 0 {
		t.Fatalf("the queue's verdict = %+v, %v; want it held for %s, never offered the spare", room, err, bound)
	}
	e.exec(`UPDATE runners SET state = 'revoked', revoked_at = now() WHERE id = $1`, bound)
	room, err = e.srv.queueRunnerFree(withOIDCHuman(ctx, e.personP), e.reread(pool), launch)
	if err != nil || len(room.runners)+len(room.remotes) != 0 {
		t.Errorf("the verdict once %s is revoked = %+v, %v; want no runner offered, never the spare %s", bound, room, err, spare)
	}
}

// Seeding a run checks a runner-bound folder by its text alone: what this server has at the same path is
// never resolved, so a server-side symlink into a denied path neither refuses the folder nor tells the
// caller it is there. The same path unbound is a folder on this server and is still resolved and refused.
func TestRunCreate_RunnerBoundFolderSeedReadsNoServerPath(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "proj")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	e := newPlacementEnv(t)
	e.srv.cfg.UserMounts = runner.UserMountPolicy{Roots: []string{root}}
	pool := e.createPool("Laptops", types.RunnerPoolUser)
	laptop := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2)
	e.addToPool(e.person, pool.ID, laptop)
	advertiseRunnerRoots(t, e.srv, laptop, root)
	w := e.call(e.person, http.MethodPost, "/api/v1/workspaces", workspaceBody(t, "server-folder", types.WorkspaceSource{Type: types.WorkspaceSourceTypeLocalDir, Path: path}))
	if w.Code != http.StatusCreated {
		t.Fatalf("create the unbound workspace = %d %s", w.Code, w.Body)
	}
	unbound := decodeJSON[types.Workspace](t, w)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", path); err != nil {
		t.Fatal(err)
	}
	bound, _ := e.boundFolderWorkspace(e.person, "laptop-folder", path, laptop)

	onPool := `"runner_pool_id":"` + pool.ID.String() + `"`
	for door, w := range e.answers(e.person, boundRunBody(bound.ID, onPool)) {
		if w.Code >= http.StatusBadRequest {
			t.Errorf("bound %s = %d %s, want the folder past seeding: this server's %s is not the runner's", door, w.Code, w.Body, path)
		}
	}
	got := e.answers(e.person, boundRunBody(unbound.ID, onPool))
	if body := sameEverywhere(t, "unbound", got, http.StatusUnprocessableEntity, reasonWorkspaceSeedPolicyConflict); !strings.Contains(body, "/etc") {
		t.Errorf("unbound refusal %s, want it to name the denied path the server folder resolves to", body)
	}
}
