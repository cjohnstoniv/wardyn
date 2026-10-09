// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// TestPeopleEntra_UnbindAfterAppReRegistration is #1818 end to end through the real callback: a person
// bound to the pairwise sub of the old app registration is refused under the new one; only a super
// admin can unbind the row; after the unbind the next sign-in binds it to the new sub and the row keeps
// its authority epoch. The unbind is audited with the principal it released, and it refuses a
// deactivated identity, an unbound one and an unknown id.
func TestPeopleEntra_UnbindAfterAppReRegistration(t *testing.T) {
	e := newEntraPeoplePG(t, func(p peoplePG, c *oidc.Config) { c.Identities = NewIdentityGate(p.st) })
	ctx := context.Background()

	e.signIn("pairwise-old", entraTenant, entraObject)
	if _, sess := e.callback(t); sess == nil {
		t.Fatal("first sign-in refused")
	}
	bound, err := e.st.GetIdentityByObject(ctx, e.issuer, entraTenant, entraObject)
	if err != nil || bound.Principal != "pairwise-old" {
		t.Fatalf("identity = %+v (%v), want bound to pairwise-old", bound, err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE principal_identities SET authority_epoch = 7 WHERE id = $1`, bound.ID); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/admin/identities/" + bound.ID.String() + "/unbind"

	e.signIn("pairwise-new", entraTenant, entraObject)
	if _, sess := e.callback(t); sess != nil {
		t.Fatal("a sign-in under the new app registration's sub was admitted to a row bound to the old one")
	}

	for name, as := range map[string]*http.Cookie{"member": e.mem, "security admin": e.sec} {
		if w := doSSO(t, e.h.srv, http.MethodPost, path, as, ""); w.Code != http.StatusForbidden {
			t.Errorf("%s unbind = %d %s, want 403", name, w.Code, w.Body.String())
		}
	}
	if got, _ := e.st.GetIdentity(ctx, bound.ID); got.Principal != "pairwise-old" {
		t.Fatalf("a refused caller unbound the identity: %+v", got)
	}
	if w := doSSO(t, e.h.srv, http.MethodPost, path, e.super, ""); w.Code != http.StatusOK {
		t.Fatalf("super admin unbind = %d %s, want 200", w.Code, w.Body.String())
	}
	rows := e.auditRows("identity.unbind")
	if len(rows) != 1 || rows[0].Target != bound.ID.String() || rows[0].Outcome != "success" {
		t.Fatalf("identity.unbind rows = %+v, want one success on the identity", rows)
	}
	var data map[string]any
	if err := json.Unmarshal(rows[0].Data, &data); err != nil || data["principal"] != "pairwise-old" || data["unbound_by"] != rows[0].Actor {
		t.Errorf("identity.unbind data = %s (%v), want the released principal and the admin", rows[0].Data, err)
	}

	if w := doSSO(t, e.h.srv, http.MethodPost, path, e.super, ""); w.Code != http.StatusConflict {
		t.Errorf("second unbind = %d %s, want 409 identity_not_bound", w.Code, w.Body.String())
	}
	if _, sess := e.callback(t); sess == nil {
		t.Fatal("sign-in under the new sub was refused after the unbind")
	}
	got, err := e.st.GetIdentity(ctx, bound.ID)
	if err != nil || got.Principal != "pairwise-new" || got.AuthorityEpoch != 7 || got.DeactivatedAt != nil {
		t.Fatalf("identity after the sign-in = %+v (%v), want bound to pairwise-new with epoch 7 kept", got, err)
	}

	if _, err := e.pool.Exec(ctx, `UPDATE principal_identities SET deactivated_at = now() WHERE id = $1`, bound.ID); err != nil {
		t.Fatal(err)
	}
	if w := doSSO(t, e.h.srv, http.MethodPost, path, e.super, ""); w.Code != http.StatusConflict {
		t.Errorf("unbind of a deactivated identity = %d %s, want 409", w.Code, w.Body.String())
	}
	if got, _ := e.st.GetIdentity(ctx, bound.ID); got.Principal != "pairwise-new" {
		t.Errorf("a deactivated identity was unbound: %+v", got)
	}
	if w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/admin/identities/"+uuid.NewString()+"/unbind", e.super, ""); w.Code != http.StatusNotFound {
		t.Errorf("unbind of an unknown identity = %d, want 404", w.Code)
	}
	if n := len(e.auditRows("identity.unbind")); n != 1 {
		t.Errorf("%d identity.unbind rows after the refusals, want still 1", n)
	}
}
