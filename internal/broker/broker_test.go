// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ---- fakes (no Postgres) -------------------------------------------------

type fakeGrant struct {
	runID uuid.UUID
	spec  types.GrantSpec
}

type fakeApproval struct {
	id        uuid.UUID
	runID     uuid.UUID
	grantID   uuid.UUID
	kind      string
	scope     json.RawMessage
	state     types.ApprovalState
	mintedJTI string
	reason    string
}

// fakeDB models just enough of the grants/approvals tables for the broker's
// queries. It matches on substrings of the SQL the broker issues. The mint
// path is single-threaded per grant in practice, but we guard with a mutex.
type fakeDB struct {
	mu          sync.Mutex
	grants      map[uuid.UUID]*fakeGrant
	approvals   map[uuid.UUID]*fakeApproval
	revokedRuns map[uuid.UUID]bool

	beginErr  error
	commitErr error
}

func newFakeDB() *fakeDB {
	return &fakeDB{
		grants:      map[uuid.UUID]*fakeGrant{},
		approvals:   map[uuid.UUID]*fakeApproval{},
		revokedRuns: map[uuid.UUID]bool{},
	}
}

func (db *fakeDB) Begin(_ context.Context) (Tx, error) {
	if db.beginErr != nil {
		return nil, db.beginErr
	}
	return &fakeTx{db: db}, nil
}

// MintedJTIs implements rowsBeginner for RevokeRun tests.
func (db *fakeDB) MintedJTIs(_ context.Context, runID uuid.UUID) ([]string, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	var out []string
	for _, a := range db.approvals {
		if a.runID == runID && a.kind == "credential" && a.mintedJTI != "" {
			out = append(out, a.mintedJTI)
		}
	}
	return out, nil
}

type fakeTx struct {
	db        *fakeDB
	committed bool
}

func (tx *fakeTx) Commit(_ context.Context) error {
	if tx.db.commitErr != nil {
		return tx.db.commitErr
	}
	tx.committed = true
	return nil
}
func (tx *fakeTx) Rollback(_ context.Context) error { return nil }

func (tx *fakeTx) QueryRow(_ context.Context, sql string, args ...any) Row {
	tx.db.mu.Lock()
	defer tx.db.mu.Unlock()
	switch {
	case strings.Contains(sql, "FROM credential_grants g"):
		// selectGrantApprovalForUpdate: args[0]=grantID, args[1]=approvalHint
		grantID := args[0].(uuid.UUID)
		g := tx.db.grants[grantID]
		if g == nil {
			return errRow{errNoRow}
		}
		var ap *fakeApproval
		for _, a := range tx.db.approvals {
			if a.grantID == grantID && a.kind == "credential" {
				if args[1] == nil || a.id == args[1].(uuid.UUID) {
					ap = a
					break
				}
			}
		}
		return &grantJoinRow{g: g, grantID: grantID, ap: ap}

	case strings.Contains(sql, "FROM credential_grants WHERE id"):
		// loadGrant: args[0]=grantID
		grantID := args[0].(uuid.UUID)
		g := tx.db.grants[grantID]
		if g == nil {
			return errRow{errNoRow}
		}
		return &loadGrantRow{g: g}

	case strings.Contains(sql, "FROM approvals") && strings.Contains(sql, "grant_id = $1"):
		// ensureApproval select: args[0]=grantID
		grantID := args[0].(uuid.UUID)
		for _, a := range tx.db.approvals {
			if a.grantID == grantID && a.kind == "credential" {
				return &ensureApprovalRow{a: a}
			}
		}
		return errRow{errNoRow}

	case strings.Contains(sql, "identity_revocations"):
		// runRevoked: args[0]=runID; EXISTS(...) always returns one bool row.
		runID := args[0].(uuid.UUID)
		return boolRow{v: tx.db.revokedRuns[runID]}
	}
	return errRow{errors.New("fakeTx: unhandled query: " + sql)}
}

