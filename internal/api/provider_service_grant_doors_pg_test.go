// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The doors of the service grant against the real store: what a provider write keeps, drops and claims, what
// the secret route allows and to whom, and the switch-on's refusal. Skipped without WARDYN_TEST_PG.

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func svcReason(t *testing.T, w interface{ Result() *http.Response }, body string) string {
	t.Helper()
	i := strings.Index(body, `"reason":"`)
	if i < 0 {
		return ""
	}
	rest := body[i+len(`"reason":"`):]
	return rest[:strings.Index(rest, `"`)]
}

func TestServiceGrant_RefusedBeforeActivation_PG(t *testing.T) {
	f := newSvcPG(t, false)
	w := f.put(svcPGRow("corp-a", true))
	if w.Code != http.StatusConflict || svcReason(t, w, w.Body.String()) != reasonServiceGrantsNotActivated {
		t.Fatalf("PUT /model-providers with a grant before activation = %d %s, want 409 service_grants_not_activated", w.Code, w.Body.String())
	}
	cfg := f.site()
	cfg.ModelProviders = &types.ModelProviders{Providers: []types.ModelProvider{svcPGRow("corp-a", true)}}
	w = f.do(http.MethodPut, "/site-config", f.admin, cfg)
	if w.Code != http.StatusConflict || svcReason(t, w, w.Body.String()) != reasonServiceGrantsNotActivated {
		t.Fatalf("PUT /site-config with a grant before activation = %d %s, want 409", w.Code, w.Body.String())
	}
	if len(f.rows()) != 0 || len(f.storedGrantUIDs()) != 0 || f.stored(store.ServiceGrantsKey) != "" {
		t.Fatalf("a refused write left rows %v and stored grants %q", f.rows(), f.stored(store.ServiceGrantsKey))
	}
	// No stored row: the refusal holds on a deployment whose site_config has no document yet.
	if _, err := f.pool.Exec(context.Background(), `DELETE FROM site_config`); err != nil {
		t.Fatal(err)
	}
	if w = f.put(svcPGRow("corp-a", true)); w.Code != http.StatusConflict {
		t.Fatalf("with no stored row, PUT = %d %s, want 409", w.Code, w.Body.String())
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM site_config`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a refused grant created %d site_config rows (%v)", n, err)
	}
	// The mint refuses with no request to the identity provider.
	f2 := newSvcPG(t, false)
	if w = f2.put(svcPGRow("corp-a", false)); w.Code != http.StatusNoContent && w.Code != http.StatusOK {
		t.Fatalf("a row with no grant = %d %s", w.Code, w.Body.String())
	}
	row := f2.row("corp-a")
	row.OIDC.ServiceGrant = svcPGGrant()
	_, err := f2.srv.serviceGrantToken(context.Background(), row, "", serviceGrantTriggerRefresh)
	if se := svcErr(t, err); se.reason != svcReasonNotActivated || f2.idp.count() != 0 {
		t.Fatalf("mint = %+v with %d requests, want not_activated and none", se, f2.idp.count())
	}
	// After activation the same write succeeds.
	f2.activate()
	if w = f2.put(svcPGRow("corp-a", true)); w.Code != http.StatusNoContent && w.Code != http.StatusOK {
		t.Fatalf("after activation: %d %s", w.Code, w.Body.String())
	}
	if f2.row("corp-a").OIDC.ServiceGrant == nil {
		t.Fatal("the grant was not stored after activation")
	}
}

func TestServiceGrant_RefusedMixedEditDestroysNothing_PG(t *testing.T) {
	f := newSvcPG(t, false)
	if w := f.put(svcPGRow("corp-a", false), svcPGRow("corp-b", false)); w.Code >= 300 {
		t.Fatalf("setup: %d %s", w.Code, w.Body.String())
	}
	// B's person holds a personal credential; changing B's address would purge it.
	bUID := f.uid("corp-b")
	secretName := providerCredentialName(f.row("corp-b"))
	f.sec.set(secretName, []byte("a-personal-credential-value"))
	before := f.stored("provider_service_grants")
	a, b := f.row("corp-a"), f.row("corp-b")
	a.OIDC.ServiceGrant = svcPGGrant()
	b.BaseURL = "https://moved.corp.example"
	w := f.put(a, b)
	if w.Code != http.StatusConflict || svcReason(t, w, w.Body.String()) != reasonServiceGrantsNotActivated {
		t.Fatalf("mixed edit = %d %s, want 409 service_grants_not_activated", w.Code, w.Body.String())
	}
	if !f.sec.has(secretName) {
		t.Fatalf("the declined edit purged the personal credential of %s", bUID)
	}
	if got := f.row("corp-b").BaseURL; got != "https://corp-b.corp.example" || f.row("corp-a").OIDC.ServiceGrant != nil || f.stored("provider_service_grants") != before {
		t.Fatalf("the declined edit changed the configuration: b=%s", got)
	}
	if n := f.sec.callsOf("delete"); n != 0 {
		t.Errorf("the declined edit deleted %d secrets", n)
	}
}

func TestServiceGrant_OmittedGrantKeptOnRetainedRow_PG(t *testing.T) {
	f := newSvcPG(t, true)
	if w := f.put(svcPGRow("corp-a", true), svcPGRow("corp-b", true)); w.Code >= 300 {
		t.Fatalf("setup: %d %s", w.Code, w.Body.String())
	}
	uidA := f.uid("corp-a")
	f.writeSecret("corp-a", svcClientSecret)
	// An absent member and null keep the grant, through PUT /model-providers.
	a := f.row("corp-a")
	a.OIDC.ServiceGrant = nil
	if w := f.put(a, f.row("corp-b")); w.Code >= 300 || f.row("corp-a").OIDC.ServiceGrant == nil {
		t.Fatalf("an omitted grant: %d %s, grant kept = %v", w.Code, w.Body.String(), f.row("corp-a").OIDC.ServiceGrant != nil)
	}
	// ... and through PUT /site-config naming model_providers.
	cfg := f.site()
	cfg.ModelProviders.Providers[0].OIDC.ServiceGrant = nil
	if w := f.do(http.MethodPut, "/site-config", f.admin, cfg); w.Code >= 300 || f.row("corp-a").OIDC.ServiceGrant == nil {
		t.Fatalf("site-config with the grant omitted: %d %s", w.Code, w.Body.String())
	}
	// An object replaces it.
	a = f.row("corp-a")
	a.OIDC.ServiceGrant = &types.ProviderServiceGrant{GrantType: types.ServiceGrantTypeClientCreds, TokenURL: svcTokenURL,
		ClientID: svcClientID, Scope: "api.write", ServiceIdentities: []string{svcOwner}}
	if w := f.put(a, f.row("corp-b")); w.Code >= 300 || f.row("corp-a").OIDC.ServiceGrant.Scope != "api.write" {
		t.Fatalf("an object did not replace the grant: %d %s", w.Code, w.Body.String())
	}
	name := serviceGrantSecretName(uidA)
	// A removed row, a new id, and a removed oidc each drop the grant and delete its record.
	for _, tc := range []struct {
		what string
		rows func() []types.ModelProvider
	}{
		{"a removed row", func() []types.ModelProvider { return []types.ModelProvider{f.row("corp-b")} }},
		{"a new id", func() []types.ModelProvider {
			a := f.row("corp-a")
			a.ID, a.UID = "corp-a2", ""
			return []types.ModelProvider{a, f.row("corp-b")}
		}},
		{"a removed oidc", func() []types.ModelProvider {
			a := f.row("corp-a")
			a.OIDC = nil
			return []types.ModelProvider{a, f.row("corp-b")}
		}},
	} {
		if !f.sec.has(name) {
			f.writeSecretFor(t, uidA)
		}
		if w := f.put(tc.rows()...); w.Code >= 300 {
			t.Fatalf("%s: %d %s", tc.what, w.Code, w.Body.String())
		}
		if slices.Contains(f.storedGrantUIDs(), uidA) || f.sec.has(name) {
			t.Errorf("%s: the grant or its record survived (stored %v, record %v)", tc.what, f.storedGrantUIDs(), f.sec.has(name))
		}
		// Put corp-a back for the next case.
		if w := f.put(svcPGRow("corp-a", true), f.row("corp-b")); w.Code >= 300 {
			t.Fatalf("restore after %s: %d %s", tc.what, w.Code, w.Body.String())
		}
		uidA = f.uid("corp-a")
		name = serviceGrantSecretName(uidA)
	}
	if f.row("corp-b").OIDC.ServiceGrant == nil {
		t.Error("an adjacent provider's grant changed")
	}
}

// writeSecretFor writes a record for uid directly, as a prior route call would have.
func (f *svcPG) writeSecretFor(t *testing.T, uid string) {
	t.Helper()
	for _, p := range f.rows() {
		if p.UID == uid {
			if w := f.writeSecret(p.ID, svcClientSecret); w.Code != http.StatusNoContent {
				t.Fatalf("write secret: %d %s", w.Code, w.Body.String())
			}
			return
		}
	}
	t.Fatalf("no row with uid %s", uid)
}

func TestServiceGrant_RemoveRouteDropsGrantAndSecret_PG(t *testing.T) {
	f := newSvcPG(t, true)
	if w := f.put(svcPGRow("corp-a", true), svcPGRow("corp-b", true)); w.Code >= 300 {
		t.Fatalf("setup: %d %s", w.Code, w.Body.String())
	}
	uidA, uidB := f.uid("corp-a"), f.uid("corp-b")
	f.writeSecret("corp-a", svcClientSecret)
	f.writeSecret("corp-b", svcClientSecret)
	w := f.do(http.MethodDelete, "/model-providers/corp-a/service-grant", f.admin, nil)
	if w.Code != http.StatusNoContent && w.Code != http.StatusOK {
		t.Fatalf("DELETE service-grant = %d %s", w.Code, w.Body.String())
	}
	// The grant is gone on a re-read, the record is deleted, adjacent grants are unchanged, and a mint refuses.
	if f.row("corp-a").OIDC.ServiceGrant != nil || slices.Contains(f.storedGrantUIDs(), uidA) {
		t.Fatal("the explicit removal left the grant in place")
	}
	if f.sec.has(serviceGrantSecretName(uidA)) || !f.sec.has(serviceGrantSecretName(uidB)) || f.row("corp-b").OIDC.ServiceGrant == nil {
		t.Fatal("the removal touched the wrong record or grant")
	}
	removed := f.row("corp-a")
	removed.OIDC.ServiceGrant = svcPGGrant()
	if _, err := f.srv.serviceGrantToken(context.Background(), removed, "", serviceGrantTriggerDispatch); err == nil {
		t.Fatal("a mint for the removed grant succeeded")
	}
	if rows := f.audit.find("model_provider.write"); len(rows) == 0 || !strings.Contains(string(rows[len(rows)-1].Data), `"service_grant_removed"`) {
		t.Errorf("no model_provider.write row records the removal")
	}
	// The claim for that uid was created in the same write and settled by the in-line delete.
	if slices.Contains(f.claimUIDs(), uidA) {
		t.Errorf("a settled claim remains: %v", f.claimUIDs())
	}
	// 409 when there is none.
	if w = f.do(http.MethodDelete, "/model-providers/corp-a/service-grant", f.admin, nil); w.Code != http.StatusConflict ||
		svcReason(t, w, w.Body.String()) != reasonServiceGrantNotConfigured {
		t.Errorf("a second removal = %d %s, want 409 service_grant_not_configured", w.Code, w.Body.String())
	}
	// Operator only.
	if w = f.do(http.MethodDelete, "/model-providers/corp-b/service-grant", f.person, nil); w.Code != http.StatusForbidden {
		t.Errorf("a person's removal = %d, want 403", w.Code)
	}
}

func TestServiceGrant_ExplicitRemovalKeepsClaimWhenDeleteFails_PG(t *testing.T) {
	f := newSvcPG(t, true)
	f.put(svcPGRow("corp-a", true))
	uid := f.uid("corp-a")
	f.writeSecret("corp-a", svcClientSecret)
	f.sec.delErr = errSvcDown
	w := f.do(http.MethodDelete, "/model-providers/corp-a/service-grant", f.admin, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), reasonServiceGrantSecretKept) || !strings.Contains(w.Body.String(), uid) {
		t.Fatalf("removal with a failing delete = %d %s, want the secret_kept warning naming the uid", w.Code, w.Body.String())
	}
	if !slices.Contains(f.claimUIDs(), uid) || f.row("corp-a").OIDC.ServiceGrant != nil {
		t.Fatalf("claims %v, grant %v: want one claim and no grant", f.claimUIDs(), f.row("corp-a").OIDC.ServiceGrant)
	}
}

func TestServiceGrant_InertEntryRetiredWhenOIDCRestored_PG(t *testing.T) {
	f := newSvcPG(t, true)
	f.put(svcPGRow("corp-a", true))
	uid := f.uid("corp-a")
	f.writeSecret("corp-a", svcClientSecret)
	// An older server removes the row's oidc block and leaves the stored entry inert.
	cfg := f.site()
	cfg.ModelProviders.Providers[0].OIDC = nil
	f.oldWriterSave(cfg)
	if f.row("corp-a").OIDC != nil || !slices.Contains(f.storedGrantUIDs(), uid) {
		t.Fatalf("setup: row oidc %v, stored %v", f.row("corp-a").OIDC, f.storedGrantUIDs())
	}
	// A new-server write restores the oidc block with the same grant and destinations.
	restored := svcPGRow("corp-a", true)
	restored.UID = uid
	if w := f.put(restored); w.Code >= 300 {
		t.Fatalf("restore: %d %s", w.Code, w.Body.String())
	}
	if f.sec.has(serviceGrantSecretName(uid)) {
		t.Fatal("the earlier generation's secret record survived the restore")
	}
	if f.row("corp-a").OIDC.ServiceGrant == nil {
		t.Fatal("the new grant was not stored")
	}
	if _, err := f.mint("corp-a"); err == nil || f.idp.count() != 0 {
		t.Fatalf("mint = %v with %d requests, want a refusal until a fresh secret is written", err, f.idp.count())
	}
	if w := f.writeSecret("corp-a", svcClientSecret); w.Code != http.StatusNoContent {
		t.Fatalf("fresh secret: %d %s", w.Code, w.Body.String())
	}
	if _, err := f.mint("corp-a"); err != nil {
		t.Fatalf("mint after a fresh secret: %v", err)
	}
	if rows := f.audit.find("model_provider.service_grant.secret_delete"); len(rows) == 0 {
		t.Errorf("no secret_delete row records the retired generation's record")
	}
}

func TestServiceGrant_SecretRouteOperatorOnlyNeverReturnsValue_PG(t *testing.T) {
	f := newSvcPG(t, true)
	f.put(svcPGRow("corp-a", true), svcPGRow("corp-nogrant", false))
	secAdmin := ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin)
	svc := ssoSession(t, svcOwner, "svc@corp.example", oidc.RoleUser)
	p := f.row("corp-a")
	body := putServiceGrantSecretRequest{Value: svcClientSecret, TokenURL: p.OIDC.ServiceGrant.TokenURL, ClientID: p.OIDC.ServiceGrant.ClientID, BaseURL: p.BaseURL}
	for name, c := range map[string]*http.Cookie{"security admin": secAdmin, "service": svc, "person": f.person} {
		for _, m := range []string{http.MethodPut, http.MethodDelete} {
			if w := f.do(m, f.secretPath("corp-a"), c, body); w.Code != http.StatusForbidden {
				t.Errorf("%s %s the secret route = %d, want 403", name, m, w.Code)
			}
		}
	}
	if f.sec.has(serviceGrantSecretName(p.UID)) {
		t.Fatal("a refused caller stored a secret")
	}
	if w := f.do(http.MethodPut, f.secretPath("corp-a"), f.admin, body); w.Code != http.StatusNoContent || strings.Contains(w.Body.String(), svcClientSecret) {
		t.Fatalf("operator write = %d %q", w.Code, w.Body.String())
	}
	// The route is write-only: a read is refused as a method the route does not have, and says nothing of the value.
	// The router adds one Allow value per method in map order, so every value is read, not only the first.
	w := f.do(http.MethodGet, f.secretPath("corp-a"), f.admin, nil)
	allow := map[string]bool{}
	for _, v := range w.Header().Values("Allow") {
		for _, m := range strings.Split(v, ",") {
			allow[strings.TrimSpace(m)] = true
		}
	}
	if w.Code != http.StatusMethodNotAllowed || strings.Contains(w.Body.String(), svcClientSecret) ||
		!allow[http.MethodPut] || !allow[http.MethodDelete] || allow[http.MethodGet] {
		t.Errorf("GET on the write-only secret route = %d (Allow %v), want 405 allowing PUT and DELETE, not GET", w.Code, w.Header().Values("Allow"))
	}
	// A write with no grant is 409.
	nb := body
	if w := f.do(http.MethodPut, f.secretPath("corp-nogrant"), f.admin, nb); w.Code != http.StatusConflict ||
		svcReason(t, w, w.Body.String()) != reasonServiceGrantNotConfigured {
		t.Errorf("a write with no grant = %d %s, want 409 service_grant_not_configured", w.Code, w.Body.String())
	}
	if w := f.do(http.MethodPut, f.secretPath("no-such"), f.admin, nb); w.Code != http.StatusNotFound {
		t.Errorf("an unknown provider = %d, want 404", w.Code)
	}
}

func TestServiceGrant_BindingRefusesNewDestination_PG(t *testing.T) {
	for _, door := range []string{"model-providers", "site-config"} {
		f := newSvcPG(t, true)
		f.put(svcPGRow("corp-a", true))
		f.writeSecret("corp-a", svcClientSecret)
		if _, err := f.mint("corp-a"); err != nil {
			t.Fatalf("%s: first mint: %v", door, err)
		}
		for name, edit := range map[string]func(*types.ModelProvider){
			"token_url": func(p *types.ModelProvider) { p.OIDC.ServiceGrant.TokenURL = "https://idp2.example.test/token" },
			"client_id": func(p *types.ModelProvider) { p.OIDC.ServiceGrant.ClientID = "svc-client-2" },
			"base_url":  func(p *types.ModelProvider) { p.BaseURL = "https://corp-a-new.corp.example" },
		} {
			p := f.row("corp-a")
			edit(&p)
			var code int
			if door == "model-providers" {
				code = f.put(p).Code
			} else {
				cfg := f.site()
				cfg.ModelProviders.Providers[0] = p
				code = f.do(http.MethodPut, "/site-config", f.admin, cfg).Code
			}
			if code >= 300 {
				t.Fatalf("%s %s: change refused with %d, want accepted", door, name, code)
			}
			before := f.idp.count()
			f.srv.dropServiceGrantCache(p.UID)
			_, err := f.mint("corp-a")
			if se := svcErr(t, err); se.reason != svcReasonUnbound || f.idp.count() != before {
				t.Fatalf("%s %s: mint = %+v with %d new requests, want unbound and none", door, name, se, f.idp.count()-before)
			}
			// Put the destination back and write the secret again for the next case.
			orig := svcPGRow("corp-a", true)
			orig.UID = p.UID
			f.put(orig)
			f.writeSecret("corp-a", svcClientSecret)
		}
		// A value not in the record's shape refuses the same way.
		f.sec.set(serviceGrantSecretName(f.uid("corp-a")), []byte("a-bare-secret-not-a-record"))
		f.srv.dropServiceGrantCache(f.uid("corp-a"))
		if _, err := f.mint("corp-a"); svcErr(t, err).reason != svcReasonUnbound {
			t.Errorf("%s: a bare value did not refuse unbound: %v", door, err)
		}
	}
	// A row removed and added back has a new name and no secret.
	f := newSvcPG(t, true)
	f.put(svcPGRow("corp-a", true))
	old := f.uid("corp-a")
	f.writeSecret("corp-a", svcClientSecret)
	f.put()
	f.put(svcPGRow("corp-a", true))
	if f.uid("corp-a") == old || f.sec.has(serviceGrantSecretName(f.uid("corp-a"))) {
		t.Errorf("a re-added row kept its uid %s or a secret", f.uid("corp-a"))
	}
}

func TestServiceGrant_CleanupClaimsAreAtomicWithTheWrite_PG(t *testing.T) {
	f := newSvcPG(t, true)
	f.put(svcPGRow("corp-a", true))
	uid := f.uid("corp-a")
	f.writeSecret("corp-a", svcClientSecret)
	f.srv.svcGrant.hook = func(stage string) bool { return stage == "after_commit" }
	if w := f.do(http.MethodDelete, "/model-providers/corp-a/service-grant", f.admin, nil); w.Code >= 500 {
		t.Fatalf("removal: %d %s", w.Code, w.Body.String())
	}
	// The request ended after the commit and before the delete: the claim and the record are both there.
	if !slices.Contains(f.claimUIDs(), uid) || !f.sec.has(serviceGrantSecretName(uid)) {
		t.Fatalf("claims %v, record %v, want the claim stored and the record untouched", f.claimUIDs(), f.sec.has(serviceGrantSecretName(uid)))
	}
}

func TestServiceGrant_CleanupClaimSurvivesCrashAfterCommit_PG(t *testing.T) {
	f := newSvcPG(t, true)
	f.put(svcPGRow("corp-a", true))
	uid := f.uid("corp-a")
	f.writeSecret("corp-a", svcClientSecret)
	f.srv.svcGrant.hook = func(stage string) bool { return stage == "after_commit" }
	f.do(http.MethodDelete, "/model-providers/corp-a/service-grant", f.admin, nil)
	if !slices.Contains(f.claimUIDs(), uid) {
		t.Fatal("the claim was not stored")
	}
	// The next leader's worker deletes the record and settles the claim.
	next := f.newReplica()
	next.srv.serviceGrantCleanupPass(context.Background())
	if f.sec.has(serviceGrantSecretName(uid)) || slices.Contains(f.claimUIDs(), uid) {
		t.Fatalf("after the worker's pass: record %v, claims %v", f.sec.has(serviceGrantSecretName(uid)), f.claimUIDs())
	}
	if rows := next.audit.find("model_provider.service_grant.secret_delete"); len(rows) != 1 || !strings.Contains(string(rows[0].Data), `"cleanup":true`) {
		t.Errorf("settle rows = %d, want one marked cleanup", len(rows))
	}
}

func TestServiceGrant_CleanupFailureWarns_PG(t *testing.T) {
	f := newSvcPG(t, true)
	f.put(svcPGRow("corp-a", true))
	uid := f.uid("corp-a")
	f.writeSecret("corp-a", svcClientSecret)
	f.sec.delErr = errSvcDown
	w := f.put()
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), reasonServiceGrantSecretKept) || !strings.Contains(w.Body.String(), uid) {
		t.Fatalf("PUT with a failing external delete = %d %s, want the warning naming the uid", w.Code, w.Body.String())
	}
	rows := f.audit.find("model_provider.write")
	if len(rows) == 0 || !strings.Contains(string(rows[len(rows)-1].Data), `"secret_cleanup":"failed"`) {
		t.Errorf("the write's audit row does not say the clean-up failed")
	}
	got := f.do(http.MethodGet, "/model-providers", f.admin, nil)
	if !strings.Contains(got.Body.String(), uid) || !strings.Contains(got.Body.String(), "service_grant_cleanups") {
		t.Errorf("GET /model-providers does not list the claim: %s", got.Body.String())
	}
	f.sec.mu.Lock()
	f.sec.delErr = nil
	f.sec.mu.Unlock()
	f.srv.serviceGrantCleanupPass(context.Background())
	if f.sec.has(serviceGrantSecretName(uid)) || slices.Contains(f.claimUIDs(), uid) {
		t.Fatalf("after recovery: record %v, claims %v", f.sec.has(serviceGrantSecretName(uid)), f.claimUIDs())
	}
	if rows := f.audit.find("model_provider.service_grant.secret_delete"); len(rows) != 1 {
		t.Errorf("settle rows = %d, want one", len(rows))
	}
}

func TestServiceGrant_CleanupRetryTargetsRetiredUidOnly_PG(t *testing.T) {
	f := newSvcPG(t, true)
	f.put(svcPGRow("corp-p", true))
	first := f.uid("corp-p")
	f.writeSecret("corp-p", svcClientSecret)
	f.sec.delErr = errSvcDown
	f.put() // P removed; the external delete fails, leaving a claim on its uid
	f.sec.mu.Lock()
	f.sec.delErr = nil
	f.sec.mu.Unlock()
	f.put(svcPGRow("corp-p", true)) // created again under the same id: a new uid
	second := f.uid("corp-p")
	if second == first {
		t.Fatal("the re-created row kept its uid")
	}
	f.writeSecret("corp-p", svcClientSecret)
	if _, err := f.mint("corp-p"); err != nil {
		t.Fatalf("mint: %v", err)
	}
	before := f.idp.count()
	if w := f.do(http.MethodPost, "/service-grant-cleanups/"+first+"/retry", f.admin, nil); w.Code != http.StatusNoContent {
		t.Fatalf("retry of the first uid = %d %s", w.Code, w.Body.String())
	}
	if f.sec.has(serviceGrantSecretName(first)) || !f.sec.has(serviceGrantSecretName(second)) || f.idp.count() != before {
		t.Fatalf("first record %v, second record %v, new requests %d: want only the first deleted", f.sec.has(serviceGrantSecretName(first)),
			f.sec.has(serviceGrantSecretName(second)), f.idp.count()-before)
	}
	for _, uid := range []string{second, "00000000-0000-4000-8000-00000000dead"} {
		if w := f.do(http.MethodPost, "/service-grant-cleanups/"+uid+"/retry", f.admin, nil); w.Code != http.StatusNotFound {
			t.Errorf("retry of %s = %d, want 404", uid, w.Code)
		}
	}
}

func TestServiceGrant_GrantRefusedWhileCleanupPending_PG(t *testing.T) {
	f := newSvcPG(t, true)
	f.put(svcPGRow("corp-a", true))
	uid := f.uid("corp-a")
	f.writeSecret("corp-a", svcClientSecret)
	f.sec.delErr = errSvcDown
	noOIDC := f.row("corp-a")
	noOIDC.OIDC = nil
	f.put(noOIDC) // the oidc block removed and its delete failing
	if !slices.Contains(f.claimUIDs(), uid) {
		t.Fatal("no claim was written")
	}
	back := svcPGRow("corp-a", true)
	back.UID = uid
	if w := f.put(back); w.Code != http.StatusConflict || svcReason(t, w, w.Body.String()) != reasonServiceGrantCleanupPending {
		t.Fatalf("a grant added back = %d %s, want 409 service_grant_cleanup_pending", w.Code, w.Body.String())
	}
	// The secret route's PUT on the retained uid says the same, not "not configured".
	plain := svcPGRow("corp-a", false)
	plain.UID = uid
	if w := f.put(plain); w.Code >= 300 {
		t.Fatalf("a row with oidc and no grant: %d %s", w.Code, w.Body.String())
	}
	body := putServiceGrantSecretRequest{Value: svcClientSecret, TokenURL: svcTokenURL, ClientID: svcClientID, BaseURL: plain.BaseURL}
	if w := f.do(http.MethodPut, f.secretPath("corp-a"), f.admin, body); w.Code != http.StatusConflict || svcReason(t, w, w.Body.String()) != reasonServiceGrantCleanupPending {
		t.Fatalf("secret PUT on a retained uid with a pending claim = %d %s, want 409 service_grant_cleanup_pending", w.Code, w.Body.String())
	}
	f.sec.mu.Lock()
	f.sec.delErr = nil
	f.sec.mu.Unlock()
	if w := f.do(http.MethodPost, "/service-grant-cleanups/"+uid+"/retry", f.admin, nil); w.Code != http.StatusNoContent {
		t.Fatalf("retry: %d %s", w.Code, w.Body.String())
	}
	if w := f.put(back); w.Code >= 300 {
		t.Fatalf("after a successful retry the grant is refused: %d %s", w.Code, w.Body.String())
	}
	if w := f.writeSecret("corp-a", svcClientSecret); w.Code != http.StatusNoContent {
		t.Fatalf("after a successful retry the secret is refused: %d %s", w.Code, w.Body.String())
	}
}

func TestServiceGrant_EachSecretDeleteIsGuardedByTheObservedRevision_PG(t *testing.T) {
	f := newSvcPG(t, true)
	f.put(svcPGRow("corp-a", true))
	uid := f.uid("corp-a")
	name := serviceGrantSecretName(uid)
	f.writeSecret("corp-a", svcClientSecret)
	// A replacement written between the read and the delete is retained.
	f.sec.hold = func(op, n string) {
		if op == "delete" && n == name {
			f.sec.hold = nil
			f.sec.set(name, []byte(`{"v":1,"replacement":true}`))
		}
	}
	w := f.do(http.MethodDelete, f.secretPath("corp-a"), f.admin, nil)
	if w.Code != http.StatusConflict || svcReason(t, w, w.Body.String()) != reasonServiceGrantSecretChanged || !f.sec.has(name) {
		t.Fatalf("delete with a replacement in between = %d %s, record kept %v", w.Code, w.Body.String(), f.sec.has(name))
	}
	// A store that keeps no revision: 409 unsupported, zero Delete calls.
	f.sec.noRev = true
	before := f.sec.callsOf("delete")
	w = f.do(http.MethodDelete, f.secretPath("corp-a"), f.admin, nil)
	if w.Code != http.StatusConflict || svcReason(t, w, w.Body.String()) != reasonServiceGrantCleanupUnsupported || f.sec.callsOf("delete") != before {
		t.Fatalf("no-revision delete = %d %s with %d Delete calls", w.Code, w.Body.String(), f.sec.callsOf("delete")-before)
	}
	f.sec.noRev = false
	f.sec.revErr = errSvcDown
	if w = f.do(http.MethodDelete, f.secretPath("corp-a"), f.admin, nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unreadable revision = %d, want 503", w.Code)
	}
	f.sec.revErr = nil
	if w = f.do(http.MethodDelete, f.secretPath("corp-a"), f.admin, nil); w.Code != http.StatusNoContent || f.sec.has(name) {
		t.Fatalf("clean delete = %d, record kept %v", w.Code, f.sec.has(name))
	}
	if w = f.do(http.MethodDelete, f.secretPath("corp-a"), f.admin, nil); w.Code != http.StatusNoContent {
		t.Errorf("deleting a missing record = %d, want 204", w.Code)
	}
}

func TestServiceGrant_TwoReplicasFailureOnASuccessOnB_PG(t *testing.T) {
	a := newSvcPG(t, true)
	a.put(svcPGRow("corp-a", true))
	a.writeSecret("corp-a", svcClientSecret)
	b := a.newReplica()
	ctx := context.Background()
	a.idp.respond = func(int, svcReq) svcAnswer { return svcOAuthError(401, "invalid_client") }
	_, _ = a.mint("corp-a")
	b.sec = a.sec
	if _, err := b.srv.serviceGrantToken(ctx, b.row("corp-a"), "", serviceGrantTriggerRefresh); err != nil {
		t.Fatalf("B: %v", err)
	}
	for _, r := range []*svcPG{a, b} {
		r.srv.serviceGrantHeartbeat(ctx)
	}
	state := func() string {
		rows, err := a.pg.ServiceGrantStatuses(ctx, []string{a.uid("corp-a")})
		if err != nil {
			t.Fatal(err)
		}
		return serviceGrantStatusOf(a.row("corp-a"), rows).State
	}
	if got := state(); got != serviceGrantStateFailed {
		t.Fatalf("aggregate = %q after A failed and B succeeded, want failing until A succeeds or ages out", got)
	}
	a.idp.respond = nil
	if _, err := a.mint("corp-a"); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != serviceGrantStateOK {
		t.Fatalf("aggregate = %q after A succeeded, want ok", got)
	}
	// An older attempt never overwrites a newer one.
	old := store.ServiceGrantStatusRow{ProviderUID: a.uid("corp-a"), ReplicaID: a.srv.svcGrant.replica(), ReplicaHost: "h",
		GrantDigest: serviceGrantDigest(a.row("corp-a")), ClientID: svcClientID, Outcome: "failed", Reason: "refused", AttemptedAt: time.Now().Add(-time.Hour)}
	if err := a.pg.UpsertServiceGrantStatus(ctx, old); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != serviceGrantStateOK {
		t.Fatalf("an older failed attempt overwrote a newer success: %q", got)
	}
}

func TestServiceGrant_QuietLongTokenStaysOK_PG(t *testing.T) {
	f := newSvcPG(t, true)
	f.put(svcPGRow("corp-a", true))
	f.writeSecret("corp-a", svcClientSecret)
	f.idp.respond = func(n int, _ svcReq) svcAnswer { return svcOK(n, 4*3600) }
	if _, err := f.mint("corp-a"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	f.srv.serviceGrantHeartbeat(ctx)
	// Three hours with no attempt: the heartbeat keeps the replica live, so the status stays ok.
	if _, err := f.pool.Exec(ctx, `UPDATE provider_service_grant_status SET attempted_at = now() - interval '3 hours', last_ok_at = now() - interval '3 hours'`); err != nil {
		t.Fatal(err)
	}
	f.srv.serviceGrantHeartbeat(ctx)
	rows, _ := f.pg.ServiceGrantStatuses(ctx, []string{f.uid("corp-a")})
	if st := serviceGrantStatusOf(f.row("corp-a"), rows); st.State != serviceGrantStateOK {
		t.Fatalf("quiet four-hour token state = %q, want ok", st.State)
	}
	// A failing replica keeps failing.
	if _, err := f.pool.Exec(ctx, `UPDATE provider_service_grant_status SET outcome = 'failed', reason = 'refused'`); err != nil {
		t.Fatal(err)
	}
	rows, _ = f.pg.ServiceGrantStatuses(ctx, []string{f.uid("corp-a")})
	if st := serviceGrantStatusOf(f.row("corp-a"), rows); st.State != serviceGrantStateFailed {
		t.Fatalf("a failing replica reads %q, want failing", st.State)
	}
}

func TestServiceGrant_DepartedReplicaDropsOut_PG(t *testing.T) {
	f := newSvcPG(t, true)
	f.put(svcPGRow("corp-a", true))
	f.writeSecret("corp-a", svcClientSecret)
	f.idp.respond = func(int, svcReq) svcAnswer { return svcOAuthError(401, "invalid_client") }
	_, _ = f.mint("corp-a")
	ctx := context.Background()
	f.srv.serviceGrantHeartbeat(ctx)
	read := func() string {
		rows, _ := f.pg.ServiceGrantStatuses(ctx, []string{f.uid("corp-a")})
		return serviceGrantStatusOf(f.row("corp-a"), rows).State
	}
	if got := read(); got != serviceGrantStateFailed {
		t.Fatalf("a live failing replica reads %q", got)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE provider_service_grant_status SET seen_at = now() - interval '4 minutes'`); err != nil {
		t.Fatal(err)
	}
	if got := read(); got == serviceGrantStateFailed {
		t.Fatalf("a replica unseen for 4 minutes still counts: %q", got)
	}
}

