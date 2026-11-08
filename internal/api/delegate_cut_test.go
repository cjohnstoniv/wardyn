// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/gatewayidentity"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// GetDelegateByID makes the gateway fixture's store a store.DelegateReader over its portals.
func (s *gatewayStore) GetDelegateByID(_ context.Context, id uuid.UUID) (types.Delegate, error) {
	s.mu.Lock()
	err := s.delErr
	s.mu.Unlock()
	if err != nil {
		return types.Delegate{}, err
	}
	s.fakeDelegateStore.mu.Lock()
	defer s.fakeDelegateStore.mu.Unlock()
	d, ok := s.fakeDelegateStore.delegates[id]
	if !ok {
		return types.Delegate{}, store.ErrNotFound
	}
	return d, nil
}

// delegatedRun registers a portal and makes the fixture's running run one it launched.
func (f *gatewayFixture) delegatedRun(t *testing.T) types.Delegate {
	t.Helper()
	d, err := f.st.fakeDelegateStore.CreateDelegate(context.Background(),
		types.Delegate{ID: uuid.New(), Name: "portal", IdPClientID: "portal-client", Group: "portal-users"}, "wdp_"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	id := d.ID
	f.run.CreatedVia = &id
	f.st.set(func(s *gatewayStore) { s.run.CreatedVia = &id })
	return d
}

func (f *gatewayFixture) revokeDelegate(t *testing.T, id uuid.UUID, query string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, f.srv, http.MethodDelete, "/api/v1/admin/delegates/"+id.String()+query, adminToken, "")
}

func (f *gatewayFixture) revokeRows(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ev := range f.h.audit.snapshot() {
		if ev.Action == "delegate.revoke" {
			out = append(out, auditData(t, ev))
		}
	}
	return out
}

func (f *gatewayFixture) cutRows(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ev := range f.mintEvents() {
		if d := auditData(t, ev); ev.Outcome == "denied" && d["reason"] == reasonGatewayAccessCut {
			out = append(out, d)
		}
	}
	return out
}

// wantCut fails unless w is the cut: 403 whose body's reason is gateway_access_cut, small enough
// for the proxy's bounded read of it.
func wantCut(t *testing.T, what string, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusForbidden || reasonOf(t, w) != reasonGatewayAccessCut {
		t.Fatalf("%s after the cut = %d %s, want 403 %s (the mint did not read the portal's cut)", what, w.Code, w.Body, reasonGatewayAccessCut)
	}
	if w.Body.Len() > 1<<10 {
		t.Errorf("%s: refusal body is %d bytes, over the 1 KiB the proxy reads", what, w.Body.Len())
	}
}

// A cut revoke answers 204, writes one row that says so and counts the portal's runs, and from then
// on every provider and MCP mint of its running run is refused, with one denied row per destination
// on this replica however often the proxy asks.
func TestRevokeDelegateCutInFlight(t *testing.T) {
	f := newGatewayFixture(t)
	f.sealMCP(t, testGatewayMCPComponent())
	d := f.delegatedRun(t)
	f.st.fakeDelegateStore.inFlight = map[uuid.UUID]int{d.ID: 2}
	if w := f.mint(t); w.Code != http.StatusOK {
		t.Fatalf("mint before the revoke = %d %s", w.Code, w.Body)
	}
	if w := f.revokeDelegate(t, d.ID, "?cut_in_flight=true"); w.Code != http.StatusNoContent {
		t.Fatalf("cut revoke = %d %s, want 204", w.Code, w.Body)
	}
	rows := f.revokeRows(t)
	if len(rows) != 1 || rows[0]["cut_in_flight"] != true || rows[0]["runs_in_flight"] != float64(2) || rows[0]["already_revoked"] != nil {
		t.Fatalf("delegate.revoke rows = %v, want one with cut_in_flight and runs_in_flight 2", rows)
	}
	for range 3 {
		wantCut(t, "provider mint", f.mint(t))
		wantCut(t, "MCP mint", f.mintMCP(t, "tools"))
	}
	cuts := f.cutRows(t)
	if len(cuts) != 2 {
		t.Fatalf("cut denied rows = %d %v, want one per destination on this replica", len(cuts), cuts)
	}
	for _, c := range cuts {
		if c["cause"] != "delegate_revoked" || c["delegate"] != d.ID.String() {
			t.Errorf("cut row %v, want cause delegate_revoked naming the portal", c)
		}
	}
	if cuts[0]["provider"] != "corp-gw" || cuts[1]["mcp_server"] != "tools" {
		t.Errorf("cut rows %v, want the provider's then the MCP server's", cuts)
	}
}