func (tx *fakeTx) Exec(_ context.Context, sql string, args ...any) (int64, error) {
	tx.db.mu.Lock()
	defer tx.db.mu.Unlock()
	switch {
	case strings.Contains(sql, "UPDATE approvals SET minted_jti"):
		// args[0]=jti, args[1]=approvalID. Mirror the conditional UPDATE's
		// rows-affected: 1 when this call wins the single-use write, 0 when
		// minted_jti was already set (a concurrent mint won the race).
		jti := args[0].(string)
		apID := args[1].(uuid.UUID)
		if a := tx.db.approvals[apID]; a != nil && a.mintedJTI == "" {
			a.mintedJTI = jti
			return 1, nil
		}
		return 0, nil
	case strings.Contains(sql, "INSERT INTO approvals"):
		// args: id, run_id, grant_id, requested_scope
		id := args[0].(uuid.UUID)
		runID := args[1].(uuid.UUID)
		grantID := args[2].(uuid.UUID)
		scope := args[3].([]byte)
		tx.db.approvals[id] = &fakeApproval{
			id: id, runID: runID, grantID: grantID, kind: "credential",
			scope: json.RawMessage(scope), state: types.ApprovalPending,
		}
		return 1, nil
	}
	return 0, errors.New("fakeTx: unhandled exec: " + sql)
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

type boolRow struct{ v bool }

func (r boolRow) Scan(dest ...any) error {
	*dest[0].(*bool) = r.v
	return nil
}

type grantJoinRow struct {
	g       *fakeGrant
	grantID uuid.UUID
	ap      *fakeApproval
}

// Scan mirrors selectGrantApprovalForUpdate's dest order:
// g.id, g.run_id, g.spec, a.id, a.run_id, a.state, a.requested_scope, a.minted_jti
func (r *grantJoinRow) Scan(dest ...any) error {
	*dest[0].(*uuid.UUID) = r.grantID
	*dest[1].(*uuid.UUID) = r.g.runID
	spec, _ := json.Marshal(r.g.spec)
	*dest[2].(*[]byte) = spec
	if r.ap != nil {
		*dest[3].(**uuid.UUID) = &r.ap.id
		*dest[4].(**uuid.UUID) = &r.ap.runID
		st := string(r.ap.state)
		*dest[5].(**string) = &st
		*dest[6].(*[]byte) = []byte(r.ap.scope)
		mj := r.ap.mintedJTI
		*dest[7].(**string) = &mj
	}
	return nil
}

type loadGrantRow struct{ g *fakeGrant }

func (r *loadGrantRow) Scan(dest ...any) error {
	*dest[0].(*uuid.UUID) = r.g.runID
	spec, _ := json.Marshal(r.g.spec)
	*dest[1].(*[]byte) = spec
	return nil
}

type ensureApprovalRow struct{ a *fakeApproval }

// Scan mirrors ensureApproval's dest: id, state, requested_scope, minted_jti, reason
func (r *ensureApprovalRow) Scan(dest ...any) error {
	*dest[0].(*uuid.UUID) = r.a.id
	*dest[1].(*types.ApprovalState) = r.a.state
	*dest[2].(*json.RawMessage) = r.a.scope
	*dest[3].(*string) = r.a.mintedJTI
	*dest[4].(*string) = r.a.reason
	return nil
}

type fakeAudit struct {
	mu     sync.Mutex
	events []types.AuditEvent
}

func (a *fakeAudit) Record(_ context.Context, ev types.AuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, ev)
	return nil
}
func (a *fakeAudit) byAction(action string) []types.AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []types.AuditEvent
	for _, e := range a.events {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// ---- helpers -------------------------------------------------------------

func githubGrantSpec(t *testing.T, requiresApproval bool) types.GrantSpec {
	t.Helper()
	scope, _ := json.Marshal(githubScope{
		Repos:       []string{"acme/widgets"},
		Permissions: map[string]string{"contents": "write", "pull_requests": "write"},
	})
	return types.GrantSpec{Kind: types.GrantGitHubToken, Scope: scope, RequiresApproval: requiresApproval, TTLSeconds: 600}
}

func newTestBroker(t *testing.T) (*Broker, *fakeDB, *fakeAudit, *FakeGitHubMinter) {
	t.Helper()
	db := newFakeDB()
	au := &fakeAudit{}
	gh := &FakeGitHubMinter{Token: "ghs_fixed"}
	b := New(db, nil, au, nil, gh)
	return b, db, au, gh
}

func seedGrant(db *fakeDB, runID uuid.UUID, spec types.GrantSpec) uuid.UUID {
	gid := uuid.New()
	db.grants[gid] = &fakeGrant{runID: runID, spec: spec}
	return gid
}

func seedApproval(db *fakeDB, runID, grantID uuid.UUID, scope json.RawMessage, state types.ApprovalState) uuid.UUID {
	aid := uuid.New()
	db.approvals[aid] = &fakeApproval{id: aid, runID: runID, grantID: grantID, kind: "credential", scope: scope, state: state}
	return aid
}

func callerFor(runID uuid.UUID) *identity.Claims {
	return &identity.Claims{RunID: runID, SPIFFEID: spiffeForRun(runID)}
}

// ---- tests ---------------------------------------------------------------

func TestMintForGrant_ApprovedHappyPath_WritesJTI(t *testing.T) {
	b, db, au, gh := newTestBroker(t)
	runID := uuid.New()
	spec := githubGrantSpec(t, true)
	gid := seedGrant(db, runID, spec)
	aid := seedApproval(db, runID, gid, spec.Scope, types.ApprovalApproved)

	minted, err := b.MintForGrant(context.Background(), callerFor(runID), gid)
	if err != nil {
		t.Fatalf("MintForGrant: %v", err)
	}
	if minted.Token != "ghs_fixed" {
		t.Fatalf("token = %q, want ghs_fixed", minted.Token)
	}
	if minted.JTI == "" {
		t.Fatal("expected non-empty jti")
	}
	if got := db.approvals[aid].mintedJTI; got != minted.JTI {
		t.Fatalf("minted_jti written = %q, want %q (same-tx write)", got, minted.JTI)
	}
	// Branch confinement namespace recorded.
	wantNS := "wardyn/" + runID.String() + "/*"
	if minted.Metadata["branch_namespace"] != wantNS {
		t.Fatalf("branch_namespace = %q, want %q", minted.Metadata["branch_namespace"], wantNS)
	}
	// Audit: one successful credential.mint with agent attribution.
	mints := au.byAction("credential.mint")
	if len(mints) != 1 || mints[0].Outcome != "success" {
		t.Fatalf("expected 1 successful credential.mint, got %+v", mints)
	}
	if mints[0].ActorType != types.ActorAgent || mints[0].Actor != spiffeForRun(runID) {
		t.Fatalf("audit actor = %s/%s, want agent/%s", mints[0].ActorType, mints[0].Actor, spiffeForRun(runID))
	}
	// github minter received clamped perms including metadata:read.
	if gh.LastPermissions["metadata"] != "read" {
		t.Fatalf("expected metadata:read in clamped perms, got %v", gh.LastPermissions)
	}
}

func TestMintForGrant_PendingApproval(t *testing.T) {
	b, db, _, _ := newTestBroker(t)
	runID := uuid.New()
	spec := githubGrantSpec(t, true)
	gid := seedGrant(db, runID, spec)
	seedApproval(db, runID, gid, spec.Scope, types.ApprovalPending)

	_, err := b.MintForGrant(context.Background(), callerFor(runID), gid)
	var pend ErrApprovalPending
	if !errors.As(err, &pend) {
		t.Fatalf("want ErrApprovalPending, got %v", err)
	}
	if pend.ApprovalID == uuid.Nil {
		t.Fatal("ErrApprovalPending missing approval id")
	}
}

func TestMintForGrant_NoApprovalYet_CreatesPending(t *testing.T) {
	b, db, _, _ := newTestBroker(t)
	runID := uuid.New()
	spec := githubGrantSpec(t, true)
	gid := seedGrant(db, runID, spec)

	_, err := b.MintForGrant(context.Background(), callerFor(runID), gid)
	var pend ErrApprovalPending
	if !errors.As(err, &pend) {
		t.Fatalf("want ErrApprovalPending, got %v", err)
	}
	// A PENDING approval should now exist with requested_scope == grant scope.
	if len(db.approvals) != 1 {
		t.Fatalf("expected 1 created approval, got %d", len(db.approvals))
	}
	for _, a := range db.approvals {
		if a.state != types.ApprovalPending {
			t.Fatalf("created approval state = %s, want PENDING", a.state)
		}
		if !jsonScopeEqual(a.scope, spec.Scope) {
			t.Fatalf("created approval scope != grant scope")
		}
	}
}

func TestMintForGrant_Denied(t *testing.T) {
	b, db, _, _ := newTestBroker(t)
	runID := uuid.New()
	spec := githubGrantSpec(t, true)
	gid := seedGrant(db, runID, spec)
	aid := seedApproval(db, runID, gid, spec.Scope, types.ApprovalDenied)
	db.approvals[aid].reason = "policy"

	_, err := b.MintForGrant(context.Background(), callerFor(runID), gid)
	var denied ErrApprovalDenied
	if !errors.As(err, &denied) {
		t.Fatalf("want ErrApprovalDenied, got %v", err)
	}
}

func TestMintForGrant_ScopeMismatch(t *testing.T) {
	b, db, au, _ := newTestBroker(t)
	runID := uuid.New()
	spec := githubGrantSpec(t, true)
	gid := seedGrant(db, runID, spec)
	// Approval scope differs from grant scope: simulate scope widening attempt.
	widened, _ := json.Marshal(githubScope{
		Repos:       []string{"acme/widgets", "acme/secrets"},
		Permissions: map[string]string{"contents": "write"},
	})
	seedApproval(db, runID, gid, widened, types.ApprovalApproved)

	_, err := b.MintForGrant(context.Background(), callerFor(runID), gid)
	if !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("want ErrScopeMismatch, got %v", err)
	}
	// A denied mint audit event should have been emitted.
	mints := au.byAction("credential.mint")
	if len(mints) != 1 || mints[0].Outcome != "denied" {
		t.Fatalf("expected 1 denied credential.mint, got %+v", mints)
	}
}

