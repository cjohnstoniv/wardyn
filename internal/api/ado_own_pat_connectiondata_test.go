// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Azure DevOps Services answers connectionData only without an api-version (or
// with a -preview one): ?api-version=7.1 is a 400 for every token, valid or not,
// so the Services identity URL must carry none, like the Server branch.
func TestADOOwnPATTarget_ServicesAsksWithoutAPIVersion(t *testing.T) {
	identityURL, org, ok := adoOwnPATTarget(ownPATTestRow())
	if !ok || org != "contoso" {
		t.Fatalf("adoOwnPATTarget = %q %q %v", identityURL, org, ok)
	}
	if strings.Contains(identityURL, "?") {
		t.Errorf("identity URL %q carries a query; Services answers connectionData?api-version=7.1 with a 400", identityURL)
	}
	if !strings.HasSuffix(identityURL, "/contoso/_apis/connectionData") {
		t.Errorf("identity URL = %q", identityURL)
	}
}

// A 400 is Azure DevOps refusing the request, not the token: the paste is not
// called a bad token, nothing is stored, and the audit row says which.
func TestADOOwnPATPut_ARefusedRequestIsNotARefusedToken(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(fake.Close)
	d := newOwnPATDoor(t, adoSite(ownPATTestRow()), nil)
	prev := adoOwnPATAPIBase
	adoOwnPATAPIBase = fake.URL
	t.Cleanup(func() { adoOwnPATAPIBase = prev })

	code, body := d.put(t, ownPATOrgKey, ownPATToken, days(10))
	if code != http.StatusBadGateway {
		t.Fatalf("PUT = %d %s, want 502", code, body)
	}
	var env struct{ Error, Reason string }
	_ = json.Unmarshal([]byte(body), &env)
	if env.Reason != reasonADOOwnPATRequestRefused || env.Error != adoOwnPATRequestRefusedRefusal {
		t.Errorf("body = %s, want reason %q and the request-refused sentence", body, reasonADOOwnPATRequestRefused)
	}
	if _, found := d.stored(t); found {
		t.Error("a token was stored after Azure DevOps refused the request")
	}
	rows := d.auditRows(adoPATAuditOwnStore)
	if len(rows) != 1 || rows[0].Outcome != "failure" || !strings.Contains(string(rows[0].Data), `"reason":"`+reasonADOOwnPATRequestRefused+`"`) {
		t.Fatalf("ado_pat.own.store rows = %+v, want one failure with reason %s", rows, reasonADOOwnPATRequestRefused)
	}
}
