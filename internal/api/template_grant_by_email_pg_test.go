// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestPG_TemplateGroupGrantByEmailResolvesThroughTheDirectory runs the grant route against the real
// identity directory: an email nobody is recorded under and an email two identities share are refused, and
// a known email, in any case, is stored as its principal so a revoke by email finds the grant.
// Guarded by WARDYN_TEST_PG.
func TestPG_TemplateGroupGrantByEmailResolvesThroughTheDirectory(t *testing.T) {
	_, srv, pg, _ := identityRig(t)
	sfx := uuid.NewString()
	carol, carolMail, twinMail := "carol-"+sfx, "carol-"+sfx+"@corp.example", "twin-"+sfx+"@corp.example"
	seedIdentity(t, pg, carol, "", carolMail)
	seedIdentity(t, pg, "twin-a-"+sfx, "", twinMail)
	seedIdentity(t, pg, "twin-b-"+sfx, "", twinMail)
	call := func(method, person string) (int, string) {
		w := do(t, srv, method, "/api/v1/admin/template-group-admins", adminToken, `{"group_id":"eng","person":"`+person+`"}`)
		return w.Code, w.Body.String()
	}

	if code, body := call(http.MethodPut, "nobody-"+sfx+"@corp.example"); code != http.StatusUnprocessableEntity || !strings.Contains(body, reasonOwnerUnresolved) {
		t.Errorf("an email no one is recorded under = %d %s, want 422 %s", code, body, reasonOwnerUnresolved)
	}
	if code, body := call(http.MethodPut, twinMail); code != http.StatusUnprocessableEntity || !strings.Contains(body, reasonOwnerAmbiguous) {
		t.Errorf("an email two identities share = %d %s, want 422 %s", code, body, reasonOwnerAmbiguous)
	}
	if grants, _ := pg.ListTemplateGroupAdmins(t.Context()); len(grants) != 0 {
		t.Fatalf("a refused grant was stored: %+v", grants)
	}

	if code, body := call(http.MethodPut, strings.ToUpper(carolMail)); code != http.StatusOK || !strings.Contains(body, `"person":"`+carol+`"`) {
		t.Fatalf("grant by a known email in capitals = %d %s, want it stored as %s", code, body, carol)
	}
	if grants, _ := pg.ListTemplateGroupAdmins(t.Context()); len(grants) != 1 || grants[0].Person != carol {
		t.Fatalf("stored grants = %+v, want one for %s", grants, carol)
	}
	if code, body := call(http.MethodDelete, carolMail); code != http.StatusNoContent {
		t.Fatalf("revoke by email = %d %s", code, body)
	}
	if grants, _ := pg.ListTemplateGroupAdmins(t.Context()); len(grants) != 0 {
		t.Fatalf("the revoke by email left the grant: %+v", grants)
	}
}