func TestMintForGrant_DoubleMintBlocked(t *testing.T) {
	b, db, _, gh := newTestBroker(t)
	runID := uuid.New()
	spec := githubGrantSpec(t, true)
	gid := seedGrant(db, runID, spec)
	seedApproval(db, runID, gid, spec.Scope, types.ApprovalApproved)

	if _, err := b.MintForGrant(context.Background(), callerFor(runID), gid); err != nil {
		t.Fatalf("first mint: %v", err)
	}
	_, err := b.MintForGrant(context.Background(), callerFor(runID), gid)
	if !errors.Is(err, ErrAlreadyMinted) {
		t.Fatalf("second mint: want ErrAlreadyMinted, got %v", err)
	}
	if gh.Calls != 1 {
		t.Fatalf("github minter called %d times, want 1 (single-use)", gh.Calls)
	}
}

func TestMintForGrant_CloudSTS_RequiresSPIRE(t *testing.T) {
	b, db, au, _ := newTestBroker(t)
	runID := uuid.New()
	spec := types.GrantSpec{Kind: types.GrantCloudSTS, Scope: json.RawMessage(`{"role":"x"}`), RequiresApproval: false}
	gid := seedGrant(db, runID, spec)

	_, err := b.MintForGrant(context.Background(), callerFor(runID), gid)
	if !errors.Is(err, ErrRequiresSPIRE) {
		t.Fatalf("want ErrRequiresSPIRE, got %v", err)
	}
	mints := au.byAction("credential.mint")
	if len(mints) != 1 || mints[0].Outcome != "denied" {
		t.Fatalf("expected 1 denied credential.mint for cloud_sts, got %+v", mints)
	}
}

