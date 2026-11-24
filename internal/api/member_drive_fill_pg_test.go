// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	secretspg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	fillPGRepo    = "acme/app"
	fillPGSharedS = "git-pat-github-com"
	fillPGAlice   = "alice"
)

// fillPGRunner is a substrate that answers the drive reads a fill needs: no sandbox holds the drive unless holder
// says so.
type fillPGRunner struct {
	*fakeRunner
	holder string
}

// fillPoolRunner is the pool tests' runner with the same drive reads.
type fillPoolRunner struct{ *poolRunner }

func (r *fillPoolRunner) DriveHolder(context.Context, types.DriveMount) (string, error) {
	return "", nil
}
func (r *fillPoolRunner) CanManageDrives(context.Context) error { return nil }
func (r *fillPoolRunner) ReclaimDrive(context.Context, types.DriveMount) (runner.DriveReclaimOutcome, error) {
	return runner.DriveReclaimDeleted, nil
}

func (r *fillPGRunner) DriveHolder(context.Context, types.DriveMount) (string, error) {
	return r.holder, nil
}
func (r *fillPGRunner) CanManageDrives(context.Context) error { return nil }
func (r *fillPGRunner) ReclaimDrive(context.Context, types.DriveMount) (runner.DriveReclaimOutcome, error) {
	return runner.DriveReclaimDeleted, nil
}

// fillPG is a Postgres-backed server where Alice has a drive, an own-token row decides github.com/acme, she has a
// sealed token of her own, the ceiling pairs one github.com secret and a shared workspace carries acme/app on a base
// image.
type fillPG struct {
	ownerOnlyPG
	t     *testing.T
	rn    *fillPGRunner
	pg    store.PG
	ws    types.Workspace
	drive types.UserDrive
}

func fillCeilingPairing(secret string, repos ...string) types.GrantSpec {
	sc := types.GitPATScope{Host: "github.com", SecretName: secret}
	if len(repos) > 0 {
		sc.Repos = types.PATRepoSet(repos...)
	}
	raw, _ := json.Marshal(sc)
	return types.GrantSpec{Kind: types.GrantGitPAT, Scope: raw}
}

func newFillPG(t *testing.T) *fillPG {
	t.Helper()
	e := newOwnerOnlyPG(t)
	ctx := context.Background()
	f := &fillPG{ownerOnlyPG: e, t: t, pg: store.NewPG(e.pool), rn: &fillPGRunner{fakeRunner: &fakeRunner{}}}
	srv := e.h.srv
	srv.cfg.Runner, srv.cfg.RunnerTarget, srv.cfg.ImageBuilder = f.rn, "docker", fakeImageBuilder{}
	srv.cfg.DefaultPolicy = types.RunPolicySpec{MinConfinementClass: types.CC1, AllowedDomains: []string{"github.com"},
		EligibleGrants: []types.GrantSpec{fillCeilingPairing(fillPGSharedS)}}
	srv.router = srv.routes()
	if _, err := f.pg.PutSiteConfig(ctx, adoSite(ghOwnTestRow())); err != nil {
		t.Fatalf("PutSiteConfig: %v", err)
	}
	raw, _ := json.Marshal(types.GitHubOwnToken{Token: "github_pat_alice", AccountID: 101, Login: "alice-gh",
		ExpiresOn: time.Now().AddDate(0, 0, 5), StoredAt: time.Now()})
	if err := e.sec.For(fillPGAlice).Put(ctx, types.GitHubOwnTokenSecretName(ghOwnRowID), raw); err != nil {
		t.Fatal(err)
	}
	f.ws = f.workspace(&types.WorkspaceBaseImage{Kind: "custom", Image: "ubuntu:24.04"}, nil)
	var err error
	if f.drive, err = f.pg.CreateMemberDrive(ctx, types.UserDrive{Name: "Notes", Backend: types.DriveBackendDockerVolume,
		OwnerSubject: fillPGAlice, CreatedBy: fillPGAlice}, 3); err != nil {
		t.Fatal(err)
	}
	return f
}

// workspace stores a shared workspace carrying acme/app.
func (f *fillPG) workspace(img *types.WorkspaceBaseImage, reqs map[string]types.WorkspaceRequirement) types.Workspace {
	f.t.Helper()
	ws, err := f.pg.CreateWorkspace(context.Background(), types.Workspace{ID: uuid.New(), Name: "ws-" + uuid.NewString()[:6],
		Status: types.WorkspaceScanned, BaseImage: img, Requirements: reqs,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: fillPGRepo}}})
	if err != nil {
		f.t.Fatal(err)
	}
	return ws
}