// Without the option a revoke is today's: the portal's runs keep their gateway access, and the row
// carries no cut.
func TestRevokeDelegateWithoutCutKeepsGateway(t *testing.T) {
	for _, q := range []string{"", "?cut_in_flight=false"} {
		t.Run(q, func(t *testing.T) {
			f := newGatewayFixture(t)
			d := f.delegatedRun(t)
			if w := f.revokeDelegate(t, d.ID, q); w.Code != http.StatusNoContent {
				t.Fatalf("revoke = %d %s, want 204", w.Code, w.Body)
			}
			if w := f.mint(t); w.Code != http.StatusOK {
				t.Fatalf("mint after a plain revoke = %d %s, want 200", w.Code, w.Body)
			}
			if rows := f.revokeRows(t); len(rows) != 1 || rows[0]["cut_in_flight"] != nil || rows[0]["runs_in_flight"] != nil {
				t.Errorf("delegate.revoke rows = %v, want one without a cut", rows)
			}
			if got, _ := f.st.GetDelegateByID(context.Background(), d.ID); got.RevokedAt == nil || got.CutInFlightAt != nil {
				t.Errorf("portal after a plain revoke = %+v, want revoked and not cut", got)
			}
		})
	}
}

// An admin can cut after revoking: the cut answers 204 and writes a row saying the portal was already
// revoked; the mint is refused from then on; a third call is 404 and writes nothing.
func TestRevokeDelegateCutAfterRevoke(t *testing.T) {
	f := newGatewayFixture(t)
	d := f.delegatedRun(t)
	if w := f.revokeDelegate(t, d.ID, ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", w.Code, w.Body)
	}
	if w := f.mint(t); w.Code != http.StatusOK {
		t.Fatalf("mint after the plain revoke = %d %s", w.Code, w.Body)
	}
	f.now = f.now.Add(time.Minute) // the cut is a later call
	if w := f.revokeDelegate(t, d.ID, "?cut_in_flight=true"); w.Code != http.StatusNoContent {
		t.Fatalf("cut of a revoked portal = %d %s, want 204", w.Code, w.Body)
	}
	wantCut(t, "provider mint", f.mint(t))
	for _, q := range []string{"?cut_in_flight=true", ""} {
		if w := f.revokeDelegate(t, d.ID, q); w.Code != http.StatusNotFound || reasonOf(t, w) != reasonDelegateNotFound {
			t.Errorf("repeat %q = %d %s, want 404", q, w.Code, w.Body)
		}
	}
	rows := f.revokeRows(t)
	if len(rows) != 2 || rows[1]["already_revoked"] != true || rows[1]["cut_in_flight"] != true {
		t.Fatalf("delegate.revoke rows = %v, want the revoke and then the cut marked already_revoked", rows)
	}
	w := do(t, f.srv, http.MethodGet, "/api/v1/admin/delegates", adminToken, "")
	var listed []types.Delegate
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil || len(listed) != 1 || listed[0].CutInFlightAt == nil {
		t.Errorf("list = %s, want the portal with cut_in_flight_at (err %v)", w.Body, err)
	}
}

// Any value of cut_in_flight but true or false is refused before anything is written.
func TestRevokeDelegateCutBadValue(t *testing.T) {
	f := newGatewayFixture(t)
	d := f.delegatedRun(t)
	for _, q := range []string{"?cut_in_flight=yes", "?cut_in_flight=1", "?cut_in_flight=TRUE", "?cut_in_flight=", "?cut_in_flight=true&cut_in_flight=false"} {
		if w := f.revokeDelegate(t, d.ID, q); w.Code != http.StatusBadRequest || reasonOf(t, w) != reasonInvalidCutInFlightParam {
			t.Errorf("revoke %q = %d %s, want 400 %s", q, w.Code, w.Body, reasonInvalidCutInFlightParam)
		}
	}
	if got, _ := f.st.GetDelegateByID(context.Background(), d.ID); got.RevokedAt != nil || got.CutInFlightAt != nil {
		t.Errorf("a refused revoke changed the portal: %+v", got)
	}
	if rows := f.revokeRows(t); len(rows) != 0 {
		t.Errorf("refused revokes wrote %v", rows)
	}
}

// A query the server cannot parse is refused before anything is written: a malformed value, a
// malformed duplicate beside a good one, and a semicolon separator would otherwise be dropped and
// read as a plain revoke that leaves the runs their gateway access.
func TestRevokeDelegateCutMalformedQuery(t *testing.T) {
	for _, q := range []string{"?cut_in_flight=%74rue%ZZ", "?cut_in_flight=true&cut_in_flight=%ZZ", "?cut_in_flight=true;x=1"} {
		t.Run(q, func(t *testing.T) {
			f := newGatewayFixture(t)
			d := f.delegatedRun(t)
			if w := f.revokeDelegate(t, d.ID, q); w.Code != http.StatusBadRequest || reasonOf(t, w) != reasonInvalidCutInFlightParam {
				t.Fatalf("revoke = %d %s, want 400 %s (a dropped parameter read as a plain revoke)", w.Code, w.Body, reasonInvalidCutInFlightParam)
			}
			if got, err := f.st.GetDelegateByID(context.Background(), d.ID); err != nil || got.RevokedAt != nil || got.CutInFlightAt != nil {
				t.Errorf("portal after a refused revoke = %+v %v, want live and not cut", got, err)
			}
			if rows := f.revokeRows(t); len(rows) != 0 {
				t.Errorf("refused revoke wrote %v", rows)
			}
		})
	}
}

