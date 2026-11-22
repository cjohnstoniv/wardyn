// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// issuerPGStore is the UI harness's run, ticket and policy store with every issuer-portal read on Postgres:
// the portal row, its grants and chains, the person's identity rows, the unit tree and chains, and the Closed set.
type issuerPGStore struct {
	*uiMemStore
	pg store.PG
	// The console portals' legacy table on Postgres, which delegationLive reads for a grant portal_grants no
	// longer answers (ended, or its portal revoked).
	store.DelegateStore
}

var (
	_ store.PortalGrantStore     = issuerPGStore{}
	_ store.UIDesktopClosedStore = issuerPGStore{}
)

func (s issuerPGStore) GetDelegateByID(ctx context.Context, id uuid.UUID) (types.Delegate, error) {
	return s.pg.GetDelegateByID(ctx, id)
}
func (s issuerPGStore) GetIssuerDelegateByRaw(ctx context.Context, raw string) (types.Delegate, error) {
	return s.pg.GetIssuerDelegateByRaw(ctx, raw)
}
func (s issuerPGStore) MintPortalGrant(ctx context.Context, g types.PortalGrant, raw string, end time.Time, ttl time.Duration, rev int64) (types.PortalGrant, time.Time, error) {
	return s.pg.MintPortalGrant(ctx, g, raw, end, ttl, rev)
}
func (s issuerPGStore) GetPortalGrantByRaw(ctx context.Context, raw string, now time.Time) (types.PortalGrant, error) {
	return s.pg.GetPortalGrantByRaw(ctx, raw, now)
}
func (s issuerPGStore) GetPortalGrantByID(ctx context.Context, id uuid.UUID, now time.Time) (types.PortalGrant, error) {
	return s.pg.GetPortalGrantByID(ctx, id, now)
}
func (s issuerPGStore) PortalChainLive(ctx context.Context, d uuid.UUID, principal string, chain uuid.UUID) (time.Time, bool, error) {
	return s.pg.PortalChainLive(ctx, d, principal, chain)
}
func (s issuerPGStore) StampPortalUsed(ctx context.Context, id uuid.UUID, at time.Time) error {
	return s.pg.StampPortalUsed(ctx, id, at)
}
func (s issuerPGStore) SweepExpiredPortalGrants(ctx context.Context, at time.Time) (int64, error) {
	return s.pg.SweepExpiredPortalGrants(ctx, at)
}
func (s issuerPGStore) IdentityBlocked(ctx context.Context, principal string, epoch int64) (bool, error) {
	return s.pg.IdentityBlocked(ctx, principal, epoch)
}
func (s issuerPGStore) ListGovernanceUnits(ctx context.Context) ([]types.GovernanceUnit, error) {
	return s.pg.ListGovernanceUnits(ctx)
}
func (s issuerPGStore) GetGovernanceUnitChain(ctx context.Context, id uuid.UUID) ([]types.GovernanceUnit, error) {
	return s.pg.GetGovernanceUnitChain(ctx, id)
}

// ListGovernanceMemberships is the units' memberships on Postgres, beside the units above: a thaw re-checks
// that a membership on the run's unit could still match its owner (ownerMembershipRefusal).
func (s issuerPGStore) ListGovernanceMemberships(ctx context.Context) ([]types.GovernanceMembership, error) {
	return s.pg.ListGovernanceMemberships(ctx)
}
func (s issuerPGStore) CloseUIDesktop(ctx context.Context, key string, at time.Time) error {
	return s.pg.CloseUIDesktop(ctx, key, at)
}
func (s issuerPGStore) UIDesktopClosed(ctx context.Context, key string, now time.Time) (bool, error) {
	return s.pg.UIDesktopClosed(ctx, key, now)
}
func (s issuerPGStore) PruneUIDesktopClosed(ctx context.Context, now time.Time) (int, error) {
	return s.pg.PruneUIDesktopClosed(ctx, now)
}

