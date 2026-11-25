// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// authorityEnv is the pinned-unit fixture with issuer portals switched on. Its portals, people and grants are
// seeded through the store, the state a successful exchange leaves, so each test drives the authority rules
// and not the exchange.
type authorityEnv struct{ *portalPinEnv }

func newAuthorityEnv(t *testing.T) *authorityEnv {
	t.Helper()
	e := &authorityEnv{newPortalPinEnv(t)}
	if _, err := e.st.ActivatePortalIssuers(context.Background(), "test", nil); err != nil {
		t.Fatalf("switch issuer portals on: %v", err)
	}
	return e
}

func sha256HexOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// bind registers a fresh issuer's binding.
func (e *authorityEnv) bind(t *testing.T) types.PortalIssuer {
	t.Helper()
	b, err := e.st.BindPortalIssuerQ(context.Background(), e.st.Pool, "https://idp-"+uuid.NewString()+".example.com",
		"https://internal-"+uuid.NewString()+".example.com", "uid", "test")
	if err != nil {
		t.Fatalf("bind an issuer: %v", err)
	}
	return b
}

// portal registers an issuer portal on a bound issuer, pinned to pin (nil is unpinned). shape edits the row first.
func (e *authorityEnv) portal(t *testing.T, b types.PortalIssuer, pin *uuid.UUID, shape ...func(*types.Delegate)) types.Delegate {
	t.Helper()
	d := types.Delegate{
		ID: uuid.New(), Name: "portal-" + uuid.NewString(), IdPClientID: uuid.NewString(), RegisteredBy: "test",
		Issuer: b.Issuer, InternalIssuer: b.InternalIssuerNorm, IssuerKey: b.IssuerKey, RunInUnder: pin,
		TokenProfile: "okta_access_token", UserType: types.UserTypeStandard, PortalManagedAdmission: true,
		MaxSubjectTokenAgeSeconds: 600, GrantTTLSeconds: 600,
	}
	for _, f := range shape {
		f(&d)
	}
	out, err := e.st.CreateDelegateQ(context.Background(), e.st.Pool, d, sha256HexOf(store.PortalCredentialPrefix+uuid.NewString()))
	if err != nil {
		t.Fatalf("register an issuer portal: %v", err)
	}
	return out
}

// person issues the identity of person on portal d and mints one grant, returning the principal and the bearer.
func (e *authorityEnv) person(t *testing.T, d types.Delegate, person string) (string, string) {
	t.Helper()
	ctx := context.Background()
	principal := types.PortalPrincipal(d.IssuerKey, person)
	if _, _, err := e.st.IssuePortalIdentity(ctx, principal, d.IssuerKey, "", d.Issuer, d.ID, d.AdmissionRev); err != nil {
		t.Fatalf("issue the identity of %s: %v", person, err)
	}
	raw := delegatedTokenPrefix + uuid.NewString()
	if _, _, err := e.st.MintPortalGrant(ctx, types.PortalGrant{DelegateID: d.ID, Principal: principal, UserType: d.UserType,
		SubjectIssuedAt: time.Now().UTC()}, raw, time.Now().Add(time.Hour), 10*time.Minute, d.AdmissionRev); err != nil {
		t.Fatalf("mint a grant for %s: %v", person, err)
	}
	return principal, raw
}

// run seeds a running run owned by owner, in unit (nil is the Default unit), launched through portal via.
func (e *authorityEnv) run(t *testing.T, owner string, unit *uuid.UUID, via uuid.UUID) types.AgentRun {
	t.Helper()
	id, now := uuid.New(), time.Now().UTC()
	run := types.AgentRun{ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: owner, Agent: "claude-code", Task: "t",
		ConfinementClass: types.CC2, State: types.RunRunning, UnitID: unit, CreatedVia: &via,
		SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(), RunnerTarget: "docker"}
	got, err := e.st.CreateRun(context.Background(), run)
	if err != nil {
		t.Fatalf("seed a run: %v", err)
	}
	return got
}

// runAs is run, then moved to state with its creation and end times set: ended is how long ago it ended (zero
// leaves a live run alone), created is its creation time (zero keeps now).
func (e *authorityEnv) runAs(t *testing.T, owner string, unit *uuid.UUID, via uuid.UUID, state types.RunState, ended time.Duration, created time.Time) types.AgentRun {
	t.Helper()
	r := e.run(t, owner, unit, via)
	if created.IsZero() {
		created = r.CreatedAt
	}
	var endedAt *time.Time
	if ended > 0 {
		at := time.Now().UTC().Add(-ended)
		endedAt = &at
	}
	if _, err := e.st.Pool.Exec(context.Background(), `UPDATE agent_runs SET state = $2, ended_at = $3, created_at = $4 WHERE id = $1`,
		r.ID, string(state), endedAt, created); err != nil {
		t.Fatalf("move a seeded run to %s: %v", state, err)
	}
	r.State, r.CreatedAt = state, created
	return r
}

func (e *authorityEnv) get(t *testing.T, bearer, path string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, e.h.srv, "GET", path, bearer, "")
}