// A delegated run's mint that cannot read its portal fails closed; a portal that is gone refuses as
// a cut; a run a person launched themselves reads nothing.
func TestGatewayMintViaStoreFailureFailsClosed(t *testing.T) {
	f := newGatewayFixture(t)
	f.st.set(func(s *gatewayStore) { s.delErr = errors.New("db down") })
	if w := f.mint(t); w.Code != http.StatusOK {
		t.Fatalf("self-launched run's mint = %d %s, want 200 (no portal read)", w.Code, w.Body)
	}
	f.delegatedRun(t)
	if w := f.mint(t); w.Code != http.StatusServiceUnavailable || reasonOf(t, w) != reasonGatewayIdentityUnavailable {
		t.Fatalf("mint with the portal unreadable = %d %s, want 503 %s", w.Code, w.Body, reasonGatewayIdentityUnavailable)
	}
	gone := uuid.New()
	f.st.set(func(s *gatewayStore) { s.delErr, s.run.CreatedVia = nil, &gone })
	wantCut(t, "mint of a run whose portal is gone", f.mint(t))
}

// A revive of a run whose portal cut its gateway access is refused with the reason and the new-run
// step; a plain revoke does not refuse it; a portal that cannot be read refuses as unreadable; a run
// with no gateway route reads nothing.
func TestGatewayReviveRefusesACutRun(t *testing.T) {
	f := newGatewayFixture(t)
	d := f.delegatedRun(t)
	rt, _ := gatewayRouteOf(testGatewayProvider())
	cfg := &proxy.Config{RunID: f.run.ID, GatewayIdentity: []proxy.GatewayIdentityRoute{rt}}
	if w := f.revokeDelegate(t, d.ID, ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", w.Code, w.Body)
	}
	if ref, err := f.srv.gatewayIdentityRefusal(context.Background(), cfg); ref != nil || err != nil {
		t.Fatalf("plain revoke: revive refused %+v %v", ref, err)
	}
	if w := f.revokeDelegate(t, d.ID, "?cut_in_flight=true"); w.Code != http.StatusNoContent {
		t.Fatalf("cut = %d %s", w.Code, w.Body)
	}
	ref, err := f.srv.gatewayIdentityRefusal(context.Background(), cfg)
	if err != nil || ref == nil || ref.status != http.StatusConflict || ref.reason != reasonGatewayAccessCut || !strings.Contains(ref.msg, "Start a new run") {
		t.Fatalf("revive of a cut run = %+v %v, want 409 %s with the new-run step", ref, err, reasonGatewayAccessCut)
	}
	if rev := f.srv.reviveOwnerRecheck(context.Background(), f.run, cfg, types.ActorSystem, "wardynd"); rev == nil || rev.reason != reasonGatewayAccessCut {
		t.Errorf("revive recheck = %+v", rev)
	}
	f.st.set(func(s *gatewayStore) { s.delErr = errors.New("db down") })
	if _, err := f.srv.gatewayIdentityRefusal(context.Background(), cfg); err == nil {
		t.Error("an unreadable portal passed the revive")
	}
	if ref, err := f.srv.gatewayIdentityRefusal(context.Background(), &proxy.Config{RunID: f.run.ID}); ref != nil || err != nil {
		t.Errorf("no route read the portal: %v %v", ref, err)
	}
}

// The sponsor rows of the "when a run's gateway access ends" table: a sponsor's full revocation
// refuses a service run's mint; a handover keeps minting, naming the new sponsor.
func TestGatewayMintSponsorEvents(t *testing.T) {
	f := newGatewayFixture(t)
	f.token = mintRunTokenAs(t, f.h, f.run.ID, types.ServicePrincipalPrefix+"ci-bot")
	sponsorOf := func(w *httptest.ResponseRecorder) string {
		t.Helper()
		if w.Code != http.StatusOK {
			t.Fatalf("mint = %d %s, want 200", w.Code, w.Body)
		}
		var body struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		parsed, err := jwt.ParseSigned(body.Token, []jose.SignatureAlgorithm{jose.ES256})
		if err != nil {
			t.Fatal(err)
		}
		var c gatewayidentity.Claims
		if err := parsed.Claims(testGatewayKeySet(t, f.signer), &c); err != nil {
			t.Fatal(err)
		}
		return c.Sponsor
	}
	f.st.set(func(s *gatewayStore) { s.service = types.ServiceIdentity{Name: "ci-bot", Sponsor: "old@example.com"} })
	if got := sponsorOf(f.mint(t)); got != "old@example.com" {
		t.Fatalf("sponsor = %q, want old@example.com", got)
	}
	f.st.set(func(s *gatewayStore) { s.service.Sponsor = "new@example.com" })
	if got := sponsorOf(f.mint(t)); got != "new@example.com" {
		t.Fatalf("after the handover the token names %q, want new@example.com", got)
	}
	f.st.set(func(s *gatewayStore) { s.blockedWho = "new@example.com" })
	if w := f.mint(t); w.Code != http.StatusForbidden || reasonOf(t, w) != reasonIdentityDeactivated {
		t.Fatalf("mint after the sponsor's full revocation = %d %s, want 403 %s", w.Code, w.Body, reasonIdentityDeactivated)
	}
}