// issuerPGRig is an issuer portal pinned to the platform unit on Postgres, one of its people with a live
// grant and a membership naming them on the platform team, and that person's run there, served by the UI
// harness.
type issuerPGRig struct {
	*issuerUIRig
	env   *authorityEnv
	d     types.Delegate
	grant types.PortalGrant
}

// personMember seeds a membership naming principal on the platform team, as an issuer-portal person enters a
// unit: their groups are stored empty, so the team's group row never places them there.
func personMember(t *testing.T, e *authorityEnv, principal string) types.GovernanceMembership {
	t.Helper()
	m, err := e.st.UpsertGovernanceMembership(context.Background(), types.GovernanceMembership{
		ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: principal, UnitID: e.team.ID, Priority: 1})
	if err != nil {
		t.Fatalf("seed the person's membership: %v", err)
	}
	return m
}

func newIssuerPGRig(t *testing.T, backend http.Handler, shape ...func(*types.Delegate)) *issuerPGRig {
	t.Helper()
	e := newAuthorityEnv(t)
	d := e.portal(t, e.bind(t), &e.plat.ID, shape...)
	principal, raw := e.person(t, d, "person-1")
	personMember(t, e, principal)
	g, err := e.st.GetPortalGrantByRaw(context.Background(), raw, time.Now())
	if err != nil {
		t.Fatalf("read the seeded grant: %v", err)
	}
	h := newUIHarness(t, backend)
	h.srv.cfg.Store = issuerPGStore{uiMemStore: h.store, pg: e.st, DelegateStore: e.st}
	h.srv.cfg.SessionRevocations = &memRevocations{cut: map[string]time.Time{}}
	h.srv.cfg.Now = time.Now // the grants' ends are on the database clock: these tests run on the real one
	h.owner, h.run.CreatedBy, h.run.UnitID = principal, principal, &e.team.ID
	h.store.putRun(h.run)
	return &issuerPGRig{issuerUIRig: &issuerUIRig{uiHarness: h, principal: principal}, env: e, d: d, grant: g}
}

// suspend marks the person deactivated, as a suspend does, leaving every grant live.
func (rig *issuerPGRig) suspend(t *testing.T) {
	t.Helper()
	if _, err := rig.env.st.Pool.Exec(context.Background(), `UPDATE principal_identities SET deactivated_at = now() WHERE principal = $1`, rig.principal); err != nil {
		t.Fatalf("suspend: %v", err)
	}
}

// tombstone marks the person purged and leaves the grant live and the rest of the purge undone.
func (rig *issuerPGRig) tombstone(t *testing.T) {
	t.Helper()
	tombstonePerson(t, rig.env.st, rig.principal)
	if _, err := rig.env.st.GetPortalGrantByID(context.Background(), rig.grant.ID, time.Now()); err != nil {
		t.Fatalf("the grant did not stay live across the tombstone: %v", err)
	}
}

