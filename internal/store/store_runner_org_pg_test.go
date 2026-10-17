// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func newOrgRunner() types.Runner {
	rn := newRunner("")
	rn.OwnerKind, rn.OrgURLSHA256 = types.RunnerOwnerOrg, "org"
	return rn
}

// TestRunnerOrgClaimPG: an org runner is created org_cluster and a person's personal, whatever class the
// caller passes; an org row with an owner is refused. Each claim refuses the other kind; the org claim
// binds the organisation URL and stamps its claimer as the trust writer.
func TestRunnerOrgClaimPG(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	in := newOrgRunner()
	in.TrustClass = types.RunnerTrustPersonal
	org, err := st.CreateRunner(ctx, in)
	if err != nil || org.OwnerKind != types.RunnerOwnerOrg || org.TrustClass != types.RunnerTrustOrgCluster || org.TrustSetBy != "" {
		t.Fatalf("org runner = %+v, %v; want org / org_cluster with no writer yet", org, err)
	}
	in = newRunner("alice")
	in.TrustClass = types.RunnerTrustOrgCluster
	person, err := st.CreateRunner(ctx, in)
	if err != nil || person.OwnerKind != types.RunnerOwnerPerson || person.TrustClass != types.RunnerTrustPersonal {
		t.Fatalf("person runner = %+v, %v; want person / personal", person, err)
	}
	bad := newOrgRunner()
	bad.Owner = "alice"
	if _, err := st.CreateRunner(ctx, bad); err == nil {
		t.Fatal("an org runner with an owner was stored")
	}
	now := time.Now()
	if _, err := st.ClaimRunner(ctx, org.ID, "", org.KeyFingerprint, now); !errors.Is(err, store.ErrRunnerClaimMismatch) {
		t.Fatalf("person claim of an org runner = %v, want ErrRunnerClaimMismatch", err)
	}
	if _, err := st.ClaimOrgRunner(ctx, person.ID, person.KeyFingerprint, person.OrgURLSHA256, "sec-a", now); !errors.Is(err, store.ErrRunnerClaimMismatch) {
		t.Fatalf("org claim of a person's runner = %v, want ErrRunnerClaimMismatch", err)
	}
	if _, err := st.ClaimOrgRunner(ctx, org.ID, org.KeyFingerprint, "other-org", "sec-a", now); !errors.Is(err, store.ErrRunnerClaimMismatch) {
		t.Fatalf("org claim under another organisation URL = %v, want ErrRunnerClaimMismatch", err)
	}
	claimed, err := st.ClaimOrgRunner(ctx, org.ID, org.KeyFingerprint, "org", "sec-a", now)
	if err != nil || claimed.State != types.RunnerClaimed || claimed.TrustClass != types.RunnerTrustOrgCluster || claimed.TrustSetBy != "sec-a" || claimed.TrustSetAt == nil {
		t.Fatalf("org claim = %+v, %v; want claimed org_cluster written by sec-a", claimed, err)
	}
	if _, err := st.ClaimOrgRunner(ctx, org.ID, org.KeyFingerprint, "org", "sec-a", now); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a second org claim = %v, want ErrConflict", err)
	}
}