// TestMintForGrant_RevokedRun_FailsClosed proves the kill-switch/mint coupling:
// once the run is in identity_revocations (RevokeRun committed), the in-tx
// revocation check refuses the mint with ErrRunRevoked, no token is minted, and
// a denied credential.mint is audited — even for an otherwise-APPROVED grant.
func TestMintForGrant_RevokedRun_FailsClosed(t *testing.T) {
	b, db, au, gh := newTestBroker(t)
	runID := uuid.New()
	spec := githubGrantSpec(t, true)
	gid := seedGrant(db, runID, spec)
	seedApproval(db, runID, gid, spec.Scope, types.ApprovalApproved)
	db.revokedRuns[runID] = true // kill-switch cascade recorded the revocation

	_, err := b.MintForGrant(context.Background(), callerFor(runID), gid)
	if !errors.Is(err, ErrRunRevoked) {
		t.Fatalf("want ErrRunRevoked, got %v", err)
	}
	if gh.Calls != 0 {
		t.Fatalf("github minter called %d times, want 0 (revoked run must not mint)", gh.Calls)
	}
	mints := au.byAction("credential.mint")
	if len(mints) != 1 || mints[0].Outcome != "denied" {
		t.Fatalf("expected 1 denied credential.mint, got %+v", mints)
	}
}