// tombstonePerson writes principal's tombstone as the portal lifecycle does (MarkPortalIdentityPurged): under
// the lifecycle marker the identity trigger requires, display email blanked, in one transaction. It leaves the
// person's grants and the purge's later steps alone, and the database guard as it is.
func tombstonePerson(t *testing.T, st store.PG, principal string) {
	t.Helper()
	ctx := context.Background()
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('wardyn.portal_lifecycle', 'v1', true)`); err != nil {
		t.Fatalf("mark the lifecycle: %v", err)
	}
	if tag, err := tx.Exec(ctx, `UPDATE principal_identities SET purged_at = clock_timestamp(), display_email = '' WHERE principal = $1`, principal); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("tombstone: %v (%d rows)", err, tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the tombstone: %v", err)
	}
}

func (rig *issuerPGRig) revokePortal(t *testing.T) {
	t.Helper()
	if _, _, err := rig.env.st.RevokeDelegate(context.Background(), rig.d.ID, time.Now(), false); err != nil {
		t.Fatalf("revoke the portal: %v", err)
	}
}

// moveOut re-parents the run's unit out of the portal's pin.
func (rig *issuerPGRig) moveOut(t *testing.T) {
	t.Helper()
	team := rig.env.team
	team.ParentID = &rig.env.other.ID
	if _, err := rig.env.st.UpsertGovernanceUnit(context.Background(), team); err != nil {
		t.Fatalf("move the run's unit out of the pin: %v", err)
	}
}

// desktopCookie is a desktop session of the person on the run under the rig's grant, as an enter signs it.
func (rig *issuerPGRig) desktopCookie() *http.Cookie {
	now := rig.clock.now()
	sess := uiSession{Run: rig.run.ID, App: uiDesktopAppName, Desktop: true, Principal: rig.principal, Role: oidc.RoleUser,
		Expires: now.Add(time.Hour).Unix(), IssuedAt: now.Unix(), AuthorizedAt: now.Unix(),
		Via: &types.DelegationVia{Delegate: rig.d.ID, Grant: rig.grant.ID}}
	return &http.Cookie{Name: uiCookieName, Value: rig.srv.encodeUISession(sess)}
}

func (rig *issuerPGRig) desktopOpen(c *http.Cookie) int {
	return uiGet(rig.uiHarness, uiRelayPrefix(rig.run.ID, uiDesktopAppName)+"/", c).Code
}

// TestUIIssuerPortal_WarmCacheNextRequestRefused_PG: a request passes, which would warm the pass cache; then
// the person is suspended, the portal revoked or the run's unit moved out of the pin, and the very next
// request is refused, on an HTTP relay and on a desktop open.
func TestUIIssuerPortal_WarmCacheNextRequestRefused_PG(t *testing.T) {
	events := map[string]func(*issuerPGRig, *testing.T){
		"suspend":       (*issuerPGRig).suspend,
		"portal revoke": (*issuerPGRig).revokePortal,
		"unit move":     (*issuerPGRig).moveOut,
	}
	for name, event := range events {
		t.Run(name, func(t *testing.T) {
			rig := newIssuerPGRig(t, closingBackend("app"))
			c := rig.session(rig.grant)
			desk := rig.desktopCookie()
			if r := rig.relay(c); r.Code != http.StatusOK {
				t.Fatalf("the first request: %d %s", r.Code, r.Body.String())
			}
			if code := rig.desktopOpen(desk); code == http.StatusForbidden || code == http.StatusServiceUnavailable {
				t.Fatalf("the first desktop open was refused: %d", code)
			}
			event(rig, t)
			if r := rig.relay(c); r.Code != http.StatusForbidden {
				t.Errorf("the next relay request after the %s: %d %s, want 403", name, r.Code, r.Body.String())
			}
			if code := rig.desktopOpen(desk); code != http.StatusForbidden {
				t.Errorf("the next desktop open after the %s: %d, want 403", name, code)
			}
		})
	}
}

func renews(d *types.Delegate) { d.UISessionRenewal = true }

// successor mints the person a further grant through the portal, which joins the chain of a live grant.
func (rig *issuerPGRig) successor(t *testing.T) types.PortalGrant {
	t.Helper()
	raw := delegatedTokenPrefix + uuid.NewString()
	g, _, err := rig.env.st.MintPortalGrant(context.Background(), types.PortalGrant{DelegateID: rig.d.ID, Principal: rig.principal,
		UserType: rig.d.UserType, SubjectIssuedAt: time.Now().UTC()}, raw, time.Now().Add(time.Hour), 10*time.Minute, rig.d.AdmissionRev)
	if err != nil {
		t.Fatalf("mint a successor: %v", err)
	}
	return g
}

// endIn moves grant g's end to d from now on the database clock (negative: already ended).
func (rig *issuerPGRig) endIn(t *testing.T, g types.PortalGrant, d time.Duration) time.Time {
	t.Helper()
	var end time.Time
	if err := rig.env.st.Pool.QueryRow(context.Background(), `UPDATE portal_grants SET expires_at = clock_timestamp() + $2 * interval '1 millisecond'
		WHERE id = $1 RETURNING expires_at`, g.ID, d.Milliseconds()).Scan(&end); err != nil {
		t.Fatalf("move a grant's end: %v", err)
	}
	return end
}

// lateSuccessor writes a grant of chain's that a mint read live and committed after that grant's end.
func (rig *issuerPGRig) lateSuccessor(t *testing.T, chain uuid.UUID) {
	t.Helper()
	if _, err := rig.env.st.Pool.Exec(context.Background(), `INSERT INTO portal_grants (id, delegate_id, token_sha256, principal, user_type, chain_id, subject_issued_at, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, now(), now(), now() + interval '10 minutes')`,
		uuid.New(), rig.d.ID, sha256HexOf(uuid.NewString()), rig.principal, rig.d.UserType, chain); err != nil {
		t.Fatalf("write a late successor: %v", err)
	}
}

// TestUITicket_RedeemedOnlyAgainstMintingGrant_PG: a ticket minted under grant A is refused once A has ended,
// though grant B of the same chain lives; a ticket minted under B is redeemed. Renewal on and off.
func TestUITicket_RedeemedOnlyAgainstMintingGrant_PG(t *testing.T) {
	for _, renewal := range []bool{false, true} {
		shape := func(d *types.Delegate) { d.UISessionRenewal = renewal }
		rig := newIssuerPGRig(t, closingBackend("app"), shape)
		b := rig.successor(t)
		if b.ChainID != rig.grant.ChainID {
			t.Fatalf("the successor started chain %s, not its predecessor's %s", b.ChainID, rig.grant.ChainID)
		}
		underA := rig.ticket(rig.grant)
		rig.endIn(t, rig.grant, -time.Second)
		if rec := rig.enterWith(underA); rec.Code != http.StatusForbidden || len(setUICookies(rec)) != 0 {
			t.Errorf("renewal=%v: a ticket of an ended grant, its chain live: %d, want the bad-ticket 403", renewal, rec.Code)
		}
		if rec := rig.enterWith(rig.ticket(b)); rec.Code != http.StatusFound {
			t.Errorf("renewal=%v: a ticket of the live grant: %d %s", renewal, rec.Code, rec.Body.String())
		}
	}
}

// TestUIRenewal_SuccessorCommitOrderForNewRequests_PG: a successor that committed before its predecessor's
// end serves the session's requests throughout; one that committed after it leaves a gap in which a request
// is refused, and serves after its commit. No request is served past the cookie's signed expiry.
func TestUIRenewal_SuccessorCommitOrderForNewRequests_PG(t *testing.T) {
	t.Run("committed before the end", func(t *testing.T) {
		rig := newIssuerPGRig(t, closingBackend("app"), renews)
		rig.srv.cfg.UISessionTTL = 6 * time.Second
		end := rig.endIn(t, rig.grant, 2*time.Second)
		c := rig.session(rig.grant)
		rig.successor(t)
		time.Sleep(time.Until(end) + 500*time.Millisecond)
		if r := rig.relay(c); r.Code != http.StatusOK {
			t.Fatalf("past the predecessor's end, its successor committed before it: %d", r.Code)
		}
		time.Sleep(time.Until(time.Unix(c.Expires.Unix(), 0)) + time.Second)
		if r := rig.relay(c); r.Code != http.StatusForbidden {
			t.Fatalf("past the cookie's signed expiry with its chain live: %d, want 403", r.Code)
		}
	})
	t.Run("committed after the end", func(t *testing.T) {
		rig := newIssuerPGRig(t, closingBackend("app"), renews)
		end := rig.endIn(t, rig.grant, 2*time.Second)
		c := rig.session(rig.grant)
		time.Sleep(time.Until(end) + 500*time.Millisecond)
		if r := rig.relay(c); r.Code != http.StatusForbidden {
			t.Fatalf("in the gap before the late successor's commit: %d, want 403", r.Code)
		}
		rig.lateSuccessor(t, rig.grant.ChainID)
		if r := rig.relay(c); r.Code != http.StatusOK {
			t.Fatalf("after the late successor's commit: %d", r.Code)
		}
	})
}