func TestSiteConfig_OldWriterKeepsProviderServiceGrants_PG(t *testing.T) {
	f := newSvcPG(t, true)
	f.put(svcPGRow("corp-a", true), svcPGRow("corp-b", true), svcPGRow("corp-c", true))
	uidA, uidB := f.uid("corp-a"), f.uid("corp-b")
	f.writeSecret("corp-a", svcClientSecret)
	// A document written through a type without the key keeps the stored grants; no stored row carries it.
	cfg := f.site()
	cfg.Branding = &types.SiteBranding{LogoPath: "/renamed-by-0.10.1.svg"}
	f.oldWriterSave(cfg)
	for _, id := range []string{"corp-a", "corp-b", "corp-c"} {
		if f.row(id).OIDC.ServiceGrant == nil {
			t.Fatalf("an old writer's save lost the grant of %s", id)
		}
	}
	if strings.Contains(f.stored("model_providers"), "service_grant") {
		t.Fatalf("a stored provider row carries service_grant: %s", f.stored("model_providers"))
	}
	// A 0.10.2 save round-trips them.
	if _, err := f.pg.PutSiteConfig(context.Background(), f.site()); err != nil || len(f.storedGrantUIDs()) != 3 {
		t.Fatalf("a 0.10.2 save: %v, stored %v", err, f.storedGrantUIDs())
	}
	// An old-type save that removes a row's oidc reads back with nothing attached and no grant served.
	cfg = f.site()
	cfg.ModelProviders.Providers[0].OIDC = nil
	f.oldWriterSave(cfg)
	if f.row("corp-a").OIDC != nil {
		t.Fatal("a row with no oidc has a grant attached")
	}
	if _, err := f.srv.serviceGrantToken(context.Background(), svcRowWithGrant(f.row("corp-a")), "", serviceGrantTriggerRefresh); err == nil {
		t.Error("a grant was served for a row with no oidc")
	}
	// A body naming provider_service_grants is 400 and no entry reaches the store.
	before := f.stored(store.ServiceGrantsKey)
	w := doSSO(t, f.srv, http.MethodPut, "/api/v1/site-config", f.admin, `{"provider_service_grants":{"x":{"token_url":"https://a.example.test/t"}}}`)
	if w.Code != http.StatusBadRequest || f.stored(store.ServiceGrantsKey) != before {
		t.Fatalf("a body naming the key = %d %s; stored changed %v", w.Code, w.Body.String(), f.stored(store.ServiceGrantsKey) != before)
	}
	// An inert entry survives a held change's save and goes at the next provider write, with its record.
	if !slices.Contains(f.storedGrantUIDs(), uidA) {
		t.Fatal("the inert entry did not survive the old writer's save")
	}
	if w := f.put(f.row("corp-b"), f.row("corp-c")); w.Code >= 300 {
		t.Fatalf("provider write: %d %s", w.Code, w.Body.String())
	}
	if slices.Contains(f.storedGrantUIDs(), uidA) || f.sec.has(serviceGrantSecretName(uidA)) || !slices.Contains(f.storedGrantUIDs(), uidB) {
		t.Fatalf("stored %v, record of A %v: want A's entry and record gone, B's kept", f.storedGrantUIDs(), f.sec.has(serviceGrantSecretName(uidA)))
	}
	// The last grant removed leaves the key empty, and a later read attaches nothing.
	for _, id := range []string{"corp-b", "corp-c"} {
		f.do(http.MethodDelete, "/model-providers/"+id+"/service-grant", f.admin, nil)
	}
	if len(f.storedGrantUIDs()) != 0 {
		t.Fatalf("stored grants after removing the last: %v", f.storedGrantUIDs())
	}
	for _, p := range f.rows() {
		if p.OIDC != nil && p.OIDC.ServiceGrant != nil {
			t.Errorf("%s still has a grant attached", p.ID)
		}
	}
}

