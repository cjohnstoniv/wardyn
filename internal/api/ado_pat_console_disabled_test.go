// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
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

// failForget is a secret store whose views refuse to delete one name.
type failForget struct {
	secretstore.Store
	name string
}

func (s failForget) For(owner string) secretstore.Store {
	return failForget{Store: s.Store.For(owner), name: s.name}
}

func (s failForget) Delete(ctx context.Context, name string) error {
	if name == s.name {
		return errors.New("injected: the store refused the delete")
	}
	return s.Store.Delete(ctx, name)
}

// A disconnect that fails on one row stops there: the rows after it were never tried, so they get
// no audit row and keep their sign-in for the retry.
func TestADODisconnect_StopsAtTheFirstRowThatFails(t *testing.T) {
	fx := newADOConsoleLane(t, false)
	git := fx.st.site.WorkspaceProviders.Git
	git[0].Disabled = true
	second := git[0]
	second.ID = "ado-row-2"
	fx.st.site.WorkspaceProviders.Git = append(git, second)
	held := fx.srv.cfg.Secrets.For(fx.subject)
	if err := held.Put(context.Background(), adoEntraSecretName(second.ID), []byte("{}")); err != nil {
		t.Fatal(err)
	}
	fx.srv.cfg.Secrets = failForget{Store: fx.srv.cfg.Secrets, name: adoEntraSecretName(git[0].ID)}

	if w := fx.disconnect(t, fx.subject); w.Code != http.StatusInternalServerError {
		t.Fatalf("disconnect with a failing first row = %d %s, want 500", w.Code, w.Body.String())
	}
	rows := fx.audit.find(adoPATAuditDisconnect)
	if len(rows) != 1 || rows[0].Target != git[0].ID || rows[0].Outcome != "failure" {
		t.Fatalf("ado_pat.disconnect rows = %+v, want one failure naming %q and none for the untried row", rows, git[0].ID)
	}
	names, err := held.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range names {
		found = found || n == adoEntraSecretName(second.ID)
	}
	if !found {
		t.Error("the untried row's sign-in was removed")
	}
}
