// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// The credentials erasure step also deletes the person's spire token bindings: a row whose sub is the
// person, and on purpose a row whose sponsor alone is the person (another subject's token must not
// outlive its sponsor's erasure; with no binding row it is refused at its next verify). An unrelated row
// is kept.
func TestPG_PersonErasure_DeletesTheTokenBindings(t *testing.T) {
	l := newMaskLab(t)
	fail := false
	a := erasureReplica(l, &fail)
	const alice, bob = "alice-sub", "bob-sub"
	pg := store.NewPG(l.pool)
	row := func(sub, sponsor string) identity.TokenBinding {
		run := uuid.New()
		b := identity.TokenBinding{JTI: uuid.NewString(), RunID: run, SPIFFEID: "spiffe://wardyn.local/agent-run/" + run.String(),
			Sub: sub, Sponsor: sponsor, Expiry: time.Now().Add(time.Hour)}
		if err := pg.PutTokenBinding(t.Context(), b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	asSub, asSponsor, unrelated := row("Alice-Sub", alice), row(bob, alice), row(bob, bob)
	if err := a.sec.For(alice).Put(t.Context(), "anthropic-api-key", []byte("alice-credential-value-0000")); err != nil {
		t.Fatal(err)
	}
	sec := ssoSession(t, "sec-sub", "sec@corp.example", oidc.RoleSecurityAdmin)
	if w := doSSO(t, a.srv, http.MethodPost, "/api/v1/people/"+alice+"/erasure", sec, erasureBody("credentials")); w.Code != http.StatusOK {
		t.Fatalf("erase credentials = %d %s, want 200", w.Code, w.Body)
	}
	for name, b := range map[string]identity.TokenBinding{"sub": asSub, "sponsor-only": asSponsor} {
		if _, err := pg.GetTokenBinding(t.Context(), b.JTI); !errors.Is(err, identity.ErrBindingNotFound) {
			t.Errorf("the %s row survived the erasure: %v", name, err)
		}
	}
	if _, err := pg.GetTokenBinding(t.Context(), unrelated.JTI); err != nil {
		t.Errorf("an unrelated row was deleted: %v", err)
	}
}