func (f *fillPG) body(ws uuid.UUID, repo, into string) string {
	b, _ := json.Marshal(map[string]string{"workspace_id": ws.String(), "repo": repo, "into": into})
	return string(b)
}

// fillAs posts the fill of drive id as sub.
func (f *fillPG) fillAs(sub string, id uuid.UUID, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	member := ssoSession(f.t, sub, sub+"@corp.example", oidc.RoleUser)
	return doSSO(f.t, f.h.srv, http.MethodPost, "/api/v1/me/drives/"+id.String()+"/seed", member, body)
}

func (f *fillPG) fill() *httptest.ResponseRecorder {
	f.t.Helper()
	return f.fillAs(fillPGAlice, f.drive.ID, f.body(f.ws.ID, fillPGRepo, ""))
}

func (f *fillPG) count(q string) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), q).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fillPG) noRunYet() {
	f.t.Helper()
	if runs, grants := f.count(`SELECT count(*) FROM agent_runs`), f.count(`SELECT count(*) FROM credential_grants`); runs != 0 || grants != 0 {
		f.t.Fatalf("a refused fill left %d run rows and %d grant rows", runs, grants)
	}
}

func (f *fillPG) rows(action string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range f.h.audit.snapshot() {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

// accepted is a 202 answer's run id.
func (f *fillPG) accepted(w *httptest.ResponseRecorder) uuid.UUID {
	f.t.Helper()
	var got struct {
		RunID  string `json:"run_id"`
		Target string `json:"target"`
	}
	if w.Code != http.StatusAccepted || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Target != "/home/agent/drives/notes" {
		f.t.Fatalf("fill = %d %s, want 202 with the run and the drive's target", w.Code, w.Body.String())
	}
	return uuid.MustParse(got.RunID)
}

// fillIntegration is a corporate package feed that delivers its token by proxy header.
func fillIntegration() types.Integration {
	return types.Integration{ID: "corp-feed", Name: "Corp feed", Kind: "artifactory", Egress: []string{"feed.corp.example"},
		Secrets: []types.IntegrationSecret{{Role: types.IntegrationCredentialToken, SecretName: "feed-token",
			Delivery: &types.IntegrationDelivery{Mode: types.DeliveryProxyHeader, Header: "Authorization", Format: "Bearer %s"}}}}
}

// wantEgress asserts the sandbox the fill started was given exactly github.com, *.githubusercontent.com and the
// requirement hosts in extra, nothing else.
func (f *fillPG) wantEgress(extra ...string) {
	f.t.Helper()
	var got []string
	waitFor(f.t, "the fill's sandbox", func() bool {
		f.rn.mu.Lock()
		defer f.rn.mu.Unlock()
		got = slices.Clone(f.rn.lastSpec.ProxyConfig.Policy.AllowedDomains)
		return len(got) > 0
	})
	want := append([]string{"github.com", "*.githubusercontent.com"}, extra...)
	for _, h := range got {
		if !slices.Contains(want, h) {
			f.t.Errorf("egress %v holds %q, outside the exact set %v", got, h, want)
		}
	}
	for _, h := range append([]string{"github.com"}, extra...) {
		if !slices.Contains(got, h) {
			f.t.Errorf("egress %v lacks %q, the fill needs it", got, h)
		}
	}
}

// patGrants is the run's grants, each with its git_pat scope decoded.
func (f *fillPG) grants(run uuid.UUID) []types.CredentialGrant {
	f.t.Helper()
	grants, err := f.pg.ListGrantsByRun(context.Background(), run)
	if err != nil {
		f.t.Fatal(err)
	}
	return grants
}

func (f *fillPG) wantReason(w *httptest.ResponseRecorder, status int, reason string) {
	f.t.Helper()
	if w.Code != status || !strings.Contains(w.Body.String(), `"reason":"`+reason+`"`) {
		f.t.Fatalf("fill = %d %s, want %d %s", w.Code, w.Body.String(), status, reason)
	}
}

// wantOwnTokenGrant asserts g is the one grant a fill authors, rewritten to Alice's own token.
func wantOwnTokenGrant(t *testing.T, g types.CredentialGrant) {
	t.Helper()
	sc, err := types.DecodeGitPATScope(g.Spec.Scope)
	if err != nil || g.Spec.Kind != types.GrantGitPAT || !g.Spec.OwnerOnly || !types.IsGitHubOwnTokenSecret(sc.SecretName) ||
		sc.Repos == nil || len(*sc.Repos) != 1 || (*sc.Repos)[0] != driveFillRepoPath(fillPGRepo) || sc.Access != types.PATAccessRead {
		t.Fatalf("grant = %+v %s, want one owner-only own-token git_pat for %s, read", g.Spec, g.Spec.Scope, driveFillRepoPath(fillPGRepo))
	}
}

// TestDriveSeed_OwnerOnly_PG: another person's drive, an admin drive and an unknown id answer the same 404 and
// write authz.denied rows only; a caller that is not a person is 403.
func TestDriveSeed_OwnerOnly_PG(t *testing.T) {
	f := newFillPG(t)
	ctx := context.Background()
	adminDrive, err := f.pg.UpsertUserDrive(ctx, types.UserDrive{Name: "Shared", Backend: types.DriveBackendDockerVolume}, false)
	if err != nil {
		t.Fatal(err)
	}
	body := f.body(f.ws.ID, fillPGRepo, "")
	var answers []string
	for _, id := range []uuid.UUID{f.drive.ID, adminDrive.ID, uuid.New()} {
		w := f.fillAs("bob", id, body)
		f.wantReason(w, http.StatusNotFound, "drive_not_found")
		answers = append(answers, strings.ReplaceAll(w.Body.String(), id.String(), "ID"))
	}
	if answers[0] != answers[1] || answers[1] != answers[2] {
		t.Errorf("the 404 bodies differ: %v", answers)
	}
	if n := len(f.rows("authz.denied")); n != 3 || len(f.rows("drive.seed")) != 0 {
		t.Errorf("rows: %d authz.denied, %d drive.seed; want 3 and 0", n, len(f.rows("drive.seed")))
	}
	w := doSSO(t, f.h.srv, http.MethodPost, "/api/v1/me/drives/"+f.drive.ID.String()+"/seed", nil, body)
	if w.Code == http.StatusAccepted {
		t.Fatalf("an unauthenticated fill was accepted: %s", w.Body.String())
	}
	f.wantReason(do(t, f.h.srv, http.MethodPost, "/api/v1/me/drives/"+f.drive.ID.String()+"/seed", adminToken, body), http.StatusForbidden, reasonMemberDriveRequiresPerson)
	f.noRunYet()
}

// TestDriveSeed_RunCarriesOnlyTheOwnToken_PG: a fill's run holds exactly the one owner-only own-token git_pat, with
// the clone URL's path and read access, whatever else the workspace requires.
func TestDriveSeed_RunCarriesOnlyTheOwnToken_PG(t *testing.T) {
	t.Run("a workspace with no requirements", func(t *testing.T) {
		f := newFillPG(t)
		grants := f.grants(f.accepted(f.fill()))
		if len(grants) != 1 {
			t.Fatalf("grants = %+v, want exactly one", grants)
		}
		wantOwnTokenGrant(t, grants[0])
		f.wantEgress()
	})
	t.Run("a workspace with a required egress row", func(t *testing.T) {
		f := newFillPG(t)
		ws := f.workspace(&types.WorkspaceBaseImage{Kind: "custom", Image: "ubuntu:24.04"},
			map[string]types.WorkspaceRequirement{"egress:registry.example": {Level: "required", Provenance: "operator_set"}})
		w := f.fillAs(fillPGAlice, f.drive.ID, f.body(ws.ID, fillPGRepo, ""))
		grants := f.grants(f.accepted(w))
		if len(grants) != 1 {
			t.Fatalf("grants = %+v, want exactly the git_pat", grants)
		}
		wantOwnTokenGrant(t, grants[0])
		f.wantEgress("registry.example")
	})
	t.Run("a workspace with a required integration", func(t *testing.T) {
		f := newFillPG(t)
		if _, err := f.pg.PutSiteConfig(context.Background(), types.SiteConfig{Integrations: []types.Integration{fillIntegration()},
			WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{ghOwnTestRow()}}}); err != nil {
			t.Fatal(err)
		}
		if err := f.sec.Put(context.Background(), "feed-token", []byte("operator-feed-token")); err != nil {
			t.Fatal(err)
		}
		ws := f.workspace(&types.WorkspaceBaseImage{Kind: "custom", Image: "ubuntu:24.04"},
			map[string]types.WorkspaceRequirement{"integration:corp-feed": {Level: "required", Provenance: "operator_set"}})
		// The post-create check passes: the run is accepted, not ended, and holds the own token plus the feed's api_key.
		run := f.accepted(f.fillAs(fillPGAlice, f.drive.ID, f.body(ws.ID, fillPGRepo, "")))
		if state := f.count(`SELECT count(*) FROM agent_runs WHERE state = 'KILLED'`); state != 0 {
			t.Errorf("killed runs = %d, want the post-create check to pass", state)
		}
		var pats, keys []types.CredentialGrant
		for _, g := range f.grants(run) {
			switch g.Spec.Kind {
			case types.GrantGitPAT:
				pats = append(pats, g)
			case types.GrantAPIKey:
				keys = append(keys, g)
			default:
				t.Errorf("unexpected grant kind %s", g.Spec.Kind)
			}
		}
		if len(pats) != 1 || len(keys) != 1 {
			t.Fatalf("git_pat grants %d, api_key grants %d; want exactly [git_pat, the feed's api_key]", len(pats), len(keys))
		}
		wantOwnTokenGrant(t, pats[0])
		var sc struct {
			Host       string `json:"host"`
			SecretName string `json:"secret_name"`
		}
		if err := json.Unmarshal(keys[0].Spec.Scope, &sc); err != nil || sc.Host != "feed.corp.example" || sc.SecretName != "feed-token" {
			t.Errorf("api_key scope = %s (%v), want the feed's host and secret", keys[0].Spec.Scope, err)
		}
		f.wantEgress("feed.corp.example")
	})
	t.Run("two pairings where only the second covers the repository", func(t *testing.T) {
		f := newFillPG(t)
		f.h.srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{
			fillCeilingPairing("git-pat-other", "other/elsewhere.git"), fillCeilingPairing(fillPGSharedS)}
		grants := f.grants(f.accepted(f.fill()))
		if len(grants) != 1 {
			t.Fatalf("grants = %+v, want exactly one", grants)
		}
		wantOwnTokenGrant(t, grants[0])
	})
}

// TestDriveSeed_NoOwnTokenRefusesBeforeCreate_PG: no own-token row for github.com is 422 before any run row or
// grant row.
func TestDriveSeed_NoOwnTokenRefusesBeforeCreate_PG(t *testing.T) {
	f := newFillPG(t)
	if _, err := f.pg.PutSiteConfig(context.Background(), adoSite()); err != nil {
		t.Fatal(err)
	}
	f.wantReason(f.fill(), http.StatusUnprocessableEntity, reasonDriveSeedOwnCredentialRequired)
	f.noRunYet()
}

// TestDriveSeed_NoCeilingPairingRefusesBeforeCreate_PG: a ceiling that pairs no github.com secret is 422 before any
// run.
func TestDriveSeed_NoCeilingPairingRefusesBeforeCreate_PG(t *testing.T) {
	f := newFillPG(t)
	f.h.srv.cfg.DefaultPolicy.EligibleGrants = nil
	f.wantReason(f.fill(), http.StatusUnprocessableEntity, reasonDriveSeedOwnCredentialRequired)
	f.noRunYet()
}

// TestDriveSeed_GrantWouldDropRefusesBeforeCreate_PG: a grant the create would drop is a refusal before any run: the
// caller lacks the secret capability on the shared name, or the pairing's repos leave the repository out.
func TestDriveSeed_GrantWouldDropRefusesBeforeCreate_PG(t *testing.T) {
	t.Run("no secret capability on the shared name", func(t *testing.T) {
		f := newFillPG(t)
		if _, err := f.pg.PutCapabilityEnforcement(context.Background(), map[string]bool{capSecret: true}); err != nil {
			t.Fatal(err)
		}
		f.wantReason(f.fill(), http.StatusUnprocessableEntity, reasonDriveSeedOwnCredentialRequired)
		f.noRunYet()
	})
	t.Run("the pairing's repos exclude the repository", func(t *testing.T) {
		f := newFillPG(t)
		f.h.srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{fillCeilingPairing(fillPGSharedS, "other/elsewhere.git")}
		f.wantReason(f.fill(), http.StatusUnprocessableEntity, reasonDriveSeedOwnCredentialRequired)
		f.noRunYet()
	})
}

// TestDriveSeed_RowDisabledMidwayRefuses_PG: the own-token row disabled between the pre-check and the create leaves
// the grant undecided; the create refuses it as owner_only, and the shared secret is never read.
func TestDriveSeed_RowDisabledMidwayRefuses_PG(t *testing.T) {
	f := newFillPG(t)
	defer func() { driveFillBeforeCreate = func() {} }()
	driveFillBeforeCreate = func() {
		row := ghOwnTestRow()
		row.Disabled = true
		if _, err := f.pg.PutSiteConfig(context.Background(), adoSite(row)); err != nil {
			t.Error(err)
		}
	}
	w := f.fill()
	if w.Code == http.StatusAccepted || !strings.Contains(w.Body.String(), "owner_only") {
		t.Fatalf("fill = %d %s, want the create's owner_only refusal", w.Code, w.Body.String())
	}
	f.noRunYet()
	if rows := f.rows("drive.seed"); len(rows) != 1 || rows[0].Outcome != "failure" {
		t.Errorf("drive.seed rows = %+v, want one refused", rows)
	}
}

// TestDriveSeed_OwnerStartingRefuses_PG: a run of the owner in STARTING refuses 409 with the starting sentence, and
// no second run is made.
func TestDriveSeed_OwnerStartingRefuses_PG(t *testing.T) {
	f := newFillPG(t)
	id := uuid.New()
	now := time.Now().UTC()
	if _, err := f.pg.CreateRun(context.Background(), types.AgentRun{ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: fillPGAlice,
		Agent: "claude-code", ConfinementClass: types.CC2, State: types.RunStarting, RunnerTarget: "docker",
		SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String()}); err != nil {
		t.Fatal(err)
	}
	w := f.fill()
	f.wantReason(w, http.StatusConflict, reasonDriveSeedInUse)
	if !strings.Contains(w.Body.String(), "A run of yours is starting.") {
		t.Errorf("body = %s, want the starting sentence", w.Body.String())
	}
	if n := f.count(`SELECT count(*) FROM agent_runs`); n != 1 {
		t.Errorf("runs = %d, want only the starting one", n)
	}
}

// TestDriveSeed_AuditOnEveryOutcome_PG: an accepted fill writes drive.seed with the run in the event's run id, a
// refused one writes drive.seed with its reason, and a drive that is not the caller's writes authz.denied.
func TestDriveSeed_AuditOnEveryOutcome_PG(t *testing.T) {
	f := newFillPG(t)
	run := f.accepted(f.fill())
	f.rn.holder = "sandbox-1"
	f.wantReason(f.fill(), http.StatusConflict, reasonDriveSeedInUse)
	f.wantReason(f.fillAs("bob", f.drive.ID, f.body(f.ws.ID, fillPGRepo, "")), http.StatusNotFound, "drive_not_found")
	rows := f.rows("drive.seed")
	if len(rows) != 2 {
		t.Fatalf("drive.seed rows = %d, want 2", len(rows))
	}
	if rows[0].Outcome != "success" || rows[0].RunID == nil || *rows[0].RunID != run {
		t.Errorf("accepted row = %+v, want success with run %s", rows[0], run)
	}
	var data map[string]any
	if json.Unmarshal(rows[1].Data, &data) != nil || rows[1].Outcome != "failure" || data["reason"] != reasonDriveSeedInUse || rows[1].RunID != nil {
		t.Errorf("refused row = %+v %s, want failure with reason %s and no run", rows[1], rows[1].Data, reasonDriveSeedInUse)
	}
	if n := len(f.rows("authz.denied")); n != 1 {
		t.Errorf("authz.denied rows = %d, want 1", n)
	}
}

// TestDriveSeed_CreateRefusalIsPassedThrough_PG: the create path's own refusal (a workspace nobody can see) is
// answered with its status and reason, and audited as refused with that reason.
func TestDriveSeed_CreateRefusalIsPassedThrough_PG(t *testing.T) {
	f := newFillPG(t)
	w := f.fillAs(fillPGAlice, f.drive.ID, f.body(uuid.New(), fillPGRepo, ""))
	var got errorBody
	if w.Code < 400 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Reason == "" {
		t.Fatalf("fill = %d %s, want the create path's refusal", w.Code, w.Body.String())
	}
	rows := f.rows("drive.seed")
	var data map[string]any
	if len(rows) != 1 || json.Unmarshal(rows[0].Data, &data) != nil || data["reason"] != got.Reason || rows[0].Outcome != "failure" {
		t.Fatalf("drive.seed rows = %+v, want one refused with reason %s", rows, got.Reason)
	}
	f.noRunYet()
}

// TestDriveSeed_NoBaseImageRefuses_PG: a workspace with no base image keeps the create path's status and reason, with
// the fill's own sentence.
func TestDriveSeed_NoBaseImageRefuses_PG(t *testing.T) {
	f := newFillPG(t)
	ws := f.workspace(nil, nil)
	w := f.fillAs(fillPGAlice, f.drive.ID, f.body(ws.ID, fillPGRepo, ""))
	f.wantReason(w, http.StatusBadRequest, reasonWorkspaceSeedNoBaseImage)
	if !strings.Contains(w.Body.String(), driveFillNoBaseImageMsg()) {
		t.Errorf("body = %s, want the fill's sentence", w.Body.String())
	}
	f.noRunYet()
}

// newFillQueuePG is the fill fixture over the pool test environment: the person has a drive, a workspace, an own
// token and a ceiling that pairs the shared github.com secret.
func newFillQueuePG(t *testing.T) (*queueEnv, *fillPG) {
	t.Helper()
	e := newQueueEnv(t)
	ctx := context.Background()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	sec, err := secretspg.New(e.pool, id)
	if err != nil {
		t.Fatal(err)
	}
	f := &fillPG{ownerOnlyPG: ownerOnlyPG{h: e.h, pool: e.pool, sec: sec}, t: t, pg: e.pg}
	srv := e.srv
	srv.cfg.Secrets, srv.cfg.RunnerTarget, srv.cfg.ImageBuilder = sec, "docker", fakeImageBuilder{}
	srv.cfg.Runner = &fillPoolRunner{poolRunner: e.runner}
	srv.cfg.DefaultPolicy = types.RunPolicySpec{MinConfinementClass: types.CC1, AllowedDomains: []string{"github.com"},
		EligibleGrants: []types.GrantSpec{fillCeilingPairing(fillPGSharedS)}}
	srv.router = srv.routes()
	if _, err := e.pg.PutSiteConfig(ctx, types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true},
		WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{ghOwnTestRow()}}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(types.GitHubOwnToken{Token: "github_pat_p", AccountID: 7, Login: "p", ExpiresOn: time.Now().AddDate(0, 0, 5), StoredAt: time.Now()})
	if err := sec.For(e.personP).Put(ctx, types.GitHubOwnTokenSecretName(ghOwnRowID), raw); err != nil {
		t.Fatal(err)
	}
	f.ws = f.workspace(&types.WorkspaceBaseImage{Kind: "custom", Image: "ubuntu:24.04"}, nil)
	if f.drive, err = e.pg.CreateMemberDrive(ctx, types.UserDrive{Name: "Notes", Backend: types.DriveBackendDockerVolume,
		OwnerSubject: e.personP, CreatedBy: e.personP}, 3); err != nil {
		t.Fatal(err)
	}
	return e, f
}

