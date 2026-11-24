// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package spire_test

import (
	"context"
	"errors"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/identity/identitytest"
	"github.com/cjohnstoniv/wardyn/internal/identity/spire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The spire provider passes the shared identity conformance suite.
func TestSpire_Conformance(t *testing.T) {
	identitytest.RunConformance(t, func(t *testing.T, rev identity.RevocationStore) identity.Provider {
		return newHarness(t, rev, jose.ES256).p
	})
}

// S1, S2: go-spiffe's own parser accepts the token, the requested TTL is at most RunTokenTTL, and the
// minted Expiry is the token's exp.
func TestSpire_InteropAndTTL(t *testing.T) {
	h := newHarness(t, identitytest.NewMemRevocationStore(), jose.ES256)
	ri, err := h.p.MintRunIdentity(context.Background(), uuid.New(), "alice", "", aud, false)
	if err != nil {
		t.Fatal(err)
	}
	svid, err := jwtsvid.ParseAndValidate(ri.Token, h.srv.Source(), []string{aud})
	if err != nil || svid.ID.String() != ri.SPIFFEID {
		t.Fatalf("go-spiffe ParseAndValidate = (%v, %v), want the run's SPIFFE id", svid, err)
	}
	if !svid.Expiry.Equal(ri.Expiry) {
		t.Fatalf("minted expiry %v is not the token's exp %v", ri.Expiry, svid.Expiry)
	}
	parsed, _ := jwt.ParseSigned(ri.Token, []jose.SignatureAlgorithm{jose.ES256})
	var c jwt.Claims
	_ = parsed.UnsafeClaimsWithoutVerification(&c)
	if ttl := c.Expiry.Time().Sub(c.IssuedAt.Time()); ttl > identity.RunTokenTTL {
		t.Fatalf("the token lives %v, past RunTokenTTL", ttl)
	}
}

// S3, S4, S5, S14: a SPIRE-signed token is refused with no binding row (what a workload's own Workload
// API fetch returns), for a non-run ID, and for a foreign trust domain.
func TestSpire_RefusesUnboundAndForeignTokens(t *testing.T) {
	h := newHarness(t, identitytest.NewMemRevocationStore(), jose.ES256)
	run := uuid.New()
	now := time.Now()
	w := identity.Workload{Owner: "alice", Name: "nightly"}
	wpath, _ := identity.RunPath(w, run)
	for name, sub := range map[string]string{
		"S3_no_binding":           "spiffe://wardyn.local/agent-run/" + run.String(),
		"S14_own_fetch":           "spiffe://wardyn.local" + wpath,
		"S4_not_a_run":            "spiffe://wardyn.local/wardyn/control-plane",
		"S5_foreign_trust_domain": "spiffe://other.test/agent-run/" + run.String(),
	} {
		tok, err := h.srv.Sign(sub, aud, now, now.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.p.Verify(context.Background(), tok, aud); err == nil {
			t.Errorf("%s: a token for %s verified", name, sub)
		}
	}
}

// failPrune is a binding store whose prune alone fails.
type failPrune struct{ *identitytest.MemBindingStore }

func (failPrune) PruneBindings(context.Context, time.Time) (int64, error) {
	return 0, errors.New("prune failed")
}

// S6: no binding row, no token; a failed binding read is an ordinary error; a failed prune is
// housekeeping and the mint still returns its token.
func TestSpire_BindingFailures(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, identitytest.NewMemRevocationStore(), jose.ES256)
	ri, err := h.p.MintRunIdentity(ctx, uuid.New(), "alice", "", aud, false)
	if err != nil {
		t.Fatal(err)
	}
	h.bindings.SetErr(errors.New("store down"))
	if got, err := h.p.MintRunIdentity(ctx, uuid.New(), "alice", "", aud, false); err == nil || got.Token != "" {
		t.Fatalf("mint with a failing binding write = (%q, %v), want no token", got.Token, err)
	}
	var rev *identity.RevokedTokenError
	if _, err := h.p.Verify(ctx, ri.Token, aud); err == nil || errors.As(err, &rev) {
		t.Fatalf("verify with a failing binding read = %v, want an ordinary error", err)
	}
	fp := newHarness(t, identitytest.NewMemRevocationStore(), jose.ES256, withBindings(failPrune{identitytest.NewMemBindingStore()}))
	if got, err := fp.p.MintRunIdentity(ctx, uuid.New(), "alice", "", aud, false); err != nil || got.Token == "" {
		t.Fatalf("mint with a failing prune = (%q, %v), want its token", got.Token, err)
	}
}

// noteRevocations notes the denylist write in the fake's event log.
type noteRevocations struct {
	*identitytest.MemRevocationStore
	note func(string)
}

func (n noteRevocations) RevokeRun(ctx context.Context, run uuid.UUID) error {
	n.note("denylist " + run.String())
	return n.MemRevocationStore.RevokeRun(ctx, run)
}

// S7: one entry per run with the run's selectors, none on renew, deleted on revoke after the denylist.
func TestSpire_EntryLifecycle(t *testing.T) {
	ctx := context.Background()
	rev := noteRevocations{MemRevocationStore: identitytest.NewMemRevocationStore()}
	h := newHarness(t, &rev, jose.ES256)
	rev.note = h.srv.Note
	run := uuid.New()
	for i := 0; i < 2; i++ {
		if _, err := h.p.MintRunIdentity(ctx, run, "alice", "", aud, false); err != nil {
			t.Fatal(err)
		}
	}
	entries := h.srv.Entries()
	if len(entries) != 1 || h.srv.Calls().Create != 1 || entries[0].X509SVIDTTL != spire.EntryX509SVIDTTL {
		t.Fatalf("after mint and renew: %d entries, %d creates, want 1 and 1", len(entries), h.srv.Calls().Create)
	}
	want := map[string]bool{"ns:runs": true, "sa:wardyn-proxy": true, "pod-label:wardyn.component:proxy": true,
		"pod-label:wardyn.run-id:" + run.String(): true}
	for _, s := range entries[0].Selectors {
		delete(want, s.Value)
	}
	if len(want) != 0 || entries[0].ParentID.String() != "spiffe://wardyn.local/k8s-cluster/test" {
		t.Fatalf("entry %+v lacks selectors %v or the parent", entries[0], want)
	}
	if err := h.p.RevokeRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	ev := h.srv.Events()
	if len(h.srv.Entries()) != 0 || len(ev) != 3 || ev[1] != "denylist "+run.String() || ev[2] != "delete "+entries[0].ID {
		t.Fatalf("revoke events %v, want the denylist written before the entry delete", ev)
	}
	// A mint after the revoke (a renew or a queued start racing the stop) must not leave the entry the
	// revoke deleted re-created.
	if ri, err := h.p.MintRunIdentity(ctx, run, "alice", "", aud, false); err == nil || ri.Token != "" || len(h.srv.Entries()) != 0 {
		t.Fatalf("mint after RevokeRun = (%q, %v) with %d entries, want an error and no entry", ri.Token, err, len(h.srv.Entries()))
	}
}

// S8, S8b, S9: a server outage fails mint and renew closed and writes no row; the current token
// verifies from the cached bundle until its exp, then is ExpiredTokenError with the run; after the
// outage a renew mints under the same entry.
func TestSpire_ServerOutage(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, identitytest.NewMemRevocationStore(), jose.ES256)
	run := uuid.New()
	ri, err := h.p.MintRunIdentity(ctx, run, "alice", "", aud, false)
	if err != nil {
		t.Fatal(err)
	}
	if !ri.Expiry.After(time.Now()) || ri.Expiry.After(time.Now().Add(identity.RunTokenTTL+time.Second)) {
		t.Fatalf("expiry %v past the TTL ceiling", ri.Expiry)
	}
	h.srv.SetDown(true)
	rows := h.bindings.Len()
	if _, err := h.p.MintRunIdentity(ctx, uuid.New(), "alice", "", aud, false); err == nil {
		t.Fatal("a mint during an outage must fail closed")
	}
	if _, err := h.p.MintRunIdentity(ctx, run, "alice", "", aud, false); err == nil || h.bindings.Len() != rows {
		t.Fatalf("a renew during an outage = %v with %d rows (was %d), want an error and no row", err, h.bindings.Len(), rows)
	}
	if _, err := h.p.Verify(ctx, ri.Token, aud); err != nil {
		t.Fatalf("verify during the outage: %v", err)
	}
	h.clock.advance(identity.RunTokenTTL + 2*time.Minute)
	var exp *identity.ExpiredTokenError
	if _, err := h.p.Verify(ctx, ri.Token, aud); !errors.As(err, &exp) || exp.RunID != run {
		t.Fatalf("verify past exp = %v, want ExpiredTokenError for run %s", err, run)
	}
	h.clock.advance(-(identity.RunTokenTTL + 2*time.Minute))
	h.srv.SetDown(false)
	if _, err := h.p.MintRunIdentity(ctx, run, "alice", "", aud, false); err != nil || h.srv.Calls().Create != 1 {
		t.Fatalf("renew after the outage = %v with %d creates, want a mint under the same entry", err, h.srv.Calls().Create)
	}
}

