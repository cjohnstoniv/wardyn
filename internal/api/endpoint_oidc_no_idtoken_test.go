// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A custom_endpoint row with identity_binding none and require_id_token false signs in against an authority
// that never grants openid. These tests pin every place the mode is read: the validation, the address and
// purge, the scopes asked for, the signed state, the callback, the capture's audit row and the redemption.

// noIDTokenRow edits a row's block to the unbound, no-identity-token mode.
func noIDTokenRow(row types.ModelProvider) types.ModelProvider {
	return withOIDC(row, func(o *types.ProviderOIDC) {
		o.IdentityBinding, o.RequireIDToken = types.OIDCBindingNone, boolPtr(false)
	})
}

// oidcAddress0100 is oidcAddress exactly as 0.10.0 wrote it: no require_id_token in it.
func oidcAddress0100(p types.ModelProvider) string {
	o := p.OIDC
	if o == nil {
		return ""
	}
	return string(mustJSON([]any{o.Issuer, o.AuthorizeURL, o.TokenURL, o.ClientID, slices.Sorted(slices.Values(o.Scopes)), o.Binding()}))
}

func TestEndpointOIDCNoIDToken_AddressBytesOfDefaultRowsAreUnchanged(t *testing.T) {
	const golden = `["https://idp.corp.example","","","wardyn-client",["api.read","api.write"],"console"]`
	const goldenNone = `["https://idp.corp.example","","","wardyn-client",["api.read","api.write"],"none"]`
	for name, tc := range map[string]struct {
		row  types.ModelProvider
		want string
	}{
		"omitted":               {oidcRow(), golden},
		"true":                  {withOIDC(oidcRow(), func(o *types.ProviderOIDC) { o.RequireIDToken = boolPtr(true) }), golden},
		"none binding, omitted": {withOIDC(oidcRow(), func(o *types.ProviderOIDC) { o.IdentityBinding = types.OIDCBindingNone }), goldenNone},
		"none binding, true": {withOIDC(oidcRow(), func(o *types.ProviderOIDC) {
			o.IdentityBinding, o.RequireIDToken = types.OIDCBindingNone, boolPtr(true)
		}), goldenNone},
		"console binding, omitted": {withOIDC(oidcRow(), func(o *types.ProviderOIDC) { o.IdentityBinding = types.OIDCBindingConsole }), golden},
	} {
		if got := oidcAddress(tc.row); got != tc.want || got != oidcAddress0100(tc.row) {
			t.Errorf("%s: oidcAddress = %s, want the 0.10.0 bytes %s (0.10.0 function: %s)", name, got, tc.want, oidcAddress0100(tc.row))
		}
	}
	// The whole row digest, which a state stamp and a capture digest hash, does not move either.
	if providerAddressDigest(oidcRow()) != providerAddressDigest(withOIDC(oidcRow(), func(o *types.ProviderOIDC) { o.RequireIDToken = boolPtr(true) })) {
		t.Error("an explicit true moved the provider digest of a default row")
	}
	off := noIDTokenRow(oidcRow())
	if got := oidcAddress(off); got == oidcAddress0100(off) || !strings.HasSuffix(got, `,"`+oidcNoIDTokenMarker+`"]`) {
		t.Errorf("false appended no distinct marker: %s", got)
	}
}