func svcRowWithGrant(p types.ModelProvider) types.ModelProvider {
	if p.OIDC == nil {
		p.OIDC = &types.ProviderOIDC{}
	}
	o := *p.OIDC
	o.ServiceGrant = svcPGGrant()
	p.OIDC = &o
	return p
}

func TestServiceGrant_ActivationMarkSurvivesWholeDocumentSavesAndNamingItIs400_PG(t *testing.T) {
	f := newSvcPG(t, true)
	f.put(svcPGRow("corp-a", true))
	mark := f.stored(store.PortalIssuersActivatedKey)
	if mark == "" {
		t.Fatal("no mark")
	}
	f.oldWriterSave(f.site())
	if _, err := f.pg.PutSiteConfig(context.Background(), f.site()); err != nil {
		t.Fatal(err)
	}
	if got := f.stored(store.PortalIssuersActivatedKey); got != mark {
		t.Fatalf("mark = %q after whole-document saves, want %q", got, mark)
	}
	w := doSSO(t, f.srv, http.MethodPut, "/api/v1/site-config", f.admin, `{"`+store.PortalIssuersActivatedKey+`":"now"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("a body naming the mark = %d, want 400", w.Code)
	}
	if w := f.put(svcPGRow("corp-a", true)); w.Code >= 300 {
		t.Errorf("a grant write after the round trip: %d %s", w.Code, w.Body.String())
	}
}
