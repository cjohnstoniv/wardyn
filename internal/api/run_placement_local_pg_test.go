// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	secretspg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// localOwnerFixture is a real Postgres server and secret store with the
// operator's namespace and two members' namespaces. The operator-eligible
// grant pairs corp.example with the secret name "corp-secret", which the
// operator and Alice both hold, and Bob does not.
type localOwnerFixture struct {
	h     *harness
	sec   *secretspg.Store
	alice *http.Cookie
	bob   *http.Cookie
}

const localCorpSecret = "corp-secret"

func newLocalOwnerFixture(t *testing.T) *localOwnerFixture {
	t.Helper()
	h, sec := newRunOwnerPGHarness(t)
	h.srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{apiKeyGrantSpec("corp.example", localCorpSecret)}
	h.srv.cfg.DefaultPolicy.AllowedDomains = append(h.srv.cfg.DefaultPolicy.AllowedDomains, "corp.example")
	f := &localOwnerFixture{h: h, sec: sec,
		alice: ssoSession(t, "alice", "alice@corp.example", oidc.RoleUser),
		bob:   ssoSession(t, "bob", "bob@corp.example", oidc.RoleUser)}
	if err := sec.Put(context.Background(), localCorpSecret, []byte("operator-value-0000")); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *localOwnerFixture) putOwn(t *testing.T, who *http.Cookie, name string) {
	t.Helper()
	if w := doSSO(t, f.h.srv, http.MethodPut, "/api/v1/secrets/"+name, who, `{"value":"own-value-0000"}`); w.Code != http.StatusNoContent {
		t.Fatalf("PUT /secrets/%s: %d %s", name, w.Code, w.Body.String())
	}
}