// TestRunnerTrustWritePG: the trust write is the only writer after the claim, it keeps the coupling, and
// nothing the runner reports moves the class.
func TestRunnerTrustWritePG(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	person, err := st.CreateRunner(ctx, newRunner("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SetRunnerTrustClass(ctx, person.ID, types.RunnerTrustManagedDevice, "sec-a", now); !errors.Is(err, store.ErrRunnerTrustRefused) {
		t.Fatalf("trust write on an unclaimed runner = %v, want ErrRunnerTrustRefused", err)
	}
	if person, err = st.ClaimRunner(ctx, person.ID, "alice", person.KeyFingerprint, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordRunnerPosture(ctx, person.ID, types.RunnerPosture{MDMManaged: true, DiskEncrypted: true, OS: "linux", Evidence: types.PostureEvidenceMDMFile}, now); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetRunner(ctx, person.ID); got.TrustClass != types.RunnerTrustPersonal {
		t.Fatalf("reported posture moved the trust class to %q", got.TrustClass)
	}
	before, after, err := st.SetRunnerTrustClass(ctx, person.ID, types.RunnerTrustManagedDevice, "sec-a", now)
	if err != nil || before.TrustClass != types.RunnerTrustPersonal || after.TrustClass != types.RunnerTrustManagedDevice ||
		after.TrustSetBy != "sec-a" || after.TrustSetAt == nil || !after.TrustSetAt.Equal(now) {
		t.Fatalf("trust write = %q -> %+v, %v", before.TrustClass, after, err)
	}
	if _, _, err := st.SetRunnerTrustClass(ctx, person.ID, types.RunnerTrustOrgCluster, "sec-a", now); !errors.Is(err, store.ErrRunnerTrustRefused) {
		t.Fatalf("org_cluster on a person's runner = %v, want ErrRunnerTrustRefused", err)
	}
	org, err := st.CreateRunner(ctx, newOrgRunner())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimOrgRunner(ctx, org.ID, org.KeyFingerprint, "org", "sec-a", now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SetRunnerTrustClass(ctx, org.ID, types.RunnerTrustPersonal, "sec-a", now); !errors.Is(err, store.ErrRunnerTrustRefused) {
		t.Fatalf("personal on an org runner = %v, want ErrRunnerTrustRefused", err)
	}
	if _, _, err := st.SetRunnerTrustClass(ctx, uuid.New(), types.RunnerTrustPersonal, "sec-a", now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("trust write on no runner = %v, want ErrNotFound", err)
	}
	if got, _ := st.GetRunner(ctx, person.ID); got.TrustClass != types.RunnerTrustManagedDevice {
		t.Fatalf("a refused write changed the class to %q", got.TrustClass)
	}
}

// TestRunnerRegistrationTokenOrgFieldsPG: an org token keeps its owner kind, minter kind and
// break-glass, and a person's token defaults to person, human.
func TestRunnerRegistrationTokenOrgFieldsPG(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	org, err := st.MintRunnerRegistrationToken(ctx, "wdr_org", types.RunnerRegistrationToken{ID: uuid.New(), MintedBy: "admin", OrgURLSHA256: "org",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), OwnerKind: types.RunnerOwnerOrg, MintedByKind: types.RunnerMintedByAdminToken, BreakGlass: true})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetRunnerRegistrationToken(ctx, org.ID)
	if err != nil || got.OwnerKind != types.RunnerOwnerOrg || got.MintedByKind != types.RunnerMintedByAdminToken || !got.BreakGlass || got.Owner != "" {
		t.Fatalf("org token = %+v, %v", got, err)
	}
	person, err := st.MintRunnerRegistrationToken(ctx, "wdr_person", types.RunnerRegistrationToken{ID: uuid.New(), Owner: "alice", MintedBy: "alice",
		MintedByEmail: "alice@example.com", OrgURLSHA256: "org", CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
	if err != nil || person.OwnerKind != types.RunnerOwnerPerson || person.MintedByKind != types.RunnerMintedByHuman || person.MintedByEmail != "alice@example.com" || person.BreakGlass {
		t.Fatalf("person token = %+v, %v", person, err)
	}
	if _, err := st.MintRunnerRegistrationToken(ctx, "wdr_bad", types.RunnerRegistrationToken{ID: uuid.New(), MintedBy: "sec-a", OrgURLSHA256: "org",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), OwnerKind: types.RunnerOwnerOrg, BreakGlass: true}); err == nil {
		t.Fatal("a human's token was stored break-glass")
	}
}

// TestRunnerOrgSessionPathPG: on the runner-session path an org runner (owner empty) records an execution,
// opens an inventory and ingests a spool record for a run anyone created on it; a person's runner still
// refuses another person's run.
func TestRunnerOrgSessionPathPG(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := context.Background()
	org, err := st.CreateRunner(ctx, newOrgRunner())
	if err != nil {
		t.Fatal(err)
	}
	if org, err = st.ClaimOrgRunner(ctx, org.ID, org.KeyFingerprint, "org", "sec-a", time.Now()); err != nil {
		t.Fatal(err)
	}
	person, err := st.CreateRunner(ctx, newRunner("bob"))
	if err != nil {
		t.Fatal(err)
	}
	if person, err = st.ClaimRunner(ctx, person.ID, "bob", person.KeyFingerprint, time.Now()); err != nil {
		t.Fatal(err)
	}
	runOn := func(rn types.Runner) types.AgentRun {
		run := newRun(types.RunRunning)
		run.CreatedBy, run.Placement, run.RunnerID, run.EvidenceSource = "alice", types.PlacementLocal, &rn.ID, types.RunEvidenceRunnerAsserted
		run, err := st.CreateRun(ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	orgRun, personRun := runOn(org), runOn(person)
	if _, err := st.BeginRunnerExecution(ctx, org.ID, orgRun.ID, uuid.Nil, uuid.New(), runnerwire.MethodExec); err != nil {
		t.Fatalf("org runner execution = %v", err)
	}
	if _, err := st.BeginRunnerExecution(ctx, person.ID, personRun.ID, uuid.Nil, uuid.New(), runnerwire.MethodExec); !errors.Is(err, store.ErrRunnerExecutionRefused) {
		t.Fatalf("a person's runner on another person's run = %v, want ErrRunnerExecutionRefused", err)
	}
	if err := st.BeginRunnerInventory(ctx, org.ID, uuid.NewString()); err != nil {
		t.Fatalf("org runner inventory = %v", err)
	}
	spool, err := runnerspool.Open(filepath.Join(t.TempDir(), "spool"), orgRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	if err := spool.Append(ctx, egress.DecisionLog{Request: egress.Request{RunID: orgRun.ID, Host: "allowed.test", Time: time.Now().UTC().Truncate(time.Microsecond)},
		Decision: egress.Allow, RuleSource: "policy:allowed"}); err != nil {
		t.Fatal(err)
	}
	rows, err := spool.After(ctx, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.IngestRunnerDecision(ctx, org.ID, rows[0], prepareSpoolTest); err != nil {
		t.Fatalf("org runner spool ingestion = %v", err)
	}
}
