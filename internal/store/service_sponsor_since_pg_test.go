// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A sponsor handed to B and back to A is a new sponsorship: a stream opened under the first one is no
// longer sponsored (sponsor_since moved past its open time), though the sponsor's name reads the same.
// Guarded by WARDYN_TEST_PG.
func TestPG_ServiceStillSponsored_ASponsorHandedAwayAndBackCutsAnEarlierStream(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	const a, b = "sub-sponsor-a", "sub-sponsor-b"
	for _, p := range []string{a, b} {
		if _, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: p, Issuer: "https://issuer.test", Email: p + "@corp.example"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.CreateServiceIdentity(ctx, types.ServiceIdentity{Name: "ci-bot", Sponsor: a, UserType: types.UserTypeStandard, CreatedBy: a}); err != nil {
		t.Fatal(err)
	}
	opened := time.Now().UTC()
	if ok, err := st.ServiceStillSponsored(ctx, "ci-bot", a, opened); err != nil || !ok {
		t.Fatalf("before any change = %v, %v; want still sponsored", ok, err)
	}
	for _, to := range []string{b, a} {
		if _, err := st.UpdateServiceIdentity(ctx, "ci-bot", store.ServiceIdentityChange{Sponsor: &to, By: "admin"}); err != nil {
			t.Fatalf("hand to %s: %v", to, err)
		}
	}
	if ok, err := st.ServiceStillSponsored(ctx, "ci-bot", a, opened); err != nil || ok {
		t.Errorf("stream opened before the A to B to A hand-over = %v, %v; want it cut", ok, err)
	}
	if ok, err := st.ServiceStillSponsored(ctx, "ci-bot", a, time.Now().UTC()); err != nil || !ok {
		t.Errorf("stream opened after the hand-over = %v, %v; want still sponsored", ok, err)
	}
}

// A service identity's own write (IdentityGuard.ServiceSince) holds the identity's row FOR SHARE while it
// runs, so a retirement, which takes the row FOR UPDATE, cannot pass it; and the write is refused once the
// identity is suspended or retired or its sponsor was handed on after the sponsor_since it was admitted
// against, though the sponsor's name reads the same again. Guarded by WARDYN_TEST_PG.
func TestPG_ServiceWriteHeldToItsAdmission(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	const a, b = "sub-sponsor-a", "sub-sponsor-b"
	for _, p := range []string{a, b} {
		if _, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: p, Issuer: "https://issuer.test", Email: p + "@corp.example"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	si, err := st.CreateServiceIdentity(ctx, types.ServiceIdentity{Name: "ci-bot", Sponsor: a, UserType: types.UserTypeStandard, CreatedBy: a})
	if err != nil {
		t.Fatal(err)
	}
	g := store.IdentityGuard{Principal: si.Principal(), Epoch: -1, ServiceSince: si.SponsorSince}
	write := func() error { return st.WithIdentityShared(ctx, g, func() error { return nil }) }
	held := st.WithIdentityShared(ctx, g, func() error {
		_, err := pool.Exec(ctx, `SELECT 1 FROM service_identities WHERE name = 'ci-bot' FOR UPDATE NOWAIT`)
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(held, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("taking the identity's row FOR UPDATE while its write runs = %v, want lock_not_available (55P03)", held)
	}
	change := func(ch store.ServiceIdentityChange) {
		t.Helper()
		ch.By = "admin"
		if _, err := st.UpdateServiceIdentity(ctx, "ci-bot", ch); err != nil {
			t.Fatal(err)
		}
	}
	yes, no := true, false
	change(store.ServiceIdentityChange{Suspended: &yes})
	if err := write(); !errors.Is(err, store.ErrIdentityDeactivated) {
		t.Errorf("a write after a suspension = %v, want ErrIdentityDeactivated", err)
	}
	change(store.ServiceIdentityChange{Suspended: &no})
	if err := write(); err != nil {
		t.Errorf("a write after the suspension ended = %v, want it admitted", err)
	}
	for _, to := range []string{b, a} {
		change(store.ServiceIdentityChange{Sponsor: &to})
	}
	if err := write(); !errors.Is(err, store.ErrIdentityDeactivated) {
		t.Errorf("a write admitted before the sponsor was handed to B and back = %v, want ErrIdentityDeactivated", err)
	}
	cur, err := st.RetireServiceIdentity(ctx, "ci-bot", "admin")
	if err != nil {
		t.Fatal(err)
	}
	g.ServiceSince = cur.After.SponsorSince
	if err := write(); !errors.Is(err, store.ErrIdentityDeactivated) {
		t.Errorf("a write after the retirement = %v, want ErrIdentityDeactivated", err)
	}
}

// sponsorTicketFixture is a service identity "ci-bot" sponsored by a, with b and c as other persons, a run the
// bot created and a run a created.
type sponsorTicketFixture struct {
	st        store.PG
	botRun    types.AgentRun
	ownRun    types.AgentRun
	a, b, c   string
	ctx       context.Context
	t         *testing.T
	expiresAt time.Time
	mintedNow time.Time
}

func newSponsorTicketFixture(t *testing.T) *sponsorTicketFixture {
	t.Helper()
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	f := &sponsorTicketFixture{st: st, ctx: ctx, t: t, a: "sub-sponsor-a", b: "sub-sponsor-b", c: "sub-sponsor-c"}
	for _, p := range []string{f.a, f.b, f.c} {
		if _, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: p, Issuer: "https://issuer.test", Email: p + "@corp.example"}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.CreateServiceIdentity(ctx, types.ServiceIdentity{Name: "ci-bot", Sponsor: f.a, UserType: types.UserTypeStandard, CreatedBy: f.a}); err != nil {
		t.Fatal(err)
	}
	bot := newRun(types.RunRunning)
	bot.CreatedBy = types.ServicePrincipalPrefix + "ci-bot"
	f.botRun = persistRun(t, ctx, pool, bot)
	own := newRun(types.RunRunning)
	own.CreatedBy = f.a
	f.ownRun = persistRun(t, ctx, pool, own)
	f.mintedNow = time.Now().UTC()
	f.expiresAt = f.mintedNow.Add(30 * time.Second)
	return f
}

// mint writes one ticket for principal on run (sponsorService, when set, holds the identity to principal) and returns
// its token and the store's answer.
func (f *sponsorTicketFixture) mint(run types.AgentRun, principal, sponsorService string, support bool) (string, error) {
	f.t.Helper()
	tk := store.AttachTicket{RunID: run.ID, ActorType: types.ActorHuman, Principal: principal, Role: "user", SponsorService: sponsorService}
	if support {
		tk.SupportGrant, tk.SupportExpires = uuid.New(), f.expiresAt
	}
	tok := uuid.NewString()
	return tok, f.st.MintAttachTicket(f.ctx, tok, tk, f.mintedNow, f.expiresAt)
}

func (f *sponsorTicketFixture) mustMint(run types.AgentRun, principal, sponsorService string, support bool) string {
	f.t.Helper()
	tok, err := f.mint(run, principal, sponsorService, support)
	if err != nil {
		f.t.Fatalf("mint for %s: %v", principal, err)
	}
	return tok
}

func (f *sponsorTicketFixture) redeems(tok string) bool {
	f.t.Helper()
	_, ok, err := f.st.ConsumeAttachTicket(f.ctx, tok, f.mintedNow)
	if err != nil {
		f.t.Fatalf("consume: %v", err)
	}
	return ok
}

func (f *sponsorTicketFixture) count(principal string) int {
	f.t.Helper()
	var n int
	row := f.st.Pool.QueryRow(f.ctx, `SELECT count(*) FROM attach_tickets WHERE principal = $1`, principal)
	if err := row.Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// A sponsor change deletes the previous sponsor's tickets on the identity's runs and nothing else: not a
// support ticket, not another person's ticket on the run, not the previous sponsor's ticket on a run the
// identity did not create. A description-only change deletes nothing. Guarded by WARDYN_TEST_PG.
func TestPG_ServiceSponsorChangeDeletesThePreviousSponsorsTickets(t *testing.T) {
	f := newSponsorTicketFixture(t)
	gone := f.mustMint(f.botRun, f.a, "", false)
	support := f.mustMint(f.botRun, f.a, "", true)
	other := f.mustMint(f.botRun, f.c, "", false)
	own := f.mustMint(f.ownRun, f.a, "", false)
	if _, err := f.st.UpdateServiceIdentity(f.ctx, "ci-bot", store.ServiceIdentityChange{Sponsor: &f.b, By: "admin"}); err != nil {
		t.Fatal(err)
	}
	if f.redeems(gone) {
		t.Error("the previous sponsor's ticket on the bot's run redeemed after the sponsor change")
	}
	for name, tok := range map[string]string{"support ticket": support, "another person's ticket": other, "ticket on the previous sponsor's own run": own} {
		if !f.redeems(tok) {
			t.Errorf("%s was deleted by the sponsor change", name)
		}
	}
	kept := f.mustMint(f.botRun, f.b, "", false)
	desc := "a new description"
	if _, err := f.st.UpdateServiceIdentity(f.ctx, "ci-bot", store.ServiceIdentityChange{Description: &desc, By: "admin"}); err != nil {
		t.Fatal(err)
	}
	if !f.redeems(kept) {
		t.Error("a description-only change deleted the sponsor's ticket")
	}
}

// A sponsor-arm mint (SponsorService set) inserts only while the identity is usable and the minter is still its
// sponsor; the field alone gates, so a mint without it is unchanged. Guarded by WARDYN_TEST_PG.
func TestPG_SponsorMintRefusedOnceTheSponsorChanged(t *testing.T) {
	f := newSponsorTicketFixture(t)
	f.mustMint(f.botRun, f.a, "ci-bot", false)
	if _, err := f.st.UpdateServiceIdentity(f.ctx, "ci-bot", store.ServiceIdentityChange{Sponsor: &f.b, By: "admin"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mint(f.botRun, f.a, "ci-bot", false); !errors.Is(err, store.ErrServiceIdentityInactive) {
		t.Fatalf("mint by the previous sponsor = %v, want ErrServiceIdentityInactive", err)
	}
	if n := f.count(f.a); n != 0 {
		t.Errorf("tickets for the previous sponsor after the refused mint = %d, want 0 (the move deleted the first, the refusal wrote none)", n)
	}
	f.mustMint(f.botRun, f.b, "ci-bot", false)
	yes := true
	if _, err := f.st.UpdateServiceIdentity(f.ctx, "ci-bot", store.ServiceIdentityChange{Suspended: &yes, By: "admin"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mint(f.botRun, f.b, "ci-bot", false); !errors.Is(err, store.ErrServiceIdentityInactive) {
		t.Errorf("mint on a suspended identity = %v, want ErrServiceIdentityInactive", err)
	}
	if _, err := f.mint(f.botRun, f.b, "no-such", false); !errors.Is(err, store.ErrServiceIdentityInactive) {
		t.Errorf("mint on an unknown identity = %v, want ErrServiceIdentityInactive", err)
	}
	if _, err := f.mint(f.botRun, f.a, "", false); err != nil {
		t.Errorf("mint without SponsorService = %v, want it written", err)
	}
}