func TestMintForGrant_RunMismatch(t *testing.T) {
	b, db, _, _ := newTestBroker(t)
	ownerRun := uuid.New()
	spec := githubGrantSpec(t, true)
	gid := seedGrant(db, ownerRun, spec)

	other := uuid.New()
	_, err := b.MintForGrant(context.Background(), callerFor(other), gid)
	if !errors.Is(err, ErrRunMismatch) {
		t.Fatalf("want ErrRunMismatch, got %v", err)
	}
}

func TestMintForGrant_AutoMint_APIKey_InjectionRuleNoSecret(t *testing.T) {
	b, db, au, _ := newTestBroker(t)
	runID := uuid.New()
	scope, _ := json.Marshal(apiKeyScope{Host: "api.example.com", Header: "Authorization", Format: "Bearer %s", SecretName: "example-key"})
	spec := types.GrantSpec{Kind: types.GrantAPIKey, Scope: scope, RequiresApproval: false}
	gid := seedGrant(db, runID, spec)

	minted, err := b.MintForGrant(context.Background(), callerFor(runID), gid)
	if err != nil {
		t.Fatalf("auto-mint api_key: %v", err)
	}
	if minted.Token != "" {
		t.Fatal("api_key mint must NOT return a token (secret never leaves broker)")
	}
	if minted.Injection == nil || minted.Injection.SecretName != "example-key" {
		t.Fatalf("expected injection rule referencing secret name, got %+v", minted.Injection)
	}
	if minted.Injection.Host != "api.example.com" {
		t.Fatalf("injection host = %q", minted.Injection.Host)
	}
	mints := au.byAction("credential.mint")
	if len(mints) != 1 || mints[0].Outcome != "success" {
		t.Fatalf("expected 1 successful credential.mint, got %+v", mints)
	}
}

func TestMintOnApproval_HappyPath(t *testing.T) {
	b, db, _, _ := newTestBroker(t)
	runID := uuid.New()
	spec := githubGrantSpec(t, true)
	gid := seedGrant(db, runID, spec)
	aid := seedApproval(db, runID, gid, spec.Scope, types.ApprovalApproved)

	minted, err := b.MintOnApproval(context.Background(), runID, gid)
	if err != nil {
		t.Fatalf("MintOnApproval: %v", err)
	}
	if db.approvals[aid].mintedJTI != minted.JTI {
		t.Fatal("MintOnApproval did not write minted_jti in tx")
	}
}