func TestEndpointOIDCNoIDToken_TransitionsInBothDirectionsChangeTheAddressAndPurge(t *testing.T) {
	// Every row here has binding none, so the mode is the only thing that differs inside a pair.
	noneOmitted := withOIDC(oidcRow(), func(o *types.ProviderOIDC) { o.IdentityBinding = types.OIDCBindingNone })
	noneTrue := withOIDC(oidcRow(), func(o *types.ProviderOIDC) {
		o.IdentityBinding = types.OIDCBindingNone
		o.RequireIDToken = boolPtr(true)
	})
	off := noIDTokenRow(oidcRow())
	if oidcAddress(noneOmitted) != oidcAddress(noneTrue) {
		t.Fatal("omitted and true differ")
	}
	for name, pair := range map[string][2]types.ModelProvider{
		"on to off": {noneOmitted, off}, "off to on": {off, noneOmitted}, "off to true": {off, noneTrue},
	} {
		a, b := pair[0], pair[1]
		if oidcAddress(a) == oidcAddress(b) || providerAddressDigest(a) == providerAddressDigest(b) || !providerAddressChanged(a, b) {
			t.Errorf("%s: the address, the digest or rule 8 did not move", name)
		}
		if !slices.Contains(invalidatedProviderUIDs(providerBlock(a), providerBlock(b)), oidcUID) {
			t.Errorf("%s: the row's credentials are not invalidated", name)
		}
	}
	// And the purge itself deletes a stored sign-in, in both directions.
	for name, toOff := range map[string]bool{"on to off": true, "off to on": false} {
		f := newOIDCFlow(t, types.OIDCBindingNone)
		on, off := f.row, noIDTokenRow(f.row)
		from, to := on, off
		if !toOff {
			from, to = off, on
		}
		f.row = from
		f.site.set(func(b *types.ModelProviders) { b.Providers[0] = from })
		f.mustSignIn(t, oidcPersonSub)
		if f.rawSignIn(t, oidcPersonSub) == nil {
			t.Fatalf("%s: setup: nothing stored", name)
		}
		purgeChangedProvider(t, f.srv, from, to)
		if f.rawSignIn(t, oidcPersonSub) != nil {
			t.Errorf("%s: the stored sign-in survived the mode change", name)
		}
	}
}

