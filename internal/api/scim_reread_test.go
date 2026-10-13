// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"
)

// A suspension whose re-read fails answers 5xx so the identity provider retries; it never answers a 2xx
// built from a zero identity.
func TestSCIMSuspendReReadFailureAnswers5xx(t *testing.T) {
	zero := "00000000-0000-0000-0000-000000000000"

	st := newSCIMCovStore()
	// suspendIdentity reads the row once; the re-read after it is the second call.
	st.failAfter("GetIdentity", errSCIMCovBoom, 1)
	e := newSCIMCovEnv(t, st)
	w := e.scim(http.MethodPost, "/scim/v2/Users", scimCovUserBody(scimCovOID, "Pat", false, "pat@corp.example"))
	scimCovWantRetry(t, w, "secret-dsn")
	if strings.Contains(w.Body.String(), zero) {
		t.Errorf("POST body carries a zero identity: %s", w.Body.String())
	}

	st = newSCIMCovStore()
	pat := st.addIdentity(scimCovBound())
	// scimIdentity, then suspendIdentity, then the re-read.
	st.failAfter("GetIdentity", errSCIMCovBoom, 2)
	e = newSCIMCovEnv(t, st)
	w = e.scim(http.MethodPatch, "/scim/v2/Users/"+pat.ID.String(), scimCovPatch(scimCovReplace("active", "false")))
	scimCovWantRetry(t, w, "secret-dsn")
	if strings.Contains(w.Body.String(), zero) {
		t.Errorf("PATCH body carries a zero identity: %s", w.Body.String())
	}
}

// A PUT that omits active changes nothing about whether the identity is active: a suspended identity stays
// suspended and no reactivation row is written. A POST keeps "absent means true".
func TestSCIMPutWithoutActiveLeavesASuspendedIdentitySuspended(t *testing.T) {
	st := newSCIMCovStore()
	row := scimCovBound()
	gone := scimCovNow
	row.DeactivatedAt = &gone
	pat := st.addIdentity(row)
	e := newSCIMCovEnv(t, st)

	body := `{` + scimCovUserSchema + `,"externalId":"` + scimCovOID + `","userName":"Pat.Put@corp.example"}`
	w := e.scim(http.MethodPut, "/scim/v2/Users/"+pat.ID.String(), body)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT without active = %d %s", w.Code, w.Body.String())
	}
	if st.identity(pat.ID).DeactivatedAt == nil {
		t.Error("a PUT without active reactivated the identity")
	}
	for _, r := range e.auditRows("scim.user.write") {
		if e.auditData(r)["op"] == "reactivate" {
			t.Errorf("a PUT without active wrote a reactivate row: %+v", r)
		}
	}

	w = e.scim(http.MethodPost, "/scim/v2/Users", `{`+scimCovUserSchema+`,"externalId":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","userName":"Lee"}`)
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"active":true`) {
		t.Errorf("POST without active = %d %s, want a created active user", w.Code, w.Body.String())
	}
}
