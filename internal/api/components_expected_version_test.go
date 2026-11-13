// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// putAt is a component PUT body that names the version its editor opened.
func putAt(name string, def types.ComponentDefinition, version int) string {
	raw, _ := json.Marshal(map[string]any{"name": name, "definition": def, "expected_version": version})
	return string(raw)
}

// Two admins open one organisation component at version 1. The first saves a
// rename; the second's save, from the draft it opened, is refused before it
// writes or purges anything, so the row keeps the first save and a person's
// sign-in for it stays. A save at the current version goes through, and a save
// for a row deleted since it was opened creates nothing.
func TestComponents_StaleOrgEditIsRefusedBeforeItsWrite(t *testing.T) {
	srv, _, _ := componentsServer(t)
	admin := ssoSession(t, "sub-admin", "adm@corp.example", oidc.RoleAdmin)
	id := uuid.NewString()
	opened := azCLIRowDef(types.ComponentAccessRead, azCLISubA)
	if w := doSSO(t, srv, http.MethodPut, "/api/v1/components/"+id, admin, putAt("Azure", opened, 0)); w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body.String())
	}
	signIns := srv.cfg.Secrets.For("person-a")
	signIn := cliSecretName(id, providerEntraPart)
	if err := signIns.Put(context.Background(), signIn, []byte("refresh")); err != nil {
		t.Fatal(err)
	}

	if w := doSSO(t, srv, http.MethodPut, "/api/v1/components/"+id, admin, putAt("Azure dev", opened, 1)); w.Code != http.StatusOK || decodeSaved(t, w).Version != 2 {
		t.Fatalf("first save at version 1 = %d %s, want 200 at version 2", w.Code, w.Body.String())
	}
	repinned := azCLIRowDef(types.ComponentAccessRead, azCLISubB)
	w := doSSO(t, srv, http.MethodPut, "/api/v1/components/"+id, admin, putAt("Azure", repinned, 1))
	if w.Code != http.StatusConflict || errorReason(w) != "component_version_conflict" {
		t.Fatalf("second save at version 1 = %d %s, want 409 component_version_conflict", w.Code, w.Body.String())
	}
	if _, err := signIns.Get(context.Background(), signIn); err != nil {
		t.Fatalf("the refused save purged a sign-in: %v", err)
	}
	var rows []types.Component
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/components", admin, ""); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rows) != nil {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}
	if len(rows) != 1 || rows[0].Version != 2 || rows[0].Name != "Azure dev" || rows[0].Definition.CLI.Azure.Subscriptions[0] != azCLISubA {
		t.Fatalf("rows = %+v, want the first save's row at version 2", rows)
	}

	if w := doSSO(t, srv, http.MethodPut, "/api/v1/components/"+id, admin, putAt("Azure", repinned, 2)); w.Code != http.StatusOK || decodeSaved(t, w).Version != 3 {
		t.Fatalf("save at the current version = %d %s, want 200 at version 3", w.Code, w.Body.String())
	}
	if _, err := signIns.Get(context.Background(), signIn); err == nil {
		t.Error("a re-pin at the current version left the sign-in")
	}

	gone := uuid.NewString()
	if w := doSSO(t, srv, http.MethodPut, "/api/v1/components/"+gone, admin, putAt("Gone", opened, 1)); w.Code != http.StatusNotFound || errorReason(w) != "component_not_found" {
		t.Fatalf("save of a row that is gone = %d %s, want 404 component_not_found", w.Code, w.Body.String())
	}
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/components", admin, ""); json.Unmarshal(w.Body.Bytes(), &rows) != nil || len(rows) != 1 {
		t.Fatalf("after the save of a gone row: %s, want it not created", w.Body.String())
	}
}

// A person's own component edited in two tabs: the save from the tab opened
// before the other's save is refused and leaves the row as the other wrote it.
func TestComponents_StalePersonalEditIsRefused(t *testing.T) {
	srv, _, _ := componentsServer(t)
	member := ssoSession(t, "sub-member", "m@corp.example", oidc.RoleUser)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/me/components", member, saveComponentBody("Stripe", stripeDef))
	if w.Code != http.StatusCreated {
		t.Fatalf("save = %d %s", w.Code, w.Body.String())
	}
	saved := decodeSaved(t, w)
	path := "/api/v1/me/components/" + saved.ID.String()
	if w := doSSO(t, srv, http.MethodPut, path, member, putAt("Stripe live", saved.Definition, 1)); w.Code != http.StatusOK {
		t.Fatalf("first tab = %d %s", w.Code, w.Body.String())
	}
	w = doSSO(t, srv, http.MethodPut, path, member, putAt("Stripe old", saved.Definition, 1))
	if w.Code != http.StatusConflict || errorReason(w) != "component_version_conflict" {
		t.Fatalf("second tab = %d %s, want 409 component_version_conflict", w.Code, w.Body.String())
	}
	if w := doSSO(t, srv, http.MethodPut, path, member, putAt("Stripe live", saved.Definition, 0)); w.Code != http.StatusOK || decodeSaved(t, w).Version != 3 {
		t.Fatalf("a save naming no version = %d %s, want 200 at version 3", w.Code, w.Body.String())
	}
}