// S11, S12: cloud_sts is refused, and mint, revoke and jti revoke are audited as embedded audits them.
func TestSpire_GrantsAndAudit(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, identitytest.NewMemRevocationStore(), jose.ES256)
	c := h.p.(interface{ CheckGrants([]types.GrantSpec) error })
	if c.CheckGrants([]types.GrantSpec{{Kind: types.GrantCloudSTS}}) == nil || c.CheckGrants(nil) != nil {
		t.Fatal("CheckGrants must refuse cloud_sts and only cloud_sts")
	}
	run := uuid.New()
	ri, err := h.p.MintRunIdentity(ctx, run, "alice", "", aud, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.p.(interface {
		RevokeJTI(context.Context, string, uuid.UUID) error
	}).RevokeJTI(ctx, ri.JTI, run); err != nil {
		t.Fatal(err)
	}
	if err := h.p.RevokeRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	want := []string{"identity.mint", "identity.jti.revoke", "identity.revoke"}
	evs := h.rec.events()
	if len(evs) != len(want) {
		t.Fatalf("audit events %+v, want %v", evs, want)
	}
	for i, ev := range evs {
		if ev.Action != want[i] || ev.Outcome != "success" || ev.ActorType != types.ActorSystem || ev.RunID == nil || *ev.RunID != run {
			t.Fatalf("event %d = %+v, want %s success for run %s", i, ev, want[i], run)
		}
	}
	if evs[0].Target != ri.JTI || evs[0].Actor != ri.SPIFFEID {
		t.Fatalf("mint event %+v does not name the token and the run", evs[0])
	}
}