func (f *localOwnerFixture) localRun(t *testing.T, path string, who *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"agent":"claude-code","task":"t","placement":"local","inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com","corp.example"],` +
		`"eligible_grants":[{"kind":"api_key","scope":{"host":"corp.example","secret_name":"` + localCorpSecret + `"}}]}}`
	return doSSO(t, f.h.srv, http.MethodPost, path, who, body)
}

func (f *localOwnerFixture) createRun(t *testing.T, id uuid.UUID, owner string, p types.Placement) types.AgentRun {
	t.Helper()
	run, err := f.h.srv.cfg.Store.CreateRun(context.Background(), types.AgentRun{ID: id, CreatedBy: owner, Agent: "claude-code", Task: "t", State: types.RunPending,
		Placement: p, RunnerTarget: "docker", ConfinementClass: types.CC2, CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func (f *localOwnerFixture) rowsWritten(t *testing.T) (runs, grants int) {
	t.Helper()
	pool := f.h.srv.cfg.Store
	rs, err := pool.ListRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		gs, err := pool.ListGrantsByRun(context.Background(), r.ID)
		if err != nil {
			t.Fatal(err)
		}
		grants += len(gs)
	}
	return len(rs), grants
}

// A member whose own namespace lacks the paired name must be refused for the
// credential, not quietly served the operator's row of that name; the person
// whose own namespace holds the name reaches only the final unsupported-local
// refusal, which is what proves the own classification (and not the operator
// one) was reached. Nothing is written by either.
func TestLocalCreateAgainstRealSecretStoreNeverFallsBackToOperatorNamespace(t *testing.T) {
	f := newLocalOwnerFixture(t)
	f.putOwn(t, f.alice, localCorpSecret)
	for _, door := range []string{"/api/v1/runs", "/api/v1/runs/preflight"} {
		w := f.localRun(t, door, f.bob)
		if w.Code != http.StatusUnprocessableEntity || errorReason(w) != string(placement.ReasonPlacementCredential) {
			t.Fatalf("%s bob (operator name only): %d %s", door, w.Code, w.Body.String())
		}
		w = f.localRun(t, door, f.alice)
		if w.Code != http.StatusUnprocessableEntity || errorReason(w) != string(placement.ReasonPlacementUnavailable) {
			t.Fatalf("%s alice (own row): %d %s", door, w.Code, w.Body.String())
		}
	}
	if runs, grants := f.rowsWritten(t); runs != 0 || grants != 0 {
		t.Fatalf("refused local doors wrote %d runs and %d grants", runs, grants)
	}
}

// A shared grant is Wardyn-authored for an org component and reads the operator's
// row only; a colliding row in the person's own namespace must not turn it own.
func TestLocalSharedAPIKeyGrantIsOperatorEvenWithCollidingOwnName(t *testing.T) {
	f := newLocalOwnerFixture(t)
	f.putOwn(t, f.alice, localCorpSecret)
	shared := types.GrantSpec{Kind: types.GrantAPIKey, Scope: mustJSON(map[string]any{"host": "corp.example", "secret_name": localCorpSecret, "shared": true})}
	origin, ref := f.h.srv.localGrantOrigin(context.Background(), "alice", shared)
	if ref != nil || origin.Class != placement.ClassOperator || origin.OwnNamespace || origin.OwnerOnly {
		t.Fatalf("shared grant classified %+v (%v)", origin, ref)
	}
	own, ref := f.h.srv.localGrantOrigin(context.Background(), "alice", apiKeyGrantSpec("corp.example", localCorpSecret))
	if ref != nil || own.Class != placement.ClassOwn || !own.OwnNamespace || !own.OwnerOnly {
		t.Fatalf("own grant classified %+v (%v)", own, ref)
	}
	// No owner is never an own namespace, whatever rows exist.
	if ghost, ref := f.h.srv.localGrantOrigin(context.Background(), "", apiKeyGrantSpec("corp.example", localCorpSecret)); ref != nil || ghost.OwnNamespace || ghost.OwnerOnly || ghost.Class != placement.ClassOperator {
		t.Fatalf("ownerless grant classified %+v (%v)", ghost, ref)
	}
	plan := placement.LocalPlan{Spec: runner.SandboxSpec{ProxyConfig: runner.ProxyConfig{Policy: types.RunPolicySpec{EligibleGrants: []types.GrantSpec{shared}}}},
		Origins: map[string]placement.CredentialOrigin{"ProxyConfig.Policy.EligibleGrants[0]": origin}}
	if _, r := placement.LocalEligibility(plan); r == nil || r.Reason != placement.ReasonPlacementCredential {
		t.Fatalf("shared grant admitted: %v", r)
	}
}

func localGrantKindsForOwner(secret string) []types.GrantSpec {
	return []types.GrantSpec{
		{Kind: types.GrantAPIKey, Scope: mustJSON(map[string]any{"host": "api.example", "secret_name": secret})},
		{Kind: types.GrantGitPAT, Scope: mustJSON(map[string]any{"host": "git.example", "secret_name": secret})},
		{Kind: types.GrantSSHKey, Scope: mustJSON(map[string]any{"host": "ssh.github.com", "key_secret_ref": secret})},
		{Kind: types.GrantEnvSecret, Scope: mustJSON(map[string]any{"name": "MY_KEY", "secret_name": secret})},
		{Kind: types.GrantFileSecret, Scope: mustJSON(map[string]any{"file": "key", "secret_name": secret})},
	}
}

func localMemberRequest(who string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(operatorCtx(who, who+"@corp.example", oidc.RoleUser))
}

// persistRunGrants is the one writer every create door shares. Against the real
// grant table, every stored kind of a local run lands OwnerOnly (so a later
// resolve cannot fall back to the operator's row of the same name), the whole
// set is checked before the first write, and a member whose namespace lacks the
// name writes nothing.
func TestLocalPersistRunGrantsOnPostgresForcesOwnerOnlyAndWritesNothingOnFallback(t *testing.T) {
	f := newLocalOwnerFixture(t)
	f.putOwn(t, f.alice, "person-secret")
	ctx := context.Background()
	st := f.h.srv.cfg.Store

	spec := types.RunPolicySpec{EligibleGrants: localGrantKindsForOwner("person-secret")}
	runID := uuid.New()
	f.createRun(t, runID, "alice", types.PlacementLocal)
	if _, ok := f.h.srv.persistRunGrants(ctx, httptest.NewRecorder(), localMemberRequest("alice"), runID, time.Now().UTC(), spec, types.PlacementLocal); !ok {
		t.Fatal("alice's own local grants were refused")
	}
	rows, err := st.ListGrantsByRun(ctx, runID)
	if err != nil || len(rows) != len(spec.EligibleGrants) {
		t.Fatalf("persisted %d rows (%v), want %d", len(rows), err, len(spec.EligibleGrants))
	}
	for _, g := range rows {
		if !g.Spec.OwnerOnly {
			t.Errorf("%s was stored without OwnerOnly: the operator's row of the same name could serve it", g.Spec.Kind)
		}
	}

	// The operator's secret exists under the same name; bob owns nothing.
	bobRun := uuid.New()
	f.createRun(t, bobRun, "bob", types.PlacementLocal)
	bobSpec := types.RunPolicySpec{EligibleGrants: append(localGrantKindsForOwner("person-secret")[:2:2], localGrantKindsForOwner(localCorpSecret)[2:]...)}
	rec := httptest.NewRecorder()
	if _, ok := f.h.srv.persistRunGrants(ctx, rec, localMemberRequest("bob"), bobRun, time.Now().UTC(), bobSpec, types.PlacementLocal); ok {
		t.Fatal("bob has no own row of that name but was admitted")
	}
	if got, _ := st.ListGrantsByRun(ctx, bobRun); len(got) != 0 {
		t.Fatalf("a refused set wrote %d grants", len(got))
	}
	for _, g := range bobSpec.EligibleGrants {
		if g.OwnerOnly {
			t.Fatal("the caller's policy was mutated")
		}
	}

	// A remote run is unchanged: no namespace proof, no forced OwnerOnly.
	remoteRun := uuid.New()
	f.createRun(t, remoteRun, "bob", types.PlacementRemote)
	if _, ok := f.h.srv.persistRunGrants(ctx, httptest.NewRecorder(), localMemberRequest("bob"), remoteRun, time.Now().UTC(), bobSpec, types.PlacementRemote); !ok {
		t.Fatal("remote placement must keep its documented behaviour")
	}
	if got, _ := st.ListGrantsByRun(ctx, remoteRun); len(got) != len(bobSpec.EligibleGrants) {
		t.Fatalf("remote run persisted %d", len(got))
	}
	for _, g := range mustListGrants(t, st, remoteRun) {
		if g.Spec.OwnerOnly {
			t.Fatal("remote persistence gained OwnerOnly")
		}
	}
}

func mustListGrants(t *testing.T, st interface {
	ListGrantsByRun(context.Context, uuid.UUID) ([]types.CredentialGrant, error)
}, id uuid.UUID,
) []types.CredentialGrant {
	t.Helper()
	rows, err := st.ListGrantsByRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// The late provider/capture/redirect writer shares the namespace rule: an own
// grant stamps OwnerOnly and Delivery=own before the write; the operator's
// explicit redirect, a foreign run and a missing own row all refuse with
// nothing stored, even when an own row happens to share the operator's name.
func TestLocalLateGrantWriterOnPostgresStampsOwnAndRefusesOperatorProvenance(t *testing.T) {
	f := newLocalOwnerFixture(t)
	f.putOwn(t, f.alice, localCorpSecret)
	ctx := context.Background()
	st := f.h.srv.cfg.Store
	run := f.createRun(t, uuid.New(), "alice", types.PlacementLocal)
	grant := func(secret string) types.CredentialGrant {
		return types.CredentialGrant{ID: uuid.New(), RunID: run.ID, Spec: apiKeyGrantSpec("corp.example", secret)}
	}
	got, err := f.h.srv.createDispatchGrant(ctx, run, grant(localCorpSecret), false)
	if err != nil || !got.Spec.OwnerOnly || got.Delivery != types.GrantDeliveryOwn {
		t.Fatalf("own late grant: %+v %v", got, err)
	}
	if stored := mustListGrants(t, st, run.ID); len(stored) != 1 || !stored[0].Spec.OwnerOnly {
		t.Fatalf("stored %+v", stored)
	}
	if _, err := f.h.srv.createDispatchGrant(ctx, run, grant(localCorpSecret), true); err == nil {
		t.Fatal("operator-held provenance was laundered into an own grant by a colliding name")
	}
	if _, err := f.h.srv.createDispatchGrant(ctx, run, grant("only-the-operator-has-this"), false); err == nil {
		t.Fatal("a name outside the owner's namespace was written")
	}
	foreign := grant(localCorpSecret)
	foreign.RunID = uuid.New()
	if _, err := f.h.srv.createDispatchGrant(ctx, run, foreign, false); err == nil {
		t.Fatal("a grant for another run was written")
	}
	if stored := mustListGrants(t, st, run.ID); len(stored) != 1 {
		t.Fatalf("refused writes left %d rows", len(stored))
	}
	remote := f.createRun(t, uuid.New(), "alice", types.PlacementRemote)
	g := grant("only-the-operator-has-this")
	g.RunID = remote.ID
	if got, err := f.h.srv.createDispatchGrant(ctx, remote, g, true); err != nil || got.Spec.OwnerOnly {
		t.Fatalf("remote writer changed: %+v %v", got, err)
	}
}

// A grant the secret store cannot prove must not appear owned: the namespace
// read is against the real secret rows, both for the person who owns it and for
// the person who does not.
func TestLocalOwnNamespaceProofIsTheRealSecretListNotTheOperatorRow(t *testing.T) {
	f := newLocalOwnerFixture(t)
	f.putOwn(t, f.alice, "alice-only")
	ctx := context.Background()
	for _, tc := range []struct {
		owner, name string
		want        bool
	}{
		{"alice", "alice-only", true},
		{"bob", "alice-only", false},
		{"alice", localCorpSecret, false},
		{"", "alice-only", false},
	} {
		if got := f.h.srv.ownsSecretMemoized(ctx, tc.owner, tc.name); got != tc.want {
			t.Errorf("owns(%q,%q)=%v, want %v", tc.owner, tc.name, got, tc.want)
		}
	}
}

// Direct dispatch is a second door: a lane that never went through create must
// still be refused before the PENDING claim, the masking manifest, any value
// resolution and the substrate, for a local run and for a placement this server
// does not know. A remote run (and one stored before placements existed) is
// untouched and reaches the sandbox.
func TestLocalDispatchRefusesBeforeClaimManifestAndSubstrateOnPostgres(t *testing.T) {
	for _, tc := range []struct {
		placement types.Placement
		refused   bool
	}{
		{types.PlacementLocal, true},
		{types.Placement("future"), true},
		{types.PlacementRemote, false},
		{"", false},
	} {
		t.Run(string(tc.placement), func(t *testing.T) {
			_, srv, probe, audit, run := maskedDispatch(t, maskOwner)
			run.Placement = tc.placement
			dispatchOf(srv, run, types.RunPolicySpec{})
			ev := findAudit(audit.events, run.ID, "run.dispatch", "failure")
			if !tc.refused {
				if !probe.sawCreate || !probe.coveredAtCreate {
					t.Fatalf("placement %q was refused: %s", tc.placement, auditDump(audit.events, run.ID))
				}
				return
			}
			if probe.sawCreate || probe.coveredAtCreate {
				t.Fatal("a refused placement reached the substrate")
			}
			if ev == nil || !strings.Contains(string(ev.Data), string(placement.ReasonPlacementUnavailable)) {
				t.Fatalf("no canonical refusal audited: %s", auditDump(audit.events, run.ID))
			}
			if srv.cfg.MaskManifests.Covered(context.Background(), run.ID) {
				t.Fatal("a masking manifest was opened for a refused placement")
			}
		})
	}
}