// wantNoQueuedFill asserts a refused fill left no run row and no queue row, and answered 409 with the reason and
// sentence.
func (f *fillPG) wantNoQueuedFill(w *httptest.ResponseRecorder, sentence string) {
	f.t.Helper()
	f.wantReason(w, http.StatusConflict, reasonDriveSeedNoCapacity)
	if !strings.Contains(w.Body.String(), sentence) {
		f.t.Errorf("body = %s, want the sentence %q", w.Body.String(), sentence)
	}
	if runs, queued := f.count(`SELECT count(*) FROM agent_runs`), f.count(`SELECT count(*) FROM run_queue`); runs != 0 || queued != 0 {
		f.t.Errorf("a queued fill left %d runs and %d queue rows", runs, queued)
	}
	if rows := f.rows("drive.seed"); len(rows) != 1 || rows[0].Outcome != "failure" {
		f.t.Errorf("drive.seed rows = %+v, want one refused", rows)
	}
}

// TestDriveSeed_QueuedCreateIsRefused_PG: a fill whose create would wait in a pool's queue is refused 409 before any
// run row or queue row, and writes one refused row: in a capped pool the sentence names the pool, and in an uncapped
// pool whose every runner is full it says every runner is busy.
func TestDriveSeed_QueuedCreateIsRefused_PG(t *testing.T) {
	t.Run("a capped pool", func(t *testing.T) {
		e, f := newFillQueuePG(t)
		pool := e.cappedPool("Busy", 1)
		e.orgDefaults(`{"pool_id":"` + pool.ID.String() + `"}`)
		w := e.call(e.person, http.MethodPost, "/api/v1/me/drives/"+f.drive.ID.String()+"/seed", f.body(f.ws.ID, fillPGRepo, ""))
		f.wantNoQueuedFill(w, driveFillCappedPoolMsg("Busy", "Notes"))
	})
	t.Run("every runner of an uncapped pool is busy", func(t *testing.T) {
		e, f := newFillQueuePG(t)
		pool := e.createPool("Roomy", types.RunnerPoolOrganization)
		e.addExecutors(pool.ID, "alpha")
		pool = e.reread(pool)
		occupyRemote(e.placementEnv, pool, "alpha")
		e.orgDefaults(`{"pool_id":"` + pool.ID.String() + `"}`)
		w := e.call(e.person, http.MethodPost, "/api/v1/me/drives/"+f.drive.ID.String()+"/seed", f.body(f.ws.ID, fillPGRepo, ""))
		f.wantNoQueuedFill(w, driveFillRunnersBusyMsg("Notes"))
	})
}