// TestMintOnApproval_RequiresApproval_NoApprovalRow_FailsClosed proves the
// in-tx chokepoint self-check: MintOnApproval (Nil approval hint) on a grant
// whose spec RequiresApproval but which has NO approval row must NOT auto-mint —
// it fails closed with ErrNotApproved and never calls the minter. Without the
// grantSpec.RequiresApproval guard the mint's `if row.hasApproval` block is
// skipped and the credential is minted with no approval ever seen.
func TestMintOnApproval_RequiresApproval_NoApprovalRow_FailsClosed(t *testing.T) {
	b, db, au, gh := newTestBroker(t)
	runID := uuid.New()
	spec := githubGrantSpec(t, true) // RequiresApproval = true
	gid := seedGrant(db, runID, spec)
	// Deliberately seed NO approval row.

	_, err := b.MintOnApproval(context.Background(), runID, gid)
	if !errors.Is(err, ErrNotApproved) {
		t.Fatalf("want ErrNotApproved, got %v", err)
	}
	if gh.Calls != 0 {
		t.Fatalf("github minter called %d times, want 0 (no approval must not mint)", gh.Calls)
	}
	mints := au.byAction("credential.mint")
	if len(mints) != 1 || mints[0].Outcome != "denied" {
		t.Fatalf("expected 1 denied credential.mint, got %+v", mints)
	}
}

func TestRevokeRun_EmitsRevokeAudit(t *testing.T) {
	b, db, au, _ := newTestBroker(t)
	runID := uuid.New()
	spec := githubGrantSpec(t, true)
	gid := seedGrant(db, runID, spec)
	seedApproval(db, runID, gid, spec.Scope, types.ApprovalApproved)
	if _, err := b.MintForGrant(context.Background(), callerFor(runID), gid); err != nil {
		t.Fatalf("mint: %v", err)
	}

	if err := b.RevokeRun(context.Background(), runID); err != nil {
		t.Fatalf("RevokeRun: %v", err)
	}
	revokes := au.byAction("credential.revoke")
	if len(revokes) != 1 {
		t.Fatalf("expected 1 credential.revoke, got %d", len(revokes))
	}
	if revokes[0].ActorType != types.ActorSystem {
		t.Fatalf("revoke actor_type = %s, want system", revokes[0].ActorType)
	}
}

func TestClampGitHubPermissions(t *testing.T) {
	// Requesting admin contents is clamped to write; out-of-ceiling perms dropped.
	got := clampGitHubPermissions(map[string]string{
		"contents":       "admin",
		"pull_requests":  "write",
		"administration": "write", // outside ceiling -> dropped
		"workflows":      "write", // outside ceiling -> dropped
	})
	want := map[string]string{"contents": "write", "pull_requests": "write", "metadata": "read"}
	if len(got) != len(want) {
		t.Fatalf("clamped perms = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("clamped[%s] = %q, want %q", k, got[k], v)
		}
	}
}

func TestTTLFor_ClampsToMax(t *testing.T) {
	if d := ttlFor(types.GrantSpec{TTLSeconds: 0}); d != defaultMaxTTL {
		t.Fatalf("zero ttl -> %v, want %v", d, defaultMaxTTL)
	}
	if d := ttlFor(types.GrantSpec{TTLSeconds: 99999}); d != defaultMaxTTL {
		t.Fatalf("over-max ttl -> %v, want clamp %v", d, defaultMaxTTL)
	}
	if d := ttlFor(types.GrantSpec{TTLSeconds: 300}); d != 5*time.Minute {
		t.Fatalf("narrow ttl -> %v, want 5m", d)
	}
}

func TestJSONScopeEqual_OrderInsensitive(t *testing.T) {
	a := json.RawMessage(`{"repos":["a","b"],"permissions":{"contents":"write","pull_requests":"write"}}`)
	b := json.RawMessage(`{"permissions":{"pull_requests":"write","contents":"write"},"repos":["a","b"]}`)
	if !jsonScopeEqual(a, b) {
		t.Fatal("expected key-order-insensitive equality")
	}
	c := json.RawMessage(`{"repos":["a"],"permissions":{"contents":"write"}}`)
	if jsonScopeEqual(a, c) {
		t.Fatal("different scopes must not be equal")
	}
}
