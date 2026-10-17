// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A bound leaver's email is also another principal's (a recycled address). Removing the leaver must not
// reach that principal through the address: their token, SSH key and session keep authenticating on both
// instances, read through the real auth paths rather than the rows.

func (e *scimEnv) key(fp string) types.SSHPublicKey {
	e.t.Helper()
	k, err := e.st.GetSSHKeyByFingerprint(context.Background(), fp)
	if err != nil {
		e.t.Fatal(err)
	}
	return k
}

func TestSCIMSuspendKeepsAnotherPrincipalSharingTheEmailAuthenticating(t *testing.T) {
	e := newSCIMEnv(t)
	ctx := context.Background()
	const email = "shared@corp.example"
	row := e.seedEntra("sub-leaver", email, oidLeaver)
	id := e.postUserID(e.a, oidLeaver, email, email)
	_, leaverRaw := e.seedToken("sub-leaver", email)
	leaverKey := e.key(e.seedKey("sub-leaver"))
	leaverCookie := scimCookie(t, "sub-leaver", email, row.AuthorityEpoch)
	_, newRaw := e.seedToken("sub-newhire", email)
	newKey := e.key(e.seedKey("sub-newhire"))
	newCookie := scimCookie(t, "sub-newhire", email, 0)
	for _, n := range []*scimNode{e.a, e.b} {
		if !e.tokenWorks(n, leaverRaw) || !e.cookieWorks(n, leaverCookie) || !e.tokenWorks(n, newRaw) || !e.cookieWorks(n, newCookie) ||
			n.srv.sshKeyRevocationRefusal(ctx, leaverKey) != "" || n.srv.sshKeyRevocationRefusal(ctx, newKey) != "" {
			t.Fatal("a credential did not work before the suspension")
		}
	}

	if w := e.patch(e.a, id, patchOf(`"False"`)); w.Code != http.StatusOK || decodeSCIMUser(t, w).Active {
		t.Fatalf("suspend = %d %s, want 200", w.Code, w.Body.String())
	}

	for name, n := range map[string]*scimNode{"a": e.a, "b": e.b} {
		if e.tokenWorks(n, leaverRaw) || e.cookieWorks(n, leaverCookie) || n.srv.sshKeyRevocationRefusal(ctx, leaverKey) == "" {
			t.Errorf("instance %s: a credential of the leaver still works", name)
		}
		if !e.tokenWorks(n, newRaw) {
			t.Errorf("instance %s: the other principal's token stopped authenticating", name)
		}
		if why := n.srv.sshKeyRevocationRefusal(ctx, newKey); why != "" {
			t.Errorf("instance %s: the other principal's SSH key is refused: %s", name, why)
		}
		if !e.cookieWorks(n, newCookie) {
			t.Errorf("instance %s: the other principal's session was cut through the shared email", name)
		}
	}
	if e.keyCount("sub-newhire") != 1 {
		t.Error("the other principal's SSH key was deleted")
	}
}

func TestSCIMGroupMemberRemoveKeepsAnotherPrincipalSharingTheEmail(t *testing.T) {
	w := newMoverWorld(t)
	e := w.e
	ctx := context.Background()
	tokNew, newRaw := e.seedToken("sub-newhire", moverEmail)
	// The other principal's snapshot holds the group too, so only ownership can spare the token.
	if _, err := e.pool.Exec(ctx, `UPDATE api_tokens SET groups = $2::jsonb WHERE id = $1`, tokNew, `["`+groupExternalID+`"]`); err != nil {
		t.Fatal(err)
	}
	newKey := e.key(e.seedKey("sub-newhire"))
	newCookie := scimCookie(t, "sub-newhire", moverEmail, 0)
	if !e.tokenWorks(e.b, newRaw) || !e.cookieWorks(e.b, newCookie) {
		t.Fatal("the other principal's credentials did not work before the removal")
	}

	if r := e.patchGroup(e.a, w.group.ID, groupRemovePatch(t, "entra-doc-patch-group-remove-members-value-array.json", w.moverID)); r.Code != http.StatusOK {
		t.Fatalf("group remove = %d %s", r.Code, r.Body.String())
	}

	w.checkRemoved(t, e.a, e.b)
	for name, n := range map[string]*scimNode{"a": e.a, "b": e.b} {
		if !e.tokenWorks(n, newRaw) {
			t.Errorf("instance %s: the other principal's token stopped authenticating", name)
		}
		if why := n.srv.sshKeyRevocationRefusal(ctx, newKey); why != "" {
			t.Errorf("instance %s: the other principal's SSH key is refused: %s", name, why)
		}
		if !e.cookieWorks(n, newCookie) {
			t.Errorf("instance %s: the other principal's session was cut through the shared email", name)
		}
	}
	if e.tokenRevoked(tokNew) {
		t.Error("the other principal's token was revoked")
	}
}