// TestDriveSeed_PostCreateCheckEndsRun_PG: a created run whose grants are not exactly the own-token git_pat is ended
// KILLED and answered 422 with the run id, audited refused with that run; a kill whose teardown fails answers the
// kill route's 500 with the run id and writes its run.revoke failure row.
func TestDriveSeed_PostCreateCheckEndsRun_PG(t *testing.T) {
	t.Run("the fold dropped the grant", func(t *testing.T) {
		f := newFillPG(t)
		defer func() { driveFillBeforeCreate = func() {} }()
		driveFillBeforeCreate = func() { f.h.srv.cfg.DefaultPolicy.EligibleGrants = nil }
		w := f.fill()
		f.wantReason(w, http.StatusUnprocessableEntity, reasonDriveSeedOwnCredentialRequired)
		var id string
		if err := f.pool.QueryRow(context.Background(), `SELECT id::text FROM agent_runs`).Scan(&id); err != nil || !strings.Contains(w.Body.String(), id) {
			t.Fatalf("run id %q (%v) is not in the answer %s", id, err, w.Body.String())
		}
		if state := f.count(`SELECT count(*) FROM agent_runs WHERE state = 'KILLED'`); state != 1 {
			t.Errorf("killed runs = %d, want the fill's run", state)
		}
		rows := f.rows("drive.seed")
		if len(rows) != 1 || rows[0].Outcome != "failure" || rows[0].RunID == nil || rows[0].RunID.String() != id {
			t.Errorf("drive.seed rows = %+v, want one refused with the run %s", rows, id)
		}
	})
	t.Run("the teardown fails", func(t *testing.T) {
		f := newFillPG(t)
		f.h.srv.cfg.Runner = &failTeardownRunner{fillPGRunner: f.rn}
		defer func() { driveFillBeforeCreate = func() {} }()
		driveFillBeforeCreate = func() { f.h.srv.cfg.DefaultPolicy.EligibleGrants = nil }
		w := f.fill()
		var got struct {
			ID     uuid.UUID `json:"id"`
			State  string    `json:"state"`
			Reason string    `json:"reason"`
		}
		if w.Code != http.StatusInternalServerError || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.State != string(types.RunKilled) || got.ID == uuid.Nil {
			t.Fatalf("fill = %d %s, want the kill route's 500 with the run id and KILLED", w.Code, w.Body.String())
		}
		if n := len(f.rows("run.revoke")); n != 1 {
			t.Errorf("run.revoke rows = %d, want 1", n)
		}
		var data map[string]any
		rows := f.rows("drive.seed")
		if len(rows) != 1 || json.Unmarshal(rows[0].Data, &data) != nil || data["reason"] != "internal" || rows[0].RunID == nil || *rows[0].RunID != got.ID {
			t.Errorf("drive.seed rows = %+v, want one refused, reason internal, with the run", rows)
		}
	})
}

// failTeardownRunner cannot stop or kill a sandbox.
type failTeardownRunner struct{ *fillPGRunner }

func (failTeardownRunner) StopSandbox(context.Context, string) error {
	return errors.New("teardown failed")
}
func (failTeardownRunner) KillSandbox(context.Context, string) error {
	return errors.New("teardown failed")
}
