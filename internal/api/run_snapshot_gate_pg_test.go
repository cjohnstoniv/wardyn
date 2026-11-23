// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretspresent"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// pgSecretsRun is a server over a throwaway database with a run config key, and one Linux VM run whose dispatch row
// and stored proxy config name the same token: a run the gate admits until a case gives it a secret.
func pgSecretsRun(t *testing.T) (*Server, store.PG, types.AgentRun) {
	t.Helper()
	pg := store.NewPG(throwawayPGPool(t))
	cfg := baseTestConfig(newHarness(t), pg)
	cfg.RunConfigKey = make([]byte, kek.DEKSize)
	srv := New(cfg)
	ctx := context.Background()
	id := uuid.New()
	run, err := pg.CreateRun(ctx, types.AgentRun{ID: id, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		CreatedBy: "owner-sub", Agent: "claude-code", Repo: "test/repo", Task: "t", ConfinementClass: types.CC2,
		State: types.RunRunning, SPIFFEID: "spiffe://wardyn.test/agent-run/" + id.String(), RunnerTarget: "docker",
		MachineKind: types.MachineLinuxVM})
	if err != nil {
		t.Fatal(err)
	}
	jti := uuid.NewString()
	if err := pg.RecordRunDispatchSecretFacts(ctx, run.ID, store.DispatchSecretFacts{RecordedJTI: jti}); err != nil {
		t.Fatal(err)
	}
	if err := srv.storeRunProxyConfig(ctx, run.ID, []byte(`{"run_token":"`+testRunToken(t, jti)+`"}`)); err != nil {
		t.Fatal(err)
	}
	if present, why := srv.snapshotSecretsGate(ctx, run.ID, admissionWindow()); present {
		t.Fatalf("a run with no secret = %v, want admitted", why)
	}
	return srv, pg, run
}

// TestSecretsPresent_TakeoverFromApprovalRow is R3 on a real store: an asked takeover is not a secret, and the
// takeover TakeOverApproval records refuses the snapshot.
func TestSecretsPresent_TakeoverFromApprovalRow(t *testing.T) {
	srv, pg, run := pgSecretsRun(t)
	ctx := context.Background()
	ap, err := pg.CreateApproval(ctx, types.ApprovalRequest{ID: uuid.New(), RunID: run.ID, Kind: types.ApprovalHumanTakeover,
		State: types.ApprovalPending, RequestedScope: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if present, why := srv.snapshotSecretsGate(ctx, run.ID, admissionWindow()); present {
		t.Fatalf("a takeover only asked for = %v, want admitted", why)
	}
	if _, _, err := pg.TakeOverApproval(ctx, ap.ID, "developer"); err != nil {
		t.Fatal(err)
	}
	if _, why := srv.snapshotSecretsGate(ctx, run.ID, admissionWindow()); !slices.Equal(why, []secretspresent.Reason{secretspresent.Takeover}) {
		t.Fatalf("a given takeover = %v, want [%s]", why, secretspresent.Takeover)
	}
}

// TestSecretsPresent_SupportSessionFromGrantRow is R13 on a real store: a support grant only asked for is not a
// secret, and an accepted one refuses the snapshot, for good, after it ends too.
func TestSecretsPresent_SupportSessionFromGrantRow(t *testing.T) {
	srv, pg, run := pgSecretsRun(t)
	ctx := context.Background()
	g, err := pg.RequestSupportGrant(ctx, store.SupportGrant{RunID: run.ID, Helper: "helper-sub", HelperEmail: "helper@example.test",
		Reason: "r", DoorsAsked: []string{"terminal"}})
	if err != nil {
		t.Fatal(err)
	}
	if present, why := srv.snapshotSecretsGate(ctx, run.ID, admissionWindow()); present {
		t.Fatalf("a support grant only asked for = %v, want admitted", why)
	}
	if _, err := pg.AcceptSupportGrant(ctx, run.ID, g.ID, run.CreatedBy, run.CreatedBy+"@example.test", []string{"terminal"}, 30, 0); err != nil {
		t.Fatal(err)
	}
	want := []secretspresent.Reason{secretspresent.SupportSession}
	if _, why := srv.snapshotSecretsGate(ctx, run.ID, admissionWindow()); !slices.Equal(why, want) {
		t.Fatalf("an accepted support grant = %v, want %v", why, want)
	}
	if _, err := pg.EndSupportGrant(ctx, run.ID, g.ID, "owner_ended", true); err != nil {
		t.Fatal(err)
	}
	if _, why := srv.snapshotSecretsGate(ctx, run.ID, admissionWindow()); !slices.Equal(why, want) {
		t.Fatalf("an ended support grant = %v, want %v", why, want)
	}
}
