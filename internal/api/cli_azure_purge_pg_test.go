// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Against Postgres and the real secret store: an Azure CLI row re-pinned to
// other subscriptions, then deleted, takes every person's sign-in for it out
// of every namespace and leaves everyone's other credentials alone. An edit
// that only reorders or re-cases the subscriptions, or changes the access,
// keeps the sign-ins: they are still for what the row pins.
func TestPG_AzureCLIRowWritesPurgeSignIns(t *testing.T) {
	const id = "6f0c1d2e-0000-4000-8000-0000000c0a08"
	srv, _, pool, sec := newProviderPurgePGHarness(t)
	name := cliSecretName(id, providerEntraPart)
	put := func(access string, subs ...string) {
		t.Helper()
		raw, _ := json.Marshal(azCLIRowDef(access, subs...))
		if w := do(t, srv, http.MethodPut, "/api/v1/components/"+id, adminToken, saveComponentBody("Azure", string(raw))); w.Code >= 300 {
			t.Fatalf("PUT = %d %s", w.Code, w.Body.String())
		}
	}
	seed := func() {
		for _, owner := range []string{"person-a", "person-b"} {
			mustPut(t, sec, owner, name)
			mustPut(t, sec, owner, "their-own-key")
		}
	}
	put(types.ComponentAccessRead, azCLISubA, azCLISubB)
	seed()
	put(types.ComponentAccessWrite, strings.ToUpper(azCLISubB), azCLISubA)
	if n := rowsLike(t, pool, cliSecretPrefix+"%"); n != 2 {
		t.Fatalf("an edit that kept the subscriptions left %d sign-ins, want both kept", n)
	}
	put(types.ComponentAccessWrite, azCLISubA)
	if n := rowsLike(t, pool, cliSecretPrefix+"%"); n != 0 {
		t.Fatalf("a re-pinned row left %d sign-ins", n)
	}
	seed()
	if w := do(t, srv, http.MethodDelete, "/api/v1/components/"+id, adminToken, ""); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s", w.Code, w.Body.String())
	}
	if n := rowsLike(t, pool, cliSecretPrefix+"%"); n != 0 {
		t.Fatalf("a deleted row left %d sign-ins", n)
	}
	if n := rowsLike(t, pool, "their-own-key"); n != 2 {
		t.Fatalf("other credentials = %d, want both people's kept", n)
	}
}
