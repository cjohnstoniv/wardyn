// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Inbound federation (#3787): the switch, the trusts and the tokens exchanged under them. Guarded by WARDYN_TEST_PG.

const inboundMigration = "0400_inbound_federation.sql"

// inboundFix is one migrated database with two bound sponsors.
type inboundFix struct {
	ctx  context.Context
	st   store.PG
	pool *pgxpool.Pool
	a, b string
}

func newInboundFix(t *testing.T) *inboundFix {
	t.Helper()
	st, pool := isolatedStore(t)
	return inboundFixOn(t, st, pool)
}

func inboundFixOn(t *testing.T, st store.PG, pool *pgxpool.Pool) *inboundFix {
	t.Helper()
	f := &inboundFix{ctx: context.Background(), st: st, pool: pool, a: "sub-inbound-a", b: "sub-inbound-b"}
	for _, p := range []string{f.a, f.b} {
		f.person(t, p)
	}
	return f
}

// person makes p a bound identity, so it can sponsor.
func (f *inboundFix) person(t *testing.T, p string) {
	t.Helper()
	if _, err := f.st.UpsertLoginIdentity(f.ctx, store.LoginIdentity{Principal: p, Issuer: "https://issuer.test", Email: p + "@corp.example"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

// identity makes a service identity sponsored by sponsor (f.a when empty).
func (f *inboundFix) identity(t *testing.T, name, sponsor string) {
	t.Helper()
	if sponsor == "" {
		sponsor = f.a
	}
	if _, err := f.st.CreateServiceIdentity(f.ctx, types.ServiceIdentity{Name: name, Sponsor: sponsor, UserType: types.UserTypeStandard, CreatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
}

func (f *inboundFix) seq(t *testing.T) int64 {
	t.Helper()
	seq, err := store.ReadRevocationSeq(f.ctx, f.pool)
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

// newTrust is the trust the API builds, with every bound at its default.
func (f *inboundFix) newTrust(t *testing.T, name, service, by string) types.InboundTrust {
	t.Helper()
	return types.InboundTrust{
		Name: name, Issuer: "https://idp.example.com/" + name, Audience: "aud-" + name, Subject: "repo:" + name,
		ServiceIdentity: service, TokenTTLSeconds: types.InboundTrustTTLDefaultSeconds,
		MaxTokenAgeSeconds: types.InboundTrustMaxAgeDefaultSeconds, MaxLive: types.InboundTrustMaxLiveDefault,
		CreatedBy: by, ConfirmedAt: time.Now(), ConfirmedBy: by, ConfirmedSeq: f.seq(t),
	}
}

// trust creates a trust on service and returns it; edit, when set, changes the request first.
func (f *inboundFix) trust(t *testing.T, name, service string, edit func(*types.InboundTrust)) types.InboundTrust {
	t.Helper()
	in := f.newTrust(t, name, service, "admin")
	if edit != nil {
		edit(&in)
	}
	out, _, err := f.st.CreateInboundTrust(f.ctx, in)
	if err != nil {
		t.Fatalf("create trust %s: %v", name, err)
	}
	return out
}

func (f *inboundFix) on(t *testing.T) {
	t.Helper()
	if _, err := f.st.SetInboundFederation(f.ctx, true, "admin"); err != nil {
		t.Fatal(err)
	}
}

// exchange builds the row the API builds for an exchange and stores it under trust.
func exchange(t *testing.T, st store.PG, trust types.InboundTrust) (types.APIToken, error) {
	t.Helper()
	exp := time.Now().UTC().Add(time.Duration(trust.TokenTTLSeconds) * time.Second)
	return st.CreateExchangedToken(context.Background(), trust, types.APIToken{
		ID: uuid.New(), Principal: types.ServicePrincipalPrefix + trust.ServiceIdentity, Role: "user",
		UserType: types.UserTypeStandard, Groups: []string{}, Name: "inbound:" + trust.Name, ExpiresAt: &exp,
		Kind: types.APITokenKindInbound,
	}, "wdn_"+uuid.NewString())
}

func (f *inboundFix) mustExchange(t *testing.T, trust types.InboundTrust) types.APIToken {
	t.Helper()
	tok, err := exchange(t, f.st, trust)
	if err != nil {
		t.Fatalf("exchange under %s: %v", trust.Name, err)
	}
	return tok
}

// age moves a token's expiry, on the token and on its link, back by d.
func (f *inboundFix) age(t *testing.T, d time.Duration, ids ...uuid.UUID) {
	t.Helper()
	for _, q := range []string{
		`UPDATE api_tokens SET expires_at = expires_at - $2::bigint * interval '1 microsecond' WHERE id = ANY($1)`,
		`UPDATE inbound_trust_tokens SET expires_at = expires_at - $2::bigint * interval '1 microsecond' WHERE token_id = ANY($1)`,
	} {
		if _, err := f.pool.Exec(f.ctx, q, ids, d.Microseconds()); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *inboundFix) liveOf(t *testing.T, service string) int {
	t.Helper()
	return rowCount(t, f.pool, `SELECT count(*) FROM api_tokens WHERE principal = $1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())`,
		types.ServicePrincipalPrefix+service)
}

func (f *inboundFix) tokenRow(t *testing.T, id uuid.UUID) (exists, revoked bool) {
	t.Helper()
	err := f.pool.QueryRow(f.ctx, `SELECT revoked_at IS NOT NULL FROM api_tokens WHERE id = $1`, id).Scan(&revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true, revoked
}

func (f *inboundFix) sponsor(service, to string) error {
	_, err := f.st.UpdateServiceIdentity(f.ctx, service, store.ServiceIdentityChange{Sponsor: &to, By: "admin"})
	return err
}

func (f *inboundFix) wantTrustGone(t *testing.T, id uuid.UUID) {
	t.Helper()
	if _, err := f.st.GetInboundTrust(f.ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("trust %s = %v; want it gone", id, err)
	}
}

func isDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

// both runs x and y at once and returns their errors.
func both(x, y func() error) (ex, ey error) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(2)
	go func() { defer wg.Done(); <-start; ex = x() }()
	go func() { defer wg.Done(); <-start; ey = y() }()
	close(start)
	wg.Wait()
	return ex, ey
}

func turnOnChange(by string) types.GovernanceChange {
	return types.GovernanceChange{
		TargetKind: types.GovernanceTargetInboundFederation, Op: "update", TargetKey: types.InboundFederationSwitchKey,
		Payload: json.RawMessage(`{"enabled":true}`), Diff: json.RawMessage(`{"changed":["enabled"]}`),
		BaseHash: `"base"`, DeploymentHash: `"dep"`, ProposedBy: by, ProposedByEmail: by + "@corp.example",
	}
}

func TestPG_InboundFederation_OffByDefault(t *testing.T) {
	f := newInboundFix(t)
	got, err := f.st.GetInboundFederation(f.ctx)
	if err != nil || got.Enabled {
		t.Fatalf("a fresh switch = %+v, %v; want off", got, err)
	}
	f.identity(t, "ci-bot", "")
	tr := f.trust(t, "ci-trust", "ci-bot", nil)
	if _, err := exchange(t, f.st, tr); !errors.Is(err, store.ErrInboundFederationOff) {
		t.Fatalf("exchange while off = %v; want ErrInboundFederationOff", err)
	}
	if n := rowCount(t, f.pool, `SELECT count(*) FROM api_tokens WHERE principal = 'service:ci-bot'`); n != 0 {
		t.Errorf("rows written while off = %d; want 0", n)
	}
}

func TestPG_InboundFederationOff_RevokesEveryExchangedToken(t *testing.T) {
	f := newInboundFix(t)
	f.identity(t, "bot-one", "")
	f.identity(t, "bot-two", "")
	t1, t2 := f.trust(t, "trust-one", "bot-one", nil), f.trust(t, "trust-two", "bot-two", nil)
	f.on(t)
	ids := []uuid.UUID{f.mustExchange(t, t1).ID, f.mustExchange(t, t1).ID, f.mustExchange(t, t2).ID}
	expired := f.mustExchange(t, t1).ID
	f.age(t, time.Hour, expired)
	exp := time.Now().UTC().Add(time.Hour)
	minted, err := f.st.CreateServiceToken(f.ctx, "bot-one", f.a, 20, types.APIToken{
		ID: uuid.New(), Principal: "service:bot-one", Role: "user", UserType: types.UserTypeStandard, Name: "ci", ExpiresAt: &exp,
	}, "wdn_"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	set, err := f.st.SetInboundFederation(f.ctx, false, "admin")
	if err != nil || set.TokensRevoked != 3 || set.State.Enabled {
		t.Fatalf("turn off = %+v, %v; want off with 3 revoked", set, err)
	}
	for _, id := range ids {
		if _, revoked := f.tokenRow(t, id); !revoked {
			t.Errorf("exchanged token %s is live after the switch-off", id)
		}
	}
	if _, revoked := f.tokenRow(t, minted.ID); revoked {
		t.Error("the sponsor's own token was revoked by the switch-off")
	}
	if exists, revoked := f.tokenRow(t, expired); !exists || revoked {
		t.Errorf("an expired exchanged token: exists %v, revoked %v; want a kept row with revoked_at NULL", exists, revoked)
	}
	if set, err = f.st.SetInboundFederation(f.ctx, true, "admin"); err != nil || set.TokensRevoked != 0 || !set.State.Enabled {
		t.Fatalf("turn on again = %+v, %v; want on with nothing revoked", set, err)
	}
}

func TestPG_SwitchOffCancelsPendingTurnOn(t *testing.T) {
	f := newInboundFix(t)
	turnOn, _, err := f.st.ProposeGovernanceChange(f.ctx, turnOnChange("alice"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	create := turnOnChange("alice")
	create.Op, create.TargetKey, create.Payload = "create", "new-trust", json.RawMessage(`{"name":"new-trust"}`)
	held, _, err := f.st.ProposeGovernanceChange(f.ctx, create, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	set, err := f.st.SetInboundFederation(f.ctx, false, "bob")
	if err != nil || len(set.ChangesCancelled) != 1 || set.ChangesCancelled[0] != turnOn.ID {
		t.Fatalf("turn off = %+v, %v; want the turn-on %s cancelled", set, err, turnOn.ID)
	}
	got, err := f.st.GetGovernanceChange(f.ctx, turnOn.ID)
	if err != nil || got.State != types.GovernanceChangeStale || got.DecidedBy != "bob" || got.Reason != "Inbound federation was turned off" {
		t.Fatalf("the turn-on after the switch-off = %+v, %v", got, err)
	}
	if got, err := f.st.GetGovernanceChange(f.ctx, held.ID); err != nil || got.State != types.GovernanceChangePending {
		t.Fatalf("the held trust create = %+v, %v; want still pending", got, err)
	}
	for _, on := range []bool{false, true} {
		if set, err := f.st.SetInboundFederation(f.ctx, on, "bob"); err != nil || len(set.ChangesCancelled) != 0 {
			t.Fatalf("set %v = %+v, %v; want nothing cancelled", on, set, err)
		}
	}
}

func TestPG_SwitchOffAndTurnOnDecisionRace(t *testing.T) {
	f := newInboundFix(t)
	for round := 0; round < 20; round++ {
		ch, _, err := f.st.ProposeGovernanceChange(f.ctx, turnOnChange("alice"), time.Hour)
		if err != nil {
			t.Fatalf("round %d: propose: %v", round, err)
		}
		decided, off := both(func() error {
			_, err := f.st.DecideGovernanceChange(f.ctx, ch.ID, store.GovernanceDecision{To: types.GovernanceChangeApplied, By: "carol"},
				func(q store.Querier, _ types.GovernanceChange) error {
					if err := store.LockGovernanceTarget(f.ctx, q, types.GovernanceTargetInboundFederation, types.InboundFederationSwitchKey); err != nil {
						return err
					}
					_, err := store.SetInboundFederationQ(f.ctx, q, true, "carol")
					return err
				})
			return err
		}, func() error {
			_, err := f.st.SetInboundFederation(f.ctx, false, "bob")
			return err
		})
		if isDeadlock(decided) || isDeadlock(off) || off != nil {
			t.Fatalf("round %d: decision %v, switch-off %v", round, decided, off)
		}
		got, err := f.st.GetGovernanceChange(f.ctx, ch.ID)
		if err != nil {
			t.Fatal(err)
		}
		var notPending *store.ErrGovernanceChangeNotPending
		switch {
		case got.State == types.GovernanceChangeApplied && decided == nil:
		case got.State == types.GovernanceChangeStale && errors.As(decided, &notPending):
		default:
			t.Fatalf("round %d: change %s, decision %v; want applied, or stale and not pending", round, got.State, decided)
		}
		if sw, err := f.st.GetInboundFederation(f.ctx); err != nil || sw.Enabled {
			t.Fatalf("round %d: switch = %+v, %v; want off", round, sw, err)
		}
	}
}

// raceRound is one round's identity, trust and tokens: two live, one expired.
func (f *inboundFix) raceRound(t *testing.T, tokens bool) (string, types.InboundTrust) {
	t.Helper()
	name := "race-bot-" + uuid.NewString()[:8]
	f.identity(t, name, "")
	tr := f.trust(t, "race-"+uuid.NewString()[:8], name, nil)
	f.on(t)
	if tokens {
		f.mustExchange(t, tr)
		f.mustExchange(t, tr)
		f.age(t, time.Hour, f.mustExchange(t, tr).ID)
	}
	return name, tr
}

func TestPG_SwitchOffAndSponsorChangeRace(t *testing.T) {
	f := newInboundFix(t)
	for round := 0; round < 20; round++ {
		name, tr := f.raceRound(t, true)
		off, moved := both(func() error {
			_, err := f.st.SetInboundFederation(f.ctx, false, "admin")
			return err
		}, func() error { return f.sponsor(name, f.b) })
		if off != nil || moved != nil {
			t.Fatalf("round %d: switch-off %v, sponsor change %v; want both to commit", round, off, moved)
		}
		if n := f.liveOf(t, name); n != 0 {
			t.Fatalf("round %d: %d live tokens; want 0", round, n)
		}
		f.wantTrustGone(t, tr.ID)
	}
}

func TestPG_SwitchOffAndTrustDeleteRace(t *testing.T) {
	f := newInboundFix(t)
	for round := 0; round < 20; round++ {
		name, tr := f.raceRound(t, true)
		off, del := both(func() error {
			_, err := f.st.SetInboundFederation(f.ctx, false, "admin")
			return err
		}, func() error {
			_, _, err := f.st.DeleteInboundTrust(f.ctx, store.InboundTrustDelete{ID: tr.ID})
			return err
		})
		if off != nil || (del != nil && !errors.Is(del, store.ErrNotFound)) || isDeadlock(del) {
			t.Fatalf("round %d: switch-off %v, delete %v", round, off, del)
		}
		f.wantTrustGone(t, tr.ID)
		if n := f.liveOf(t, name); n != 0 {
			t.Fatalf("round %d: %d live tokens; want every token revoked", round, n)
		}
	}
}

// bothWithin is both with a watchdog: a race that wedges fails the test instead of hanging it.
func bothWithin(t *testing.T, x, y func() error) (ex, ey error) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); ex, ey = both(x, y) }()
	select {
	case <-done:
		return ex, ey
	case <-time.After(time.Minute):
		t.Fatal("the race did not finish in a minute")
		return nil, nil
	}
}

func TestPG_ExchangeAndSwitchOffRace(t *testing.T) {
	f := newInboundFix(t)
	for round := 0; round < 20; round++ {
		name, tr := f.raceRound(t, false)
		var tok types.APIToken
		ex, off := bothWithin(t, func() (err error) {
			tok, err = exchange(t, f.st, tr)
			return err
		}, func() error {
			_, err := f.st.SetInboundFederation(f.ctx, false, "admin")
			return err
		})
		if off != nil || isDeadlock(ex) {
			t.Fatalf("round %d: exchange %v, switch-off %v", round, ex, off)
		}
		switch {
		case ex == nil:
			if _, revoked := f.tokenRow(t, tok.ID); !revoked {
				t.Fatalf("round %d: the exchanged token outlived the switch-off", round)
			}
		case errors.Is(ex, store.ErrInboundFederationOff):
		default:
			t.Fatalf("round %d: exchange: %v", round, ex)
		}
		if n := f.liveOf(t, name); n != 0 {
			t.Fatalf("round %d: %d live tokens after the switch-off; want 0", round, n)
		}
	}
}

func TestPG_ExchangeAndTrustDeleteRace(t *testing.T) {
	f := newInboundFix(t)
	for round := 0; round < 20; round++ {
		name, tr := f.raceRound(t, false)
		var tok types.APIToken
		ex, del := bothWithin(t, func() (err error) {
			tok, err = exchange(t, f.st, tr)
			return err
		}, func() error {
			_, _, err := f.st.DeleteInboundTrust(f.ctx, store.InboundTrustDelete{ID: tr.ID})
			return err
		})
		if del != nil || isDeadlock(ex) {
			t.Fatalf("round %d: exchange %v, delete %v", round, ex, del)
		}
		switch {
		case ex == nil:
			if _, revoked := f.tokenRow(t, tok.ID); !revoked {
				t.Fatalf("round %d: the exchanged token outlived the trust's delete", round)
			}
		case errors.Is(ex, store.ErrNotFound):
		default:
			t.Fatalf("round %d: exchange: %v", round, ex)
		}
		f.wantTrustGone(t, tr.ID)
		if n := f.liveOf(t, name); n != 0 {
			t.Fatalf("round %d: %d live tokens after the trust's delete; want 0", round, n)
		}
	}
}

func TestPG_EraseAttributionClearsInboundPeople(t *testing.T) {
	f := newInboundFix(t)
	const p = "sub-inbound-erased"
	f.person(t, p)
	if _, err := f.st.CreateServiceIdentity(f.ctx, types.ServiceIdentity{Name: "bot-a", Sponsor: f.a, UserType: types.UserTypeStandard, CreatedBy: p}); err != nil {
		t.Fatal(err)
	}
	f.identity(t, "bot-b", p)
	if _, err := f.st.SetInboundFederation(f.ctx, true, p); err != nil {
		t.Fatal(err)
	}
	trA, _, err := f.st.CreateInboundTrust(f.ctx, f.newTrust(t, "trust-a", "bot-a", p))
	if err != nil {
		t.Fatal(err)
	}
	trB := f.trust(t, "trust-b", "bot-b", func(in *types.InboundTrust) { in.CreatedBy, in.ConfirmedBy = "other", "other" })
	if _, err := f.st.ConfirmInboundTrust(f.ctx, store.InboundTrustConfirm{ID: trB.ID, Service: "bot-b", Sponsor: p, At: time.Now(), Seq: f.seq(t)}); err != nil {
		t.Fatal(err)
	}
	cleared, sponsored, err := f.st.EraseServiceIdentityAttributionOf(f.ctx, p)
	if err != nil || cleared != 1 || len(sponsored) != 1 || sponsored[0] != "bot-b" {
		t.Fatalf("erase = %d, %v, %v; want 1 service identity row cleared and bot-b still sponsored", cleared, sponsored, err)
	}
	if sw, err := f.st.GetInboundFederation(f.ctx); err != nil || sw.UpdatedBy != "" || !sw.Enabled {
		t.Fatalf("switch after the erasure = %+v, %v; want on, with no one named", sw, err)
	}
	a, _ := f.st.GetInboundTrust(f.ctx, trA.ID)
	b, _ := f.st.GetInboundTrust(f.ctx, trB.ID)
	if a.CreatedBy != "" || a.ConfirmedBy != "" || b.ConfirmedBy != "" || b.CreatedBy != "other" {
		t.Fatalf("after the erasure A = %q/%q, B = %q/%q; want A empty, B confirmed by no one and created by other",
			a.CreatedBy, a.ConfirmedBy, b.CreatedBy, b.ConfirmedBy)
	}
	f.mustExchange(t, a)
}

func TestPG_InboundTrust_UniqueIssuerAudienceSubject(t *testing.T) {
	f := newInboundFix(t)
	f.identity(t, "ci-bot", "")
	first := f.trust(t, "first-trust", "ci-bot", nil)
	sameTriple := f.newTrust(t, "other-name", "ci-bot", "admin")
	sameTriple.Issuer, sameTriple.Audience, sameTriple.Subject = first.Issuer, first.Audience, first.Subject
	sameName := f.newTrust(t, "first-trust", "ci-bot", "admin")
	sameName.Subject = "repo:another"
	for _, in := range []types.InboundTrust{sameTriple, sameName} {
		if _, _, err := f.st.CreateInboundTrust(f.ctx, in); !errors.Is(err, store.ErrInboundTrustExists) {
			t.Errorf("create %s = %v; want ErrInboundTrustExists", in.Name, err)
		}
	}
}

func TestPG_InboundTrust_RefusesPortalAudience(t *testing.T) {
	f := newInboundFix(t)
	f.identity(t, "ci-bot", "")
	portal := newIssuerPortal(t, f.st, "")
	aud := "portal-aud-" + uuid.NewString()
	if _, err := f.pool.Exec(f.ctx, `UPDATE delegates SET audience = $2 WHERE id = $1`, portal.Delegate.ID, aud); err != nil {
		t.Fatal(err)
	}
	for i, reserved := range []string{portal.Delegate.IdPClientID, aud} {
		in := f.newTrust(t, []string{"reserved-one", "reserved-two"}[i], "ci-bot", "admin")
		in.Audience = reserved
		if _, _, err := f.st.CreateInboundTrust(f.ctx, in); !errors.Is(err, store.ErrInboundTrustAudienceReserved) {
			t.Errorf("audience %q = %v; want ErrInboundTrustAudienceReserved", reserved, err)
		}
	}
	if _, _, err := f.st.RevokeDelegate(f.ctx, portal.Delegate.ID, time.Now(), false); err != nil {
		t.Fatal(err)
	}
	f.trust(t, "after-revoke", "ci-bot", func(in *types.InboundTrust) { in.Audience = aud })
}

func TestPG_InboundTrust_RefusesInactiveIdentity(t *testing.T) {
	f := newInboundFix(t)
	f.identity(t, "held-bot", "")
	f.identity(t, "gone-bot", "")
	yes := true
	if _, err := f.st.UpdateServiceIdentity(f.ctx, "held-bot", store.ServiceIdentityChange{Suspended: &yes, By: "admin"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.RetireServiceIdentity(f.ctx, "gone-bot", "admin"); err != nil {
		t.Fatal(err)
	}
	for svc, want := range map[string]error{"held-bot": store.ErrServiceIdentityInactive, "gone-bot": store.ErrServiceIdentityInactive, "no-bot": store.ErrNotFound} {
		if _, _, err := f.st.CreateInboundTrust(f.ctx, f.newTrust(t, "trust-"+svc, svc, "admin")); !errors.Is(err, want) {
			t.Errorf("trust on %s = %v; want %v", svc, err, want)
		}
	}
}

func TestPG_InboundTrustDelete_RevokesItsTokens(t *testing.T) {
	f := newInboundFix(t)
	f.identity(t, "ci-bot", "")
	trA, trB := f.trust(t, "trust-a", "ci-bot", nil), f.trust(t, "trust-b", "ci-bot", nil)
	f.on(t)
	live := []uuid.UUID{f.mustExchange(t, trA).ID, f.mustExchange(t, trA).ID}
	expired := f.mustExchange(t, trA).ID
	f.age(t, time.Hour, expired)
	// A moved token survives the delete's removal of expired rows, so its revoked_at can be read afterwards.
	expiredMoved := f.mustExchange(t, trA).ID
	f.age(t, time.Hour, expiredMoved)
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO api_token_moves (token_id, service_identity, linked_by) VALUES ($1, 'ci-bot', 'admin')`, expiredMoved); err != nil {
		t.Fatal(err)
	}
	other := f.mustExchange(t, trB).ID
	_, n, err := f.st.DeleteInboundTrust(f.ctx, store.InboundTrustDelete{ID: trA.ID})
	if err != nil || n != 2 {
		t.Fatalf("delete = %d, %v; want 2 revoked", n, err)
	}
	for _, id := range live {
		if exists, revoked := f.tokenRow(t, id); !exists || !revoked {
			t.Errorf("A's live token %s: exists %v, revoked %v; want a revoked row", id, exists, revoked)
		}
	}
	if exists, _ := f.tokenRow(t, expired); exists {
		t.Error("A's expired token row is still there")
	}
	if exists, revoked := f.tokenRow(t, expiredMoved); !exists || revoked {
		t.Errorf("A's expired moved token: exists %v, revoked %v; want a kept row with revoked_at NULL", exists, revoked)
	}
	if _, revoked := f.tokenRow(t, other); revoked {
		t.Error("B's token was revoked by A's delete")
	}
	if n := rowCount(t, f.pool, `SELECT count(*) FROM inbound_trust_tokens WHERE trust_id = $1`, trA.ID); n != 0 {
		t.Errorf("A has %d links left; want 0", n)
	}
}

func TestPG_InboundTrustDelete_SponsorScoped(t *testing.T) {
	f := newInboundFix(t)
	f.identity(t, "ci-bot", "")
	f.identity(t, "ci-other", "")
	tr := f.trust(t, "ci-trust", "ci-bot", nil)
	for _, d := range []store.InboundTrustDelete{
		{ID: tr.ID, Service: "ci-bot", Sponsor: f.b},
		{ID: tr.ID, Service: "ci-other", Sponsor: f.a},
	} {
		if _, _, err := f.st.DeleteInboundTrust(f.ctx, d); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("delete %+v = %v; want ErrNotFound", d, err)
		}
	}
	if _, err := f.st.GetInboundTrust(f.ctx, tr.ID); err != nil {
		t.Fatalf("the trust after the refused deletes: %v", err)
	}
	if _, _, err := f.st.DeleteInboundTrust(f.ctx, store.InboundTrustDelete{ID: tr.ID, Service: "ci-bot", Sponsor: f.a}); err != nil {
		t.Fatal(err)
	}
	f.wantTrustGone(t, tr.ID)
}

func TestPG_InboundExchange_RefusesInactiveIdentity(t *testing.T) {
	f := newInboundFix(t)
	f.identity(t, "held-bot", "")
	f.identity(t, "gone-bot", "")
	held, gone := f.trust(t, "held-trust", "held-bot", nil), f.trust(t, "gone-trust", "gone-bot", nil)
	f.on(t)
	yes := true
	if _, err := f.st.UpdateServiceIdentity(f.ctx, "held-bot", store.ServiceIdentityChange{Suspended: &yes, By: "admin"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE service_identities SET retired_at = now(), retired_by = 'admin' WHERE name = 'gone-bot'`); err != nil {
		t.Fatal(err)
	}
	for _, tr := range []types.InboundTrust{held, gone} {
		if _, err := exchange(t, f.st, tr); !errors.Is(err, store.ErrServiceIdentityInactive) {
			t.Errorf("exchange under %s = %v; want ErrServiceIdentityInactive", tr.Name, err)
		}
	}
	if n := rowCount(t, f.pool, `SELECT count(*) FROM inbound_trust_tokens`); n != 0 {
		t.Errorf("%d tokens written; want 0", n)
	}
}

func TestPG_InboundExchange_TokenAfterSponsorSince(t *testing.T) {
	f := newInboundFix(t)
	f.identity(t, "ci-bot", "")
	tr := f.trust(t, "ci-trust", "ci-bot", nil)
	f.on(t)
	created := time.Now().UTC().Add(-time.Hour)
	exp := created.Add(15 * time.Minute)
	_, err := f.st.CreateExchangedToken(f.ctx, tr, types.APIToken{
		ID: uuid.New(), Principal: "service:ci-bot", Role: "user", UserType: types.UserTypeStandard, Name: "early",
		CreatedAt: created, ExpiresAt: &exp, Kind: types.APITokenKindInbound,
	}, "wdn_"+uuid.NewString())
	if !errors.Is(err, store.ErrServiceIdentityInactive) {
		t.Fatalf("a token made before sponsor_since = %v; want ErrServiceIdentityInactive", err)
	}
	if n := rowCount(t, f.pool, `SELECT count(*) FROM api_tokens WHERE principal = 'service:ci-bot'`); n != 0 {
		t.Errorf("%d rows written; want 0", n)
	}
}

func TestPG_InboundExchange_RefusesExpiryPastTrustTTL(t *testing.T) {
	f := newInboundFix(t)
	f.identity(t, "ci-bot", "")
	tr := f.trust(t, "ci-trust", "ci-bot", nil)
	f.on(t)
	// The door reads the stored ttl, so a caller's stale copy of the trust cannot widen it.
	tr.TokenTTLSeconds = types.InboundTrustTTLMaxSeconds
	exp := time.Now().UTC().Add(time.Duration(types.InboundTrustTTLDefaultSeconds)*time.Second + 90*time.Second)
	_, err := f.st.CreateExchangedToken(f.ctx, tr, types.APIToken{
		ID: uuid.New(), Principal: "service:ci-bot", Role: "user", UserType: types.UserTypeStandard, Name: "long",
		ExpiresAt: &exp, Kind: types.APITokenKindInbound,
	}, "wdn_"+uuid.NewString())
	if err == nil {
		t.Fatal("a token expiring 90 s past its trust's ttl was stored")
	}
	if n := rowCount(t, f.pool, `SELECT count(*) FROM api_tokens WHERE principal = 'service:ci-bot'`); n != 0 {
		t.Errorf("%d rows written; want 0", n)
	}
	if _, err := exchange(t, f.st, tr); err == nil {
		t.Fatal("exchange with the caller's widened ttl was stored")
	}
	// Up to 60 s past the ttl is clock slack between the API host and the database, and is stored.
	slack := time.Now().UTC().Add(time.Duration(types.InboundTrustTTLDefaultSeconds)*time.Second + 30*time.Second)
	if _, err := f.st.CreateExchangedToken(f.ctx, tr, types.APIToken{
		ID: uuid.New(), Principal: "service:ci-bot", Role: "user", UserType: types.UserTypeStandard, Name: "slack",
		ExpiresAt: &slack, Kind: types.APITokenKindInbound,
	}, "wdn_"+uuid.NewString()); err != nil {
		t.Fatalf("a token expiring 30 s past the ttl = %v; want stored (inside the clock slack)", err)
	}
	tr.TokenTTLSeconds = types.InboundTrustTTLDefaultSeconds
	if _, err := exchange(t, f.st, tr); err != nil {
		t.Fatalf("a token expiring at the trust's ttl = %v; want stored", err)
	}
}

func TestPG_InboundTrust_RefusesOversizeTriple(t *testing.T) {
	f := newInboundFix(t)
	f.identity(t, "ci-bot", "")
	for name, edit := range map[string]func(*types.InboundTrust){
		"issuer":          func(in *types.InboundTrust) { in.Issuer = "https://" + strings.Repeat("a", 1020) },
		"subject":         func(in *types.InboundTrust) { in.Subject = strings.Repeat("s", 513) },
		"internal_issuer": func(in *types.InboundTrust) { in.InternalIssuer = strings.Repeat("i", 1025) },
		"required_claim": func(in *types.InboundTrust) {
			in.RequiredClaim, in.RequiredClaimValue = strings.Repeat("c", 257), "v"
		},
		"required_claim_value": func(in *types.InboundTrust) {
			in.RequiredClaim, in.RequiredClaimValue = "c", strings.Repeat("v", 513)
		},
		// 512 four-byte characters each: inside every character bound, over the byte bound the unique index needs.
		"bytes": func(in *types.InboundTrust) {
			in.Audience, in.Subject = strings.Repeat("\U0001F600", 512), strings.Repeat("\U0001F601", 512)
		},
	} {
		in := f.newTrust(t, "big-"+name, "ci-bot", "admin")
		edit(&in)
		if _, _, err := f.st.CreateInboundTrust(f.ctx, in); !errors.Is(err, store.ErrInboundTrustInvalid) {
			t.Errorf("oversize %s = %v; want ErrInboundTrustInvalid", name, err)
		}
	}
	ok := f.newTrust(t, "big-ok", "ci-bot", "admin")
	ok.Issuer, ok.Audience, ok.Subject = "https://"+strings.Repeat("a", 1016), strings.Repeat("u", 512), strings.Repeat("s", 512)
	ok.InternalIssuer, ok.RequiredClaim, ok.RequiredClaimValue = strings.Repeat("i", 1024), strings.Repeat("c", 256), strings.Repeat("v", 512)
	if _, _, err := f.st.CreateInboundTrust(f.ctx, ok); err != nil {
		t.Errorf("a triple at every bound = %v; want stored", err)
	}
}

func TestPG_InboundExchange_TrustGone(t *testing.T) {
	f := newInboundFix(t)
	f.identity(t, "ci-bot", "")
	tr := f.trust(t, "ci-trust", "ci-bot", nil)
	f.on(t)
	if _, _, err := f.st.DeleteInboundTrust(f.ctx, store.InboundTrustDelete{ID: tr.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := exchange(t, f.st, tr); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("exchange under a deleted trust = %v; want ErrNotFound", err)
	}
}

func TestPG_InboundExchange_CapPerTrust(t *testing.T) {
	f := newInboundFix(t)
	f.identity(t, "ci-bot", "")
	tr := f.trust(t, "ci-trust", "ci-bot", func(in *types.InboundTrust) { in.MaxLive = 2 })
	f.on(t)
	first := f.mustExchange(t, tr)
	f.mustExchange(t, tr)
	if _, err := exchange(t, f.st, tr); !errors.Is(err, store.ErrInboundTrustCap) {
		t.Fatalf("third exchange = %v; want ErrInboundTrustCap", err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE api_tokens SET revoked_at = now() WHERE id = $1`, first.ID); err != nil {
		t.Fatal(err)
	}
	f.mustExchange(t, tr)
}

func TestPG_InboundExchange_CapHoldsUnderConcurrency(t *testing.T) {
	f := newInboundFix(t)
	f.identity(t, "ci-bot", "")
	tr := f.trust(t, "ci-trust", "ci-bot", func(in *types.InboundTrust) { in.MaxLive = 3 })
	f.on(t)
	errs := make([]error, 10)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = exchange(t, f.st, tr)
		}(i)
	}
	close(start)
	wg.Wait()
	ok, capped := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, store.ErrInboundTrustCap):
			capped++
		default:
			t.Errorf("exchange: %v", err)
		}
	}
	if ok != 3 || capped != 7 {
		t.Fatalf("%d succeeded and %d were capped; want 3 and 7", ok, capped)
	}
}

func TestPG_ExchangedTokensDoNotFillSponsorCap(t *testing.T) {
	f := newInboundFix(t)
	f.identity(t, "ci-bot", "")
	tr := f.trust(t, "ci-trust", "ci-bot", nil)
	f.on(t)
	for i := 0; i < 20; i++ {
		f.mustExchange(t, tr)
	}
	mint := func() error {
		exp := time.Now().UTC().Add(time.Hour)
		_, err := f.st.CreateServiceToken(f.ctx, "ci-bot", f.a, 20, types.APIToken{
			ID: uuid.New(), Principal: "service:ci-bot", Role: "user", UserType: types.UserTypeStandard, Name: "ci", ExpiresAt: &exp,
		}, "wdn_"+uuid.NewString())
		return err
	}
	for i := 0; i < 20; i++ {
		if err := mint(); err != nil {
			t.Fatalf("sponsor mint %d beside 20 exchanged tokens: %v", i+1, err)
		}
	}
	if err := mint(); !errors.Is(err, store.ErrServiceTokenCap) {
		t.Fatalf("21st sponsor mint = %v; want ErrServiceTokenCap", err)
	}
	f.mustExchange(t, tr)
}

func TestPG_InboundExchange_PrunesExpired(t *testing.T) {
	f := newInboundFix(t)
	f.identity(t, "ci-bot", "")
	tr := f.trust(t, "ci-trust", "ci-bot", func(in *types.InboundTrust) { in.MaxLive = types.InboundTrustMaxLiveMax })
	f.on(t)
	old := make([]uuid.UUID, 150)
	for i := range old {
		old[i] = f.mustExchange(t, tr).ID
	}
	recent := f.mustExchange(t, tr).ID
	f.age(t, 25*time.Hour, old...)
	f.age(t, time.Hour, recent)
	oldLeft := func() int {
		return rowCount(t, f.pool, `SELECT count(*) FROM api_tokens WHERE id = ANY($1)`, old)
	}
	f.mustExchange(t, tr)
	if n := oldLeft(); n != 50 {
		t.Fatalf("after one exchange %d old rows are left; want 50", n)
	}
	f.mustExchange(t, tr)
	if n := oldLeft(); n != 0 {
		t.Fatalf("after two exchanges %d old rows are left; want 0", n)
	}
	if exists, _ := f.tokenRow(t, recent); !exists {
		t.Fatal("the row expired an hour ago was pruned")
	}
}

func TestPG_InboundExchange_PruneSkipsAMovedToken(t *testing.T) {
	f := newInboundFix(t)
	f.identity(t, "ci-bot", "")
	tr := f.trust(t, "ci-trust", "ci-bot", nil)
	f.on(t)
	moved := f.mustExchange(t, tr).ID
	f.age(t, 25*time.Hour, moved)
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO api_token_moves (token_id, service_identity, linked_by) VALUES ($1, 'ci-bot', 'admin')`, moved); err != nil {
		t.Fatal(err)
	}
	f.mustExchange(t, tr)
	if exists, _ := f.tokenRow(t, moved); !exists {
		t.Fatal("a token named by a move was pruned")
	}
}

// twoTrusts makes service with two trusts, one exchanged token under each, and the switch on.
func (f *inboundFix) twoTrusts(t *testing.T, service string) []types.InboundTrust {
	t.Helper()
	f.identity(t, service, "")
	trs := []types.InboundTrust{f.trust(t, service+"-one", service, nil), f.trust(t, service+"-two", service, nil)}
	f.on(t)
	for _, tr := range trs {
		f.mustExchange(t, tr)
	}
	return trs
}

func TestPG_SponsorChangeDeletesTrusts(t *testing.T) {
	f := newInboundFix(t)
	trs := f.twoTrusts(t, "ci-bot")
	ch, err := f.st.UpdateServiceIdentity(f.ctx, "ci-bot", store.ServiceIdentityChange{Sponsor: &f.b, By: "admin"})
	if err != nil || ch.TrustsRemoved != 2 || ch.TokensRevoked != 2 {
		t.Fatalf("sponsor change = %+v, %v; want 2 trusts removed and 2 tokens revoked", ch, err)
	}
	for _, tr := range trs {
		f.wantTrustGone(t, tr.ID)
		if _, err := f.st.MatchInboundTrust(f.ctx, tr.Issuer, tr.Audience, tr.Subject); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("match %s = %v; want ErrNotFound", tr.Name, err)
		}
	}
}

func TestPG_SponsorChangeInHeldTransactionDeletesTrusts(t *testing.T) {
	f := newInboundFix(t)
	f.twoTrusts(t, "ci-bot")
	for _, commit := range []bool{false, true} {
		tx, err := f.pool.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		ch, err := store.UpdateServiceIdentityQ(f.ctx, tx, "ci-bot", store.ServiceIdentityChange{Sponsor: &f.b, By: "admin"})
		if err != nil || ch.TrustsRemoved != 2 {
			_ = tx.Rollback(f.ctx)
			t.Fatalf("held sponsor change = %+v, %v; want 2 trusts removed", ch, err)
		}
		if commit {
			err = tx.Commit(f.ctx)
		} else {
			err = tx.Rollback(f.ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
		want := 2
		if commit {
			want = 0
		}
		if got, err := f.st.ListInboundTrusts(f.ctx, "ci-bot"); err != nil || len(got) != want {
			t.Fatalf("commit %v: %d trusts, %v; want %d", commit, len(got), err, want)
		}
	}
}

func TestPG_DescriptionChangeKeepsTrusts(t *testing.T) {
	f := newInboundFix(t)
	f.twoTrusts(t, "ci-bot")
	desc := "builds"
	ch, err := f.st.UpdateServiceIdentity(f.ctx, "ci-bot", store.ServiceIdentityChange{Description: &desc, By: "admin"})
	if err != nil || ch.TrustsRemoved != 0 {
		t.Fatalf("description change = %+v, %v; want no trust removed", ch, err)
	}
	if got, err := f.st.ListInboundTrusts(f.ctx, "ci-bot"); err != nil || len(got) != 2 {
		t.Fatalf("%d trusts, %v; want 2", len(got), err)
	}
}

func TestPG_RetireDeletesTrusts(t *testing.T) {
	f := newInboundFix(t)
	trs := f.twoTrusts(t, "ci-bot")
	ch, err := f.st.RetireServiceIdentity(f.ctx, "ci-bot", "admin")
	if err != nil || ch.TrustsRemoved != 2 {
		t.Fatalf("retire = %+v, %v; want 2 trusts removed", ch, err)
	}
	for _, tr := range trs {
		f.wantTrustGone(t, tr.ID)
	}
}

func TestPG_ExchangeAndSponsorChangeRace(t *testing.T) {
	f := newInboundFix(t)
	for round := 0; round < 20; round++ {
		name, tr := f.raceRound(t, false)
		var tok types.APIToken
		ex, moved := both(func() (err error) {
			tok, err = exchange(t, f.st, tr)
			return err
		}, func() error { return f.sponsor(name, f.b) })
		if moved != nil {
			t.Fatalf("round %d: sponsor change: %v", round, moved)
		}
		switch {
		case ex == nil:
			if _, revoked := f.tokenRow(t, tok.ID); !revoked {
				t.Fatalf("round %d: the exchanged token outlived the sponsor change", round)
			}
		case errors.Is(ex, store.ErrNotFound), errors.Is(ex, store.ErrServiceIdentityInactive):
		default:
			t.Fatalf("round %d: exchange: %v", round, ex)
		}
		if n := f.liveOf(t, name); n != 0 {
			t.Fatalf("round %d: %d live tokens under the new sponsor; want 0", round, n)
		}
	}
}

// trustDeleteRace races an admin's delete of a trust holding one live token against change, 20 rounds.
func trustDeleteRace(t *testing.T, change func(f *inboundFix, service string) error) {
	f := newInboundFix(t)
	for round := 0; round < 20; round++ {
		name, tr := f.raceRound(t, false)
		tok := f.mustExchange(t, tr)
		del, ch := both(func() error {
			_, _, err := f.st.DeleteInboundTrust(f.ctx, store.InboundTrustDelete{ID: tr.ID})
			return err
		}, func() error { return change(f, name) })
		if isDeadlock(del) || isDeadlock(ch) {
			t.Fatalf("round %d: deadlock: delete %v, change %v", round, del, ch)
		}
		if ch != nil || (del != nil && !errors.Is(del, store.ErrNotFound)) {
			t.Fatalf("round %d: delete %v, change %v", round, del, ch)
		}
		f.wantTrustGone(t, tr.ID)
		if _, revoked := f.tokenRow(t, tok.ID); !revoked {
			t.Fatalf("round %d: the exchanged token is live", round)
		}
	}
}

func TestPG_TrustDeleteAndSponsorChangeRace(t *testing.T) {
	trustDeleteRace(t, func(f *inboundFix, service string) error { return f.sponsor(service, f.b) })
}

func TestPG_TrustDeleteAndRetireRace(t *testing.T) {
	trustDeleteRace(t, func(f *inboundFix, service string) error {
		_, err := f.st.RetireServiceIdentity(f.ctx, service, "admin")
		return err
	})
}

func TestPG_InboundTrust_StoresConfirmation(t *testing.T) {
	f := newInboundFix(t)
	f.identity(t, "ci-bot", "")
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	in := f.newTrust(t, "ci-trust", "ci-bot", "admin")
	in.ConfirmedAt, in.ConfirmedSeq, in.ConfirmedBy = at, 7, "confirmer"
	out, sponsor, err := f.st.CreateInboundTrust(f.ctx, in)
	if err != nil || sponsor != f.a || !out.ConfirmedAt.Equal(at) || out.ConfirmedSeq != 7 || out.ConfirmedBy != "confirmer" {
		t.Fatalf("create = %+v, %q, %v; want the confirmation as given and sponsor %s", out, sponsor, err, f.a)
	}
	for _, edit := range []func(*types.InboundTrust){
		func(in *types.InboundTrust) { in.ConfirmedAt = time.Time{} },
		func(in *types.InboundTrust) { in.ConfirmedBy = "" },
		func(in *types.InboundTrust) { in.CreatedBy = "" },
	} {
		bad := f.newTrust(t, "bad-trust", "ci-bot", "admin")
		edit(&bad)
		if _, _, err := f.st.CreateInboundTrust(f.ctx, bad); err == nil {
			t.Errorf("create %+v succeeded; want a refusal", bad)
		}
	}
	if n := rowCount(t, f.pool, `SELECT count(*) FROM inbound_trusts WHERE name = 'bad-trust'`); n != 0 {
		t.Errorf("%d refused trusts written; want 0", n)
	}
}

func TestPG_InboundTrustConfirm_SponsorScoped(t *testing.T) {
	f := newInboundFix(t)
	f.identity(t, "ci-bot", "")
	f.identity(t, "ci-other", "")
	tr := f.trust(t, "ci-trust", "ci-bot", nil)
	at := time.Now().UTC().Truncate(time.Microsecond)
	out, err := f.st.ConfirmInboundTrust(f.ctx, store.InboundTrustConfirm{ID: tr.ID, Service: "ci-bot", Sponsor: f.a, At: at, Seq: tr.ConfirmedSeq + 5})
	if err != nil || !out.ConfirmedAt.Equal(at) || out.ConfirmedSeq != tr.ConfirmedSeq+5 || out.ConfirmedBy != f.a {
		t.Fatalf("confirm = %+v, %v; want the three fields set", out, err)
	}
	for _, c := range []store.InboundTrustConfirm{
		{ID: tr.ID, Service: "ci-bot", Sponsor: f.b, At: time.Now(), Seq: 99},
		{ID: tr.ID, Service: "ci-other", Sponsor: f.a, At: time.Now(), Seq: 99},
	} {
		if _, err := f.st.ConfirmInboundTrust(f.ctx, c); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("confirm %+v = %v; want ErrNotFound", c, err)
		}
	}
	if got, _ := f.st.GetInboundTrust(f.ctx, tr.ID); got.ConfirmedSeq != tr.ConfirmedSeq+5 || got.ConfirmedBy != f.a {
		t.Fatalf("a refused confirm changed the trust: %+v", got)
	}
	yes, no := true, false
	if _, err := f.st.UpdateServiceIdentity(f.ctx, "ci-bot", store.ServiceIdentityChange{Suspended: &yes, By: "admin"}); err != nil {
		t.Fatal(err)
	}
	ok := store.InboundTrustConfirm{ID: tr.ID, Service: "ci-bot", Sponsor: f.a, At: time.Now(), Seq: 1}
	if _, err := f.st.ConfirmInboundTrust(f.ctx, ok); !errors.Is(err, store.ErrServiceIdentityInactive) {
		t.Fatalf("confirm on a suspended identity = %v; want ErrServiceIdentityInactive", err)
	}
	if _, err := f.st.UpdateServiceIdentity(f.ctx, "ci-bot", store.ServiceIdentityChange{Suspended: &no, By: "admin"}); err != nil {
		t.Fatal(err)
	}
	if err := f.sponsor("ci-bot", f.b); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.ConfirmInboundTrust(f.ctx, ok); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the old sponsor's confirm after a sponsor change = %v; want ErrNotFound", err)
	}
}

func TestPG_InboundFederation_QFormsRollBack(t *testing.T) {
	f := newInboundFix(t)
	f.identity(t, "ci-bot", "")
	first := f.trust(t, "first-trust", "ci-bot", nil)
	for _, commit := range []bool{false, true} {
		tx, err := f.pool.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.SetInboundFederationQ(f.ctx, tx, true, "admin"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.CreateInboundTrustQ(f.ctx, tx, f.newTrust(t, "held-trust", "ci-bot", "admin")); err != nil {
			t.Fatal(err)
		}
		if commit {
			err = tx.Commit(f.ctx)
		} else {
			err = tx.Rollback(f.ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
		sw, _ := f.st.GetInboundFederation(f.ctx)
		n := rowCount(t, f.pool, `SELECT count(*) FROM inbound_trusts WHERE name = 'held-trust'`)
		if sw.Enabled != commit || (n == 1) != commit {
			t.Fatalf("commit %v: switch %v, %d trusts; want both written only on commit", commit, sw.Enabled, n)
		}
	}
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	sameName := f.newTrust(t, "first-trust", "ci-bot", "admin")
	sameName.Subject = "repo:another"
	sameTriple := f.newTrust(t, "triple-trust", "ci-bot", "admin")
	sameTriple.Issuer, sameTriple.Audience, sameTriple.Subject = first.Issuer, first.Audience, first.Subject
	for _, in := range []types.InboundTrust{sameName, sameTriple} {
		if _, _, err := store.CreateInboundTrustQ(f.ctx, tx, in); !errors.Is(err, store.ErrInboundTrustExists) {
			t.Fatalf("held create %s = %v; want ErrInboundTrustExists", in.Name, err)
		}
		var one int
		if err := tx.QueryRow(f.ctx, `SELECT 1`).Scan(&one); err != nil {
			t.Fatalf("the caller's transaction after a refused create: %v", err)
		}
	}
}

// TestPG_InboundFederation_NarrowGrantRole runs every writer as a role that holds, on the three new tables, only what
// the migration's grant block gives an app role.
func TestPG_InboundFederation_NarrowGrantRole(t *testing.T) {
	ctx := context.Background()
	owner := databaseBefore(t, inboundMigration)
	role := "wardyn_inbound_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	for _, q := range []string{
		`CREATE ROLE ` + role + ` NOLOGIN`,
		`GRANT ` + role + ` TO current_user`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + role,
	} {
		if _, err := owner.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	t.Cleanup(func() {
		_, _ = owner.Exec(context.Background(), `DROP OWNED BY `+role)
		_, _ = owner.Exec(context.Background(), `DROP ROLE IF EXISTS `+role)
	})
	for _, name := range migrationFileNames(t) {
		if name >= inboundMigration {
			execMigrationFile(t, owner, name)
		}
	}
	if _, err := owner.Exec(ctx, `DO $$ DECLARE r record; BEGIN
		FOR r IN SELECT tablename FROM pg_tables WHERE schemaname = 'public'
			AND tablename NOT IN ('inbound_federation', 'inbound_trusts', 'inbound_trust_tokens') LOOP
			EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I TO %I', r.tablename, '`+role+`');
		END LOOP; END $$`); err != nil {
		t.Fatal(err)
	}
	cfg := owner.Config()
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, `SET ROLE `+role)
		return err
	}
	narrow, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(narrow.Close)

	f := inboundFixOn(t, store.NewPG(owner), owner)
	for _, s := range []string{"bot-delete", "bot-move", "bot-retire", "bot-refused"} {
		f.identity(t, s, "")
	}
	f.st = store.NewPG(narrow)
	step := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("as the app role, %s: %v", what, err)
		}
	}
	_, err = f.st.SetInboundFederation(ctx, true, "admin")
	step("turn the switch on", err)
	tr, _, err := f.st.CreateInboundTrust(ctx, f.newTrust(t, "narrow-trust", "bot-delete", "admin"))
	step("create a trust", err)
	_, err = exchange(t, f.st, tr)
	step("exchange", err)
	_, err = f.st.ConfirmInboundTrust(ctx, store.InboundTrustConfirm{ID: tr.ID, Service: "bot-delete", Sponsor: f.a, At: time.Now(), Seq: 1})
	step("confirm", err)
	_, _, err = f.st.DeleteInboundTrust(ctx, store.InboundTrustDelete{ID: tr.ID})
	step("delete the trust", err)
	for _, s := range []string{"bot-move", "bot-retire", "bot-refused"} {
		_, _, err = f.st.CreateInboundTrust(ctx, f.newTrust(t, s+"-trust", s, "admin"))
		step("create a trust on "+s, err)
	}
	step("change a sponsor", f.sponsor("bot-move", f.b))
	_, err = f.st.RetireServiceIdentity(ctx, "bot-retire", "admin")
	step("retire an identity", err)
	_, _, err = f.st.EraseServiceIdentityAttributionOf(ctx, "admin")
	step("erase a person", err)
	_, _, err = f.st.ProposeGovernanceChange(ctx, turnOnChange("alice"), time.Hour)
	step("propose a turn-on hold", err)
	set, err := f.st.SetInboundFederation(ctx, false, "admin")
	step("turn the switch off", err)
	if len(set.ChangesCancelled) != 1 {
		t.Fatalf("the switch-off cancelled %v; want the one turn-on", set.ChangesCancelled)
	}

	if _, err := owner.Exec(ctx, `REVOKE UPDATE ON inbound_trusts FROM `+role); err != nil {
		t.Fatal(err)
	}
	var pgErr *pgconn.PgError
	if err := f.sponsor("bot-refused", f.b); !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("a sponsor change without UPDATE on inbound_trusts = %v; want SQLSTATE 42501", err)
	}
}
