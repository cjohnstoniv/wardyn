// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_RunSecretFacts_Query reads each fact from the row that records it: grant kinds, run_components rows (an
// org row, a person's row by its delivery modes, a cleared row), a given takeover, an accepted support grant, a
// begun desktop sign-in, the harness task, the dispatch row and the newest credential.mint per grant kind, up to
// the asked time.
func TestPG_RunSecretFacts_Query(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	if _, err := pg.ReadRunSecretFacts(ctx, uuid.New(), "harness login", nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a run that does not exist: %v, want ErrNotFound", err)
	}
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	f, err := pg.ReadRunSecretFacts(ctx, run.ID, "harness login", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.GrantKinds) != 0 || len(f.Components) != 0 || len(f.NewestMints) != 0 || f.TakeoverGiven || f.SupportSessionAccepted ||
		f.SigninBegun || f.HarnessLogin || f.Dispatch != nil || f.Now.IsZero() || !f.RunCreatedAt.Equal(run.CreatedAt.Truncate(time.Microsecond)) {
		t.Fatalf("a plain run = %+v, want no facts", f)
	}
	if f, err = pg.ReadRunSecretFacts(ctx, run.ID, run.Task, nil); err != nil || !f.HarnessLogin {
		t.Fatalf("the run's own task as the harness task = %+v, %v; want HarnessLogin", f, err)
	}

	if _, err := pg.CreateGrant(ctx, types.CredentialGrant{ID: uuid.New(), RunID: run.ID, Spec: types.GrantSpec{Kind: types.GrantEnvSecret}}); err != nil {
		t.Fatal(err)
	}
	gh, err := pg.CreateGrant(ctx, types.CredentialGrant{ID: uuid.New(), RunID: run.ID, Spec: types.GrantSpec{Kind: types.GrantGitHubToken}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO run_components (run_id, ordinal, self_defined, component_id, owner, name, version, definition)
		VALUES ($1, 0, false, NULL, '', 'aws-cli', 1, '{}'),
		       ($1, 1, true, NULL, 'alice', 'mine', 1, '{"secrets":[{"delivery":{"mode":"header"}},{"delivery":{"mode":"file"}}]}'),
		       ($1, 2, true, NULL, NULL, NULL, NULL, NULL)`, run.ID); err != nil {
		t.Fatal(err)
	}
	ap, err := pg.CreateApproval(ctx, types.ApprovalRequest{ID: uuid.New(), RunID: run.ID, Kind: types.ApprovalHumanTakeover,
		State: types.ApprovalPending, RequestedScope: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if f, _ := pg.ReadRunSecretFacts(ctx, run.ID, "harness login", nil); f.TakeoverGiven {
		t.Fatal("a takeover only asked for reads as given")
	}
	if _, _, err := pg.TakeOverApproval(ctx, ap.ID, "developer"); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("cred"))
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := pg.BeginDesktopSignin(ctx, types.DesktopSignin{ID: uuid.New(), RunID: run.ID, Holder: "alice", CredentialHash: sum[:],
		BeganAt: now, ExpiresAt: now.Add(30 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	sg, err := pg.RequestSupportGrant(ctx, store.SupportGrant{RunID: run.ID, Helper: "helper", HelperEmail: "helper@example.test",
		Reason: "r", DoorsAsked: []string{"terminal"}})
	if err != nil {
		t.Fatal(err)
	}
	if f, _ := pg.ReadRunSecretFacts(ctx, run.ID, "harness login", nil); f.SupportSessionAccepted {
		t.Fatal("a support grant only asked for reads as accepted")
	}
	if _, err := pg.AcceptSupportGrant(ctx, run.ID, sg.ID, run.CreatedBy, run.CreatedBy, []string{"terminal"}, 30, 0); err != nil {
		t.Fatal(err)
	}
	if err := pg.RecordRunDispatchSecretFacts(ctx, run.ID, store.DispatchSecretFacts{BedrockResident: true, RecordedJTI: "j1"}); err != nil {
		t.Fatal(err)
	}
	for _, grant := range []string{gh.ID.String(), uuid.NewString()} {
		ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), RunID: &run.ID, ActorType: types.ActorSystem, Actor: "broker",
			Action: "credential.mint", Target: grant, Outcome: "success", Data: json.RawMessage(`{"grant_id":"` + grant + `"}`)}
		if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
			t.Fatal(err)
		}
	}

	f, err = pg.ReadRunSecretFacts(ctx, run.ID, "harness login", nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.GrantKinds["env_secret"] != 1 || f.GrantKinds["github_token"] != 1 || len(f.GrantKinds) != 2 {
		t.Errorf("grant kinds = %v", f.GrantKinds)
	}
	if len(f.Components) != 3 || !f.Components[0].Org || f.Components[0].Cleared ||
		f.Components[1].Org || len(f.Components[1].Deliveries) != 2 || f.Components[1].Deliveries[1] != "file" || !f.Components[2].Cleared {
		t.Errorf("components = %+v", f.Components)
	}
	if !f.TakeoverGiven || !f.SupportSessionAccepted || !f.SigninBegun || f.HarnessLogin {
		t.Errorf("takeover %v support %v signin %v harness %v, want the first three", f.TakeoverGiven, f.SupportSessionAccepted, f.SigninBegun, f.HarnessLogin)
	}
	if f.Dispatch == nil || !f.Dispatch.BedrockResident || f.Dispatch.GitTokenResident || f.Dispatch.RecordedJTI != "j1" {
		t.Errorf("dispatch = %+v", f.Dispatch)
	}
	if _, ok := f.NewestMints["github_token"]; !ok || len(f.NewestMints) != 2 {
		t.Errorf("mints = %v, want github_token and the gone grant's \"\"", f.NewestMints)
	}
	if _, ok := f.NewestMints[""]; !ok {
		t.Errorf("mints = %v, want the mint whose grant row is gone under \"\"", f.NewestMints)
	}
	before := f.Now.Add(-time.Hour)
	if f, err = pg.ReadRunSecretFacts(ctx, run.ID, "harness login", &before); err != nil || len(f.NewestMints) != 0 {
		t.Errorf("mints up to an hour ago = %v, %v; want none", f.NewestMints, err)
	}
}

// TestPG_RunSecretFacts_UpsertOrs: the dispatch write ORs each boolean into the row and replaces the jti; the
// revive write ORs the git fact, keeps the Bedrock fact, replaces the jti, and makes no row for a run without one.
func TestPG_RunSecretFacts_UpsertOrs(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	run := persistRun(t, ctx, pool, newRun(types.RunStarting))
	read := func() *store.DispatchSecretFacts {
		t.Helper()
		f, err := pg.ReadRunSecretFacts(ctx, run.ID, "harness login", nil)
		if err != nil {
			t.Fatal(err)
		}
		return f.Dispatch
	}
	if err := pg.RecordRunDispatchSecretFacts(ctx, run.ID, store.DispatchSecretFacts{BedrockResident: true, RecordedJTI: "j1"}); err != nil {
		t.Fatal(err)
	}
	if err := pg.RecordRunDispatchSecretFacts(ctx, run.ID, store.DispatchSecretFacts{GitTokenResident: true, RecordedJTI: "j2"}); err != nil {
		t.Fatal(err)
	}
	if d := read(); d == nil || !d.BedrockResident || !d.GitTokenResident || d.RecordedJTI != "j2" {
		t.Fatalf("after two dispatch writes = %+v, want both set and jti j2", d)
	}
	if err := pg.RecordRunDispatchSecretFacts(ctx, run.ID, store.DispatchSecretFacts{RecordedJTI: "j3"}); err != nil {
		t.Fatal(err)
	}
	if d := read(); !d.BedrockResident || !d.GitTokenResident || d.RecordedJTI != "j3" {
		t.Fatalf("a write of false = %+v, want both still set and jti j3", d)
	}

	other := persistRun(t, ctx, pool, newRun(types.RunStarting))
	if err := pg.RecordRunDispatchSecretFacts(ctx, other.ID, store.DispatchSecretFacts{BedrockResident: true, RecordedJTI: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := pg.RecordRevivedSecretFacts(ctx, other.ID, false, "b"); err != nil {
		t.Fatal(err)
	}
	f, _ := pg.ReadRunSecretFacts(ctx, other.ID, "harness login", nil)
	if d := f.Dispatch; d == nil || !d.BedrockResident || d.GitTokenResident || d.RecordedJTI != "b" {
		t.Fatalf("a revive with no git token = %+v, want Bedrock kept and jti b", d)
	}
	if err := pg.RecordRevivedSecretFacts(ctx, other.ID, true, "c"); err != nil {
		t.Fatal(err)
	}
	if err := pg.RecordRevivedSecretFacts(ctx, other.ID, false, "d"); err != nil {
		t.Fatal(err)
	}
	f, _ = pg.ReadRunSecretFacts(ctx, other.ID, "harness login", nil)
	if d := f.Dispatch; !d.BedrockResident || !d.GitTokenResident || d.RecordedJTI != "d" {
		t.Fatalf("revives git then none = %+v, want both set and jti d", d)
	}

	bare := persistRun(t, ctx, pool, newRun(types.RunRunning))
	if err := pg.RecordRevivedSecretFacts(ctx, bare.ID, true, "x"); err != nil {
		t.Fatal(err)
	}
	if f, _ := pg.ReadRunSecretFacts(ctx, bare.ID, "harness login", nil); f.Dispatch != nil {
		t.Fatalf("a revive of a run with no row made one: %+v", f.Dispatch)
	}
}
