// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A leaver's self-hosted runners: a suspension refuses their sessions without revoking them, a
// reactivation admits them again, and a purge revokes them and the person's unused registration tokens.

// scimRunnerShape gives an instance what the production runner Hub needs to admit a runner.
func scimRunnerShape(cfg *Config) {
	cfg.RunnerOrgURL = admissionOrgURL
	cfg.RunnerAuditPrepare = func(_ context.Context, ev types.AuditEvent) (types.AuditEvent, error) { return ev, nil }
}

// seedClaimedRunner registers and claims a keyed runner for owner on the env's organisation.
func (e *scimEnv) seedClaimedRunner(owner string) (types.Runner, ed25519.PrivateKey) {
	e.t.Helper()
	ctx := context.Background()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	id := uuid.New()
	rn, err := e.st.CreateRunner(ctx, types.Runner{ID: id, Owner: owner, Name: "laptop", PublicKey: pub, KeyFingerprint: id.String(),
		Version: "0.9.0", OrgURLSHA256: federation.OrgURLSHA256(admissionOrgURL), Limits: roomyRunnerLimits()})
	if err != nil {
		e.t.Fatal(err)
	}
	if rn, err = e.st.ClaimRunner(ctx, rn.ID, owner, rn.KeyFingerprint, time.Now()); err != nil {
		e.t.Fatal(err)
	}
	return rn, key
}

func (e *scimEnv) runnerState(id uuid.UUID) types.RunnerState {
	e.t.Helper()
	rn, err := e.st.GetRunner(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return rn.State
}

func TestSCIMSuspendRefusesTheRunnerUntilReactivated(t *testing.T) {
	const sub, email = "sub-runner", "runner.owner@corp.example"
	e := newSCIMEnv(t, scimRunnerShape)
	e.seedEntra(sub, email, oidPat)
	id := e.postUserID(e.a, oidPat, email, email)
	rn, key := e.seedClaimedRunner(sub)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	f := &admissionFixture{ctx: ctx, pool: e.pool, st: e.st, srv: e.a.srv, rn: rn, key: key}

	e.suspendOn(e.a, email)
	s := f.connect(t, nil)
	<-s.done
	if !errors.Is(s.err, runnerwire.ErrGoaway) || !strings.Contains(s.err.Error(), "runner_owner_inactive") || f.srv.RunnerOnline(rn.ID) {
		t.Fatalf("a deactivated owner's runner connected: %v, online=%v", s.err, f.srv.RunnerOnline(rn.ID))
	}
	if got := e.runnerState(rn.ID); got != types.RunnerClaimed {
		t.Fatalf("suspension left the runner %s, want claimed (refused, not revoked)", got)
	}

	if w := e.patch(e.b, id, patchOf("true")); w.Code != http.StatusOK {
		t.Fatalf("reactivate = %d %s", w.Code, w.Body.String())
	}
	if s := f.connect(t, nil); !f.waitReady(s) {
		t.Fatalf("the reactivated owner's runner was not admitted: %v", s.result())
	}
}

func TestSCIMPurgeRevokesRunnersAndRegistrationTokens(t *testing.T) {
	e := newSCIMEnv(t, scimRunnerShape)
	f := e.seedPurgeSubject(purgeSub, purgeEmail, purgeOID)
	rn, _ := e.seedClaimedRunner(purgeSub)
	other, _ := e.seedClaimedRunner("sub-bystander")
	ctx := context.Background()
	now := time.Now().UTC()
	mint := func(owner string) uuid.UUID {
		tok, err := e.st.MintRunnerRegistrationToken(ctx, newBearer("wdr_"), types.RunnerRegistrationToken{ID: uuid.New(), Owner: owner, MintedBy: "admin",
			CreatedAt: now, ExpiresAt: now.Add(time.Hour), OrgURLSHA256: federation.OrgURLSHA256(admissionOrgURL)})
		if err != nil {
			t.Fatal(err)
		}
		return tok.ID
	}
	mint(purgeSub)
	kept := mint("sub-bystander")

	if w := e.del(e.a, f.id); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s", w.Code, w.Body.String())
	}
	if got := e.runnerState(rn.ID); got != types.RunnerRevoked {
		t.Errorf("the leaver's runner is %s after the purge, want revoked", got)
	}
	if got := e.runnerState(other.ID); got != types.RunnerClaimed {
		t.Errorf("a bystander's runner is %s after the purge, want claimed", got)
	}
	left, err := e.st.ListUnusedRunnerRegistrationTokensPage(ctx, time.Now().UTC(), "", store.Page{})
	if err != nil || len(left) != 1 || left[0].ID != kept {
		t.Errorf("unused registration tokens after the purge = %+v, %v; want only the bystander's", left, err)
	}
	if len(e.rows(e.a, "runner.revoke")) != 1 || len(e.rows(e.a, "runner.token.revoke")) != 1 {
		t.Errorf("revoke rows: runner %d, token %d, want one each", len(e.rows(e.a, "runner.revoke")), len(e.rows(e.a, "runner.token.revoke")))
	}
}
