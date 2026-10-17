// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Against Postgres and the real secret store: an AWS CLI row re-pinned to
// another role, then deleted, takes every person's sign-in for it out of every
// namespace, and leaves everyone's other credentials alone.
func TestPG_AWSCLIRowWritesPurgeSignIns(t *testing.T) {
	srv, _, pool, sec := newProviderPurgePGHarness(t)
	name := cliSecretName(cliAWSID, providerSSOPart)
	put := func(roleName string) {
		t.Helper()
		def := cliAWSRowDef()
		def.CLI.AWS.RoleName = roleName
		raw, _ := json.Marshal(def)
		if w := do(t, srv, http.MethodPut, "/api/v1/components/"+cliAWSID, adminToken, saveComponentBody("AWS", string(raw))); w.Code >= 300 {
			t.Fatalf("PUT = %d %s", w.Code, w.Body.String())
		}
	}
	seed := func() {
		for _, owner := range []string{"person-a", "person-b"} {
			mustPut(t, sec, owner, name)
			mustPut(t, sec, owner, "their-own-key")
		}
	}
	put("ReadOnly")
	seed()
	put("Admin")
	if n := rowsLike(t, pool, cliSecretPrefix+"%"); n != 0 {
		t.Fatalf("a re-pinned row left %d sign-ins", n)
	}
	seed()
	if w := do(t, srv, http.MethodDelete, "/api/v1/components/"+cliAWSID, adminToken, ""); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s", w.Code, w.Body.String())
	}
	if n := rowsLike(t, pool, cliSecretPrefix+"%"); n != 0 {
		t.Fatalf("a deleted row left %d sign-ins", n)
	}
	if n := rowsLike(t, pool, "their-own-key"); n != 2 {
		t.Fatalf("other credentials = %d, want both people's kept", n)
	}
}
