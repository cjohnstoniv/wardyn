// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"
)

// A person whose Azure DevOps row was disabled still disconnects: the stored sign-in is forgotten,
// their live token is revoked, and the audit row names the row. Disconnect is a cleanup, not a use.
func TestADODisconnect_DisabledRowStillForgetsTheSignIn(t *testing.T) {
	fx := newADOConsoleLane(t, true)
	if len(fx.st.unrevoked(t)) != 1 {
		t.Fatal("the fixture's dispatch left no live token")
	}
	fx.st.site.WorkspaceProviders.Git[0].Disabled = true
	rowID := fx.st.site.WorkspaceProviders.Git[0].ID

	if w := fx.disconnect(t, fx.subject); w.Code != http.StatusNoContent {
		t.Fatalf("disconnect on a disabled row = %d %s, want 204", w.Code, w.Body.String())
	}
	if _, stored := fx.stored(t, fx.subject); stored {
		t.Error("the stored sign-in survived the disconnect")
	}
	if left := fx.st.unrevoked(t); len(left) != 0 {
		t.Errorf("live tokens after disconnect = %+v, want none", left)
	}
	rows := fx.audit.find(adoPATAuditDisconnect)
	if len(rows) != 1 || rows[0].Target != rowID || !strings.Contains(string(rows[0].Data), `"removed":true`) {
		t.Errorf("ado_pat.disconnect rows = %+v, want one removed row naming %q", rows, rowID)
	}

	// Nothing held any more: the disabled row answers as none.
	if w := fx.disconnect(t, fx.subject); w.Code != http.StatusNotFound {
		t.Errorf("a second disconnect = %d %s, want 404", w.Code, w.Body.String())
	}
}
