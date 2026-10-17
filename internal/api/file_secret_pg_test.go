// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// file_secret delivery against a real Postgres and the real secret store
// (whose owner_only read is the production one): the value is the person's
// own, is on the run's masking manifest in the person's key domain before
// anything can read it, is masked by a replica that never saw the dispatch,
// and goes with the person. Guarded by WARDYN_TEST_PG (throwawayPGPool);
// skipped cleanly when unset. Named TestPG_MaskErasure* so the Makefile's
// test-race-pg target runs it under the race detector.

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// personRunningRun persists a RUNNING run with a sandbox, owned by owner and
// NOT operator-owned (l.run's run is the operator's, whose owner_only reads
// the operator namespace).
func (l *maskLab) personRunningRun(owner string) types.AgentRun {
	l.t.Helper()
	now := time.Now().UTC()
	id := uuid.New()
	pg := store.NewPG(l.pool)
	created, err := pg.CreateRun(l.t.Context(), types.AgentRun{
		ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: owner, Agent: "claude-code", Task: "file secret",
		ConfinementClass: types.CC2, State: types.RunRunning, SPIFFEID: "spiffe://wardyn.test/agent-run/" + id.String(), RunnerTarget: "docker",
	})
	if err != nil {
		l.t.Fatalf("create run: %v", err)
	}
	if err := pg.SetSandboxRef(l.t.Context(), id, "sbx-1"); err != nil {
		l.t.Fatal(err)
	}
	created.SandboxRef = "sbx-1"
	return created
}

func TestPG_MaskErasureFileSecretIsOwnMaskedAndGoesWithThePerson(t *testing.T) {
	l := newMaskLab(t)
	fail := false
	a := erasureReplica(l, &fail)
	ctx := t.Context()
	const (
		ownValue   = "person-file-secret-0001\n"
		ownBare    = "person-file-secret-0001"
		operSame   = "operator-same-name-0001"
		operOnly   = "operator-only-value-0001"
		secretName = "deploy-token"
	)
	run := l.personRunningRun(maskOwner)
	if err := a.sec.For(maskOwner).Put(ctx, secretName, []byte(ownValue)); err != nil {
		t.Fatal(err)
	}
	if err := a.sec.Put(ctx, secretName, []byte(operSame)); err != nil {
		t.Fatal(err)
	}
	if err := a.sec.Put(ctx, "operator-only", []byte(operOnly)); err != nil {
		t.Fatal(err)
	}
	policy := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{
		fileSecretGrant("api-token", secretName, true),
		fileSecretGrant("other", "operator-only", true),
	}}

	// What dispatchRun does for this lane: begin, resolve, complete.
	if !a.srv.beginMaskManifest(ctx, run) {
		t.Fatal("the manifest could not begin")
	}
	files, ok := a.srv.resolveFileSecretGrants(ctx, run, policy)
	if !ok {
		t.Fatal("resolve failed the run")
	}
	if !a.srv.completeMaskManifest(ctx, run) {
		t.Fatal("the manifest could not complete")
	}

	// Owner-only on the production store: the person's own row, never the
	// operator's same-name row, and no operator-only row in its place.
	if len(files) != 1 || files[0].Path != runner.ComponentSecretDir+"/api-token" || string(files[0].Content) != ownValue || !files[0].AgentOwned {
		t.Fatalf("files = %+v, want only the person's own value at %s/api-token", files, runner.ComponentSecretDir)
	}
	var reasons []string
	for _, ev := range l.rec.snapshot() {
		if ev.Action == "run.file_secret.resolve" && ev.RunID != nil && *ev.RunID == run.ID {
			reasons = append(reasons, ev.Outcome+":"+string(ev.Data))
			for _, v := range []string{ownBare, operSame, operOnly} {
				if strings.Contains(string(ev.Data), v) {
					t.Errorf("a resolve row carries a value: %s", ev.Data)
				}
			}
		}
	}
	if len(reasons) != 2 || !strings.Contains(reasons[0], `"secret_scope":"own"`) || !strings.Contains(reasons[1], "owner_only") {
		t.Errorf("resolve rows = %q, want the own row read and the operator-only grant refused as owner_only", reasons)
	}

	// On the manifest, in the person's key domain, as stored and without its
	// line break — and nothing else (no operator value was ever read).
	if n := l.count(`SELECT count(*) FROM run_mask_values WHERE run_id=$1 AND owner=$2`, run.ID, maskOwner); n != 2 {
		t.Errorf("%d manifest values for the run under the person, want 2 (raw and trimmed)", n)
	}
	if n := l.count(`SELECT count(*) FROM run_mask_values WHERE run_id=$1 AND owner<>$2`, run.ID, maskOwner); n != 0 {
		t.Errorf("%d manifest values under another owner", n)
	}

	// A wardynd that never saw the dispatch masks both renderings.
	b := l.replica()
	if w := l.upload(b, run, "cat: "+ownValue+"subst: "+ownBare+" end\n"); w.Code != http.StatusNoContent {
		t.Fatalf("upload to another replica: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(string(b.rec.saved), ownBare) {
		t.Errorf("another replica persisted the file's value unmasked: %q", b.rec.saved)
	}

	// The person is erased: their masking copies and their credentials.
	if _, err := store.NewPG(l.pool).CreateAPIToken(ctx, types.APIToken{ID: uuid.New(), Principal: maskOwner, Email: maskOwner, Role: "user", UserType: types.UserTypeStandard, Name: "file secret erasure fixture"}, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if w := do(t, a.srv, http.MethodPost, "/api/v1/people/"+maskOwner+"/erasure", adminToken, erasureBody("mask_copies", "credentials")); w.Code != http.StatusOK {
		t.Fatalf("erase = %d %s", w.Code, w.Body)
	}
	if n := l.count(`SELECT count(*) FROM run_mask_values WHERE run_id=$1`, run.ID); n != 0 {
		t.Errorf("%d of the run's masking values survived the erasure", n)
	}
	if n := l.count(`SELECT count(*) FROM run_mask_manifest WHERE run_id=$1 AND fenced_at IS NULL`, run.ID); n != 0 {
		t.Error("the run's manifest is not fenced after the erasure")
	}
	// The fence: a replica can no longer prove the run's secrets masked, so the
	// output door refuses rather than keep unmasked bytes.
	wantMaskRefusal(t, l.upload(l.replica(), run, "after: "+ownBare+"\n"), "upload after the erasure")
	names, err := a.sec.For(maskOwner).List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(names, secretName) {
		t.Error("the person's own secret survived the credentials erasure")
	}
	if op, err := a.sec.List(ctx); err != nil || !slices.Contains(op, secretName) {
		t.Errorf("the operator's same-name secret went with the person (%v, %v)", op, err)
	}

	// Nothing is left to deliver: a later resolve finds no row of the
	// person's own, and the operator's same-name row does not stand in.
	again, ok := a.srv.resolveFileSecretGrants(ctx, run, policy)
	if !ok || len(again) != 0 {
		t.Errorf("a resolve after the erasure delivered %+v", again)
	}
}