func TestEndpointOIDCNoIDToken_ValidationTable(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*types.ProviderOIDC)
		want string // "" = accepted; else a substring of the refusal
	}{
		{"false with none", func(o *types.ProviderOIDC) {
			o.IdentityBinding, o.RequireIDToken = types.OIDCBindingNone, boolPtr(false)
		}, ""},
		{"true with none", func(o *types.ProviderOIDC) {
			o.IdentityBinding, o.RequireIDToken = types.OIDCBindingNone, boolPtr(true)
		}, ""},
		{"true with console", func(o *types.ProviderOIDC) {
			o.IdentityBinding, o.RequireIDToken = types.OIDCBindingConsole, boolPtr(true)
		}, ""},
		{"omitted with console", func(o *types.ProviderOIDC) { o.IdentityBinding = types.OIDCBindingConsole }, ""},
		{"false with the console binding", func(o *types.ProviderOIDC) {
			o.IdentityBinding, o.RequireIDToken = types.OIDCBindingConsole, boolPtr(false)
		}, "require_id_token"},
		{"false with the default binding", func(o *types.ProviderOIDC) { o.RequireIDToken = boolPtr(false) }, "require_id_token"},
		{"false with openid in scopes", func(o *types.ProviderOIDC) {
			o.IdentityBinding, o.RequireIDToken, o.Scopes = types.OIDCBindingNone, boolPtr(false), []string{"api.read", "openid"}
		}, "openid"},
		{"true with openid in scopes", func(o *types.ProviderOIDC) {
			o.IdentityBinding, o.RequireIDToken, o.Scopes = types.OIDCBindingNone, boolPtr(true), []string{"openid"}
		}, ""},
		{"omitted with openid in scopes", func(o *types.ProviderOIDC) { o.Scopes = []string{"openid"} }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := withOIDC(oidcRow(), tc.edit)
			_, err := validateModelProviders(providerBlock(p), providerWriteEnv{})
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

func TestEndpointOIDCNoIDToken_ScopesAreBuiltWithoutOpenID(t *testing.T) {
	if got := endpointOIDCRequestedScopes(noIDTokenRow(oidcRow()).OIDC); !slices.Equal(got, []string{entraOfflineAccessScope, "api.read", "api.write"}) {
		t.Errorf("false asks for %v, want offline_access then the row's scopes and no openid", got)
	}
	for name, o := range map[string]*types.ProviderOIDC{"omitted": oidcRow().OIDC, "true": withOIDC(oidcRow(), func(o *types.ProviderOIDC) { o.RequireIDToken = boolPtr(true) }).OIDC} {
		if got := endpointOIDCRequestedScopes(o); !slices.Equal(got, []string{entraOpenIDScope, entraOfflineAccessScope, "api.read", "api.write"}) {
			t.Errorf("%s asks for %v, want the 0.10.0 set", name, got)
		}
	}
	// On the wire: the authorization request the door redirects to.
	f := newOIDCFlow(t, types.OIDCBindingNone)
	off := noIDTokenRow(f.row)
	f.site.set(func(b *types.ModelProviders) { b.Providers[0] = off })
	f.row = off
	at := f.begin(t, oidcPersonSub)
	u, err := url.Parse(at.authURL)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(u.Query().Get("scope")); slices.Contains(got, entraOpenIDScope) || !slices.Contains(got, entraOfflineAccessScope) || !slices.Contains(got, "api.read") {
		t.Errorf("the authorization request scope = %v", got)
	}
}

func TestEndpointOIDCNoIDToken_AConfigEditThatRacesACallbackIsRefused(t *testing.T) {
	for name, flip := range map[string]struct {
		start, to func(types.ModelProvider) types.ModelProvider
	}{
		"on to off": {func(r types.ModelProvider) types.ModelProvider {
			return withOIDC(r, func(o *types.ProviderOIDC) { o.IdentityBinding = types.OIDCBindingNone })
		}, noIDTokenRow},
		"off to on": {noIDTokenRow, func(r types.ModelProvider) types.ModelProvider {
			return withOIDC(r, func(o *types.ProviderOIDC) { o.IdentityBinding, o.RequireIDToken = types.OIDCBindingNone, nil })
		}},
	} {
		f := newOIDCFlow(t, types.OIDCBindingNone)
		start := flip.start(f.row)
		f.row = start
		f.site.set(func(b *types.ModelProviders) { b.Providers[0] = start })
		at := f.begin(t, oidcPersonSub)
		next := flip.to(start)
		f.site.set(func(b *types.ModelProviders) { b.Providers[0] = next })
		w := f.callback(oidcPersonSub, at.q, at.cookies)
		if w.Code != http.StatusFound || w.Header().Get("Location") != f.errorRedirect(azureCaptureRowChanged) {
			t.Errorf("%s: %d Location %q, want the row-changed refusal", name, w.Code, w.Header().Get("Location"))
		}
		if _, found := f.stored(t, oidcPersonSub); found {
			t.Errorf("%s: stored a sign-in started under another mode", name)
		}
	}
}

// The same race at the handler, with the authority's token endpoint held: the callback has passed its first
// read of the row (endpointOIDCStampHolds before the exchange) and is waiting on the token POST when the admin
// flips the mode. Only the check under the site-config lock, after the exchange, can refuse it.
func TestEndpointOIDCNoIDToken_AModeFlipWhileTheTokenPostIsHeldIsRefusedAfterTheExchange(t *testing.T) {
	for name, toOff := range map[string]bool{"on to off": true, "off to on": false} {
		f := newOIDCFlow(t, types.OIDCBindingNone)
		from, to := f.row, noIDTokenRow(f.row)
		if !toOff {
			from, to = to, from
		}
		f.site.set(func(b *types.ModelProviders) { b.Providers[0] = from })
		f.row = from
		at := f.begin(t, oidcPersonSub)

		gate, reached, release := oidcPauseFirst("/token")
		f.gate.Store(&gate)
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- f.callback(oidcPersonSub, at.q, at.cookies) }()
		oidcWait(t, reached, "the callback to reach the held token POST")

		f.site.set(func(b *types.ModelProviders) { b.Providers[0] = to })
		close(release)
		var w *httptest.ResponseRecorder
		select {
		case w = <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: the callback did not finish", name)
		}
		if w.Code != http.StatusFound || w.Header().Get("Location") != f.errorRedirect(azureCaptureRowChanged) {
			t.Errorf("%s: %d Location %q, want the row-changed refusal", name, w.Code, w.Header().Get("Location"))
		}
		if _, found := f.stored(t, oidcPersonSub); found || f.rawSignIn(t, oidcPersonSub) != nil {
			t.Errorf("%s: stored a sign-in captured under the other mode", name)
		}
		if rows := f.audit.find(endpointOIDCCapturedAction); len(rows) != 1 || rows[0].Outcome != "failure" {
			t.Errorf("%s: audit rows = %+v; want one failure row", name, rows)
		}
	}
}

// The check under the site-config lock, after the exchange: a row whose mode moved between the callback's own
// read and the write is refused, by the address digest and by the mode the signed state names.
func TestEndpointOIDCNoIDToken_TheLiveRowCheckAfterTheExchangeRefusesAChangedMode(t *testing.T) {
	f := newOIDCFlow(t, types.OIDCBindingNone)
	off := noIDTokenRow(f.row)
	on := f.row
	stampFor := func(row types.ModelProvider) endpointOIDCStamp {
		return endpointOIDCStamp{RowUID: row.UID, Digest: providerAddressDigest(row), NoIDToken: !row.OIDC.IDTokenRequired()}
	}
	if !endpointOIDCStampHolds(off, stampFor(off)) || !endpointOIDCStampHolds(on, stampFor(on)) {
		t.Fatal("a stamp does not hold for the row it was made for")
	}
	// The digest alone would pass if only the mode named by the stamp differed from the row's.
	if stamp := stampFor(off); endpointOIDCStampHolds(off, endpointOIDCStamp{RowUID: off.UID, Digest: stamp.Digest, NoIDToken: false}) {
		t.Error("a stamp that names the default mode holds for a row with require_id_token false")
	}
	for name, pair := range map[string][2]types.ModelProvider{"on to off": {on, off}, "off to on": {off, on}} {
		f.site.set(func(b *types.ModelProviders) { b.Providers[0] = pair[1] })
		ec, err := endpointOIDCCapture(pair[0], nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		reason, err := f.srv.storeEndpointOIDCCapture(context.Background(), oidcPersonSub, stampFor(pair[0]), ec, adoEntraBlob{RefreshToken: "rt"})
		if err != nil || reason != azureCaptureRowChanged {
			t.Errorf("%s: store = (%q, %v), want %q", name, reason, err, azureCaptureRowChanged)
		}
	}
	// A stamp from 0.10.0 has no mode field, and a default row's own stamp does not write one.
	raw, _ := json.Marshal(stampFor(on))
	if strings.Contains(string(raw), `"n"`) {
		t.Errorf("a default row's state carries a mode field: %s", raw)
	}
}

func TestEndpointOIDCNoIDToken_ACallbackWithNoIDTokenStoresTheTokensUnderThePerson(t *testing.T) {
	f := newOIDCFlow(t, types.OIDCBindingNone)
	off := noIDTokenRow(f.row)
	f.site.set(func(b *types.ModelProviders) { b.Providers[0] = off })
	f.row = off
	drop := func(m map[string]any) { delete(m, "id_token") }
	f.tokenEdit.Store(&drop)

	_, w := f.capture(t, oidcPersonSub)
	if w.Code != http.StatusFound || w.Header().Get("Location") != f.doneRedirect() {
		t.Fatalf("capture with no id_token: %d Location %q", w.Code, w.Header().Get("Location"))
	}
	blob, found := f.stored(t, oidcPersonSub)
	if !found || blob.RefreshToken == "" || blob.Subject != oidcPersonSub || blob.Issuer != "" || blob.IDPSubject != "" || !blob.IDTokenNotRequired {
		t.Fatalf("stored = %+v found=%v; want the person's sign-in with no issuer and no idp_sub", blob, found)
	}
	name := providerSecretName(oidcUID, providerOIDCPart)
	if own, _ := f.srv.cfg.Secrets.For(oidcPersonSub).List(context.Background()); !slices.Contains(own, name) {
		t.Errorf("the person's namespace %v does not hold %q", own, name)
	}
	if ops, _ := f.srv.cfg.Secrets.List(context.Background()); slices.Contains(ops, name) {
		t.Errorf("the operator namespace holds %q", name)
	}
	if !blob.valid() {
		t.Error("the shape check refuses a sign-in that holds no issuer")
	}
	if want := []string{entraOfflineAccessScope, "api.read"}; !slices.Equal(oidcSorted(blob.Scopes), oidcSorted(want)) {
		t.Errorf("stored scopes %v, want %v (the set asked for, no openid)", blob.Scopes, want)
	}

	// The default mode, same authority behaviour: no id_token is a refusal and nothing is stored.
	g := newOIDCFlow(t, types.OIDCBindingNone)
	g.tokenEdit.Store(&drop)
	if _, w := g.capture(t, oidcPersonSub); w.Header().Get("Location") != g.errorRedirect(reasonADOCallbackIdentityBinding) {
		t.Errorf("default mode, no id_token: Location %q, want the identity-binding refusal", w.Header().Get("Location"))
	}
	if _, found := g.stored(t, oidcPersonSub); found {
		t.Error("default mode stored a sign-in with no identity token")
	}
}

func TestEndpointOIDCNoIDToken_AnUnsolicitedIDTokenIsIgnoredAndNeverStoredOrLogged(t *testing.T) {
	const bogus = "unsolicited.identity-token-value.never-verifiable-0001"
	swap := func(m map[string]any) { m["id_token"] = bogus }

	f := newOIDCFlow(t, types.OIDCBindingNone)
	off := noIDTokenRow(f.row)
	f.site.set(func(b *types.ModelProviders) { b.Providers[0] = off })
	f.row = off
	f.tokenEdit.Store(&swap)
	_, w := f.capture(t, oidcPersonSub)
	if w.Header().Get("Location") != f.doneRedirect() {
		t.Fatalf("an unsolicited id_token refused the sign-in: %d Location %q", w.Code, w.Header().Get("Location"))
	}
	blob, found := f.stored(t, oidcPersonSub)
	if !found || blob.Issuer != "" || blob.IDPSubject != "" {
		t.Fatalf("stored = %+v found=%v; want no issuer and no idp_sub from an ignored token", blob, found)
	}
	for where, text := range map[string]string{"log": f.logs.String(), "audit": f.auditText(), "stored": string(f.rawSignIn(t, oidcPersonSub)), "response": w.Body.String() + w.Header().Get("Location")} {
		if strings.Contains(text, bogus) {
			t.Errorf("the %s carries the unsolicited identity token", where)
		}
	}
	if !slices.Contains(f.wire.all(), bogus) {
		t.Error("setup: the authority never sent the unsolicited token")
	}

	// The default mode reads the same token, cannot verify it and refuses: the difference is the mode.
	g := newOIDCFlow(t, types.OIDCBindingNone)
	g.tokenEdit.Store(&swap)
	if _, w := g.capture(t, oidcPersonSub); w.Header().Get("Location") != g.errorRedirect(reasonADOCallbackIdentityBinding) {
		t.Errorf("default mode, unverifiable id_token: Location %q, want the identity-binding refusal", w.Header().Get("Location"))
	}
}

func TestEndpointOIDCNoIDToken_TheCaptureAuditRowNamesTheField(t *testing.T) {
	idTokenField := func(f *oidcFlow) (any, bool) {
		rows := f.audit.find(endpointOIDCCapturedAction)
		if len(rows) != 1 || rows[0].Outcome != "success" {
			t.Fatalf("audit rows = %+v; want one success row", rows)
		}
		var data map[string]any
		if err := json.Unmarshal(rows[0].Data, &data); err != nil {
			t.Fatal(err)
		}
		v, ok := data["id_token"]
		return v, ok
	}
	for name, edit := range map[string]func(map[string]any){
		"a token came back":     func(m map[string]any) {},
		"no token came back":    func(m map[string]any) { delete(m, "id_token") },
		"an unverifiable token": func(m map[string]any) { m["id_token"] = "not.a.token" },
	} {
		f := newOIDCFlow(t, types.OIDCBindingNone)
		off := noIDTokenRow(f.row)
		f.site.set(func(b *types.ModelProviders) { b.Providers[0] = off })
		f.row = off
		f.tokenEdit.Store(&edit)
		f.mustSignIn(t, oidcPersonSub)
		if v, ok := idTokenField(f); !ok || v != "not_required" {
			t.Errorf("%s: id_token = %v (present %v), want not_required", name, v, ok)
		}
	}
	f := newOIDCFlow(t, types.OIDCBindingNone)
	f.mustSignIn(t, oidcPersonSub)
	if v, ok := idTokenField(f); ok {
		t.Errorf("a default row's capture row carries id_token = %v; it reads as it always has", v)
	}
}

func TestEndpointOIDCNoIDToken_RefreshWorksAndRotationKeepsTheSignInUsable(t *testing.T) {
	f := newOIDCFlow(t, types.OIDCBindingNone)
	off := noIDTokenRow(f.row)
	f.site.set(func(b *types.ModelProviders) { b.Providers[0] = off })
	f.row = off
	drop := func(m map[string]any) { delete(m, "id_token") }
	f.tokenEdit.Store(&drop)
	f.mustSignIn(t, oidcPersonSub)
	before, _ := f.stored(t, oidcPersonSub)

	for i := 0; i < 2; i++ {
		access, err := f.srv.RedeemEndpointOIDCAccess(context.Background(), oidcPersonSub, f.row)
		if err != nil || access.AccessToken == "" {
			t.Fatalf("redeem %d = %+v, %v", i+1, access, err)
		}
	}
	if got := f.fake.RefreshRequestScopes(); !slices.Equal(got, []string{"", ""}) {
		t.Errorf("refresh requests sent scopes %q; an endpoint sign-in sends none", got)
	}
	after, found := f.stored(t, oidcPersonSub)
	if !found || after.RefreshToken == before.RefreshToken || after.RenewedAt.IsZero() || !after.IDTokenNotRequired || after.CaptureDigest != before.CaptureDigest {
		t.Errorf("the rotation did not persist a usable sign-in: before %+v after %+v found=%v", before, after, found)
	}
}

func TestEndpointOIDCNoIDToken_AStaleCaptureIsRefusedAtRedemptionWithoutATokenPost(t *testing.T) {
	for name, capturedOff := range map[string]bool{"captured off, now default": true, "captured default, now off": false} {
		f := newOIDCFlow(t, types.OIDCBindingNone)
		captured, other := f.row, noIDTokenRow(f.row)
		if capturedOff {
			captured, other = other, captured
		}
		f.site.set(func(b *types.ModelProviders) { b.Providers[0] = captured })
		f.row = captured
		f.mustSignIn(t, oidcPersonSub)
		before := f.rawSignIn(t, oidcPersonSub)
		f.site.set(func(b *types.ModelProviders) { b.Providers[0] = other })

		if _, err := f.srv.RedeemEndpointOIDCAccess(context.Background(), oidcPersonSub, other); !errors.Is(err, ErrADOEntraNotCaptured) {
			t.Errorf("%s: redeem = %v, want ErrADOEntraNotCaptured", name, err)
		}
		if got := f.fake.RefreshRequestScopes(); len(got) != 0 {
			t.Errorf("%s: the authority received %d token requests, want none", name, len(got))
		}
		if after := f.rawSignIn(t, oidcPersonSub); string(after) != string(before) {
			t.Errorf("%s: the refused redemption changed the stored sign-in", name)
		}
	}
}
