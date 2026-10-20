// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// listFailsFor is a secret store whose List fails for one owner's namespace:
// the last read of subKnown's chain, so reaching it refuses the sign-in.
type listFailsFor struct {
	secretstore.Store
	owner string
}

func (s listFailsFor) For(owner string) secretstore.Store {
	if owner != s.owner {
		return s.Store.For(owner)
	}
	return listErrStore{s.Store.For(owner)}
}

type listErrStore struct{ secretstore.Store }

func (listErrStore) List(context.Context) ([]string, error) {
	return nil, errors.New("secret store unavailable")
}

// attachedEntraPerson sets up the object-id person behind the sign-in gate and
// signs pairwise-a in once, so its identity row is bound to the person.
func attachedEntraPerson(t *testing.T) entraPeoplePG {
	t.Helper()
	e := newEntraPeoplePG(t, func(p peoplePG, c *oidc.Config) { c.Identities = NewIdentityGate(p.st) })
	if w := e.createEntraPerson(t, `{"tenant_id":"`+entraTenant+`","object_id":"`+entraObject+`"}`); w.Code != http.StatusCreated {
		t.Fatalf("object-id create: %d %s", w.Code, w.Body.String())
	}
	e.signIn("pairwise-a", entraTenant, entraObject)
	if _, sess := e.callback(t); sess == nil {
		t.Fatal("first sign-in refused")
	}
	row, err := e.st.GetIdentityByObject(context.Background(), e.issuer, entraTenant, entraObject)
	if err != nil || row.Principal != entraPerson {
		t.Fatalf("identity = %+v (%v), want bound to %s", row, err, entraPerson)
	}
	return e
}

// signsInAs signs pairwise-a in again and reports the principal its session acts as.
func (e entraPeoplePG) signsInAs(t *testing.T) string {
	t.Helper()
	e.signIn("pairwise-a", entraTenant, entraObject)
	w, sess := e.callback(t)
	if sess == nil {
		t.Fatalf("sign-in refused: %d %s", w.Code, w.Header().Get("Location"))
	}
	return decodeToken(t, doSSO(t, e.h.srv, http.MethodPost, "/api/v1/me/tokens", sess, `{"name":"who"}`)).Principal
}

// TestPeopleEntra_AttachedPersonSkipsSubKnown: a sign-in whose identity row is
// already bound to the object-id person resolves to them without reading what
// the raw sub owns, so a secret store that cannot list does not refuse it. A
// first attach still reads it.
func TestPeopleEntra_AttachedPersonSkipsSubKnown(t *testing.T) {
	e := newEntraPeoplePG(t, func(p peoplePG, c *oidc.Config) { c.Identities = NewIdentityGate(p.st) })
	if w := e.createEntraPerson(t, `{"tenant_id":"`+entraTenant+`","object_id":"`+entraObject+`"}`); w.Code != http.StatusCreated {
		t.Fatalf("object-id create: %d %s", w.Code, w.Body.String())
	}
	orig := e.h.srv.cfg.Secrets
	e.h.srv.cfg.Secrets = listFailsFor{orig, "pairwise-a"}
	e.signIn("pairwise-a", entraTenant, entraObject)
	if _, sess := e.callback(t); sess != nil {
		t.Fatal("first attach with an unlistable secret store signed in; want subKnown read and the sign-in refused")
	}
	e.h.srv.cfg.Secrets = orig

	e = attachedEntraPerson(t)
	e.h.srv.cfg.Secrets = listFailsFor{e.h.srv.cfg.Secrets, "pairwise-a"}
	if got := e.signsInAs(t); got != entraPerson {
		t.Fatalf("attached sign-in acts as %q, want %q", got, entraPerson)
	}
}

// TestPeopleEntra_AttachedPersonDoesNotFlipBack: once attached, a workspace
// that later appears under the raw sub does not move the person back to it.
func TestPeopleEntra_AttachedPersonDoesNotFlipBack(t *testing.T) {
	e := attachedEntraPerson(t)
	if _, err := e.st.CreateWorkspace(context.Background(), types.Workspace{
		ID: uuid.New(), Name: "ws-a", OwnedBy: "pairwise-a", Status: types.WorkspaceScanned,
		Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if got := e.signsInAs(t); got != entraPerson {
		t.Fatalf("sign-in after a workspace appeared under the sub acts as %q, want still %q", got, entraPerson)
	}
}