// S13: a binding row cannot relabel a token. A token signed for workload A's run, with a binding row
// for workload B's run path, is refused; the workload claim is never read from the binding.
func TestSpire_BindingCannotRelabel(t *testing.T) {
	h := newHarness(t, identitytest.NewMemRevocationStore(), jose.ES256)
	run := uuid.New()
	pa, _ := identity.RunPath(identity.Workload{Owner: "alice", Name: "a"}, run)
	pb, _ := identity.RunPath(identity.Workload{Owner: "bob", Name: "b"}, run)
	now := time.Now()
	tok, err := h.srv.Sign("spiffe://wardyn.local"+pa, aud, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	b := identity.TokenBinding{JTI: jtiOf(tok), RunID: run, SPIFFEID: "spiffe://wardyn.local" + pb, Sub: "bob", Sponsor: "bob", Expiry: now.Add(time.Hour)}
	if err := h.bindings.PutBinding(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if cl, err := h.p.Verify(context.Background(), tok, aud); err == nil {
		t.Fatalf("a token for workload a verified through a binding for workload b: %+v", cl)
	}
}

// S18: the host sensor's uuid.Nil mint finds and creates no entry, verifies, and writes one row each.
func TestSpire_SensorMintMakesNoEntry(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, identitytest.NewMemRevocationStore(), jose.ES256)
	for i := 1; i <= 2; i++ {
		ri, err := h.p.MintRunIdentity(ctx, uuid.Nil, "wardyn-tetragon-ingest", "", "wardyn-groundtruth", false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.p.Verify(ctx, ri.Token, "wardyn-groundtruth"); err != nil {
			t.Fatalf("the sensor token does not verify: %v", err)
		}
		if c := h.srv.Calls(); c.Find != 0 || c.Create != 0 || h.bindings.Len() != i || len(h.srv.Entries()) != 0 {
			t.Fatalf("after %d sensor mints: calls %+v, %d rows, %d entries; want no entry call and %d rows", i, c, h.bindings.Len(), len(h.srv.Entries()), i)
		}
	}
	if err := h.p.RevokeRun(ctx, uuid.Nil); err != nil || h.srv.Calls().Find != 0 {
		t.Fatalf("revoke of the sensor run = %v with calls %+v, want no entry call", err, h.srv.Calls())
	}
}
