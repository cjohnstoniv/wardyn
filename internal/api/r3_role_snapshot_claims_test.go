// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// refreshAPITokenIdentityExists is the COMPILE-TIME anchor for every claim this
// file pins: the store contract really does re-stamp an api_token's role, so a
// comment or an operator-facing line saying no sign-in refreshes it is not a
// stylistic quibble but a false statement about the system. If the refresh is
// ever removed, this stops compiling and the claims below are re-opened for
// review rather than silently inverted a second time.
var refreshAPITokenIdentityExists func(store.Store, context.Context, string, string, []string, bool) error = store.Store.RefreshAPITokenIdentity

// TestRoleSnapshotClaimsMatchTheRefreshThatShipped is F281.
//
// The role-mapping WARN told operators "a token's role is frozen at mint and no
// sign-in refreshes it" — in the same release that gave the token lane the login
// hook the key lane had since 0046 (store.RefreshAPITokenRoles; CHANGELOG 0.7
// states it plainly). The doc comment beside it said "a wdn_ token has no such
// bound". Both described the code as it stood before that change, and an
// operator acting on a stale remedy line either does unnecessary work or
// assumes a bound that is not there.
//
// The demotion path has since gained a second, sharper answer (F112: the edit
// revokes what it demotes), so the line now has THREE true things to keep
// straight at once. This pins all three rather than banning one sentence.
func TestRoleSnapshotClaimsMatchTheRefreshThatShipped(t *testing.T) {
	_ = refreshAPITokenIdentityExists

	t.Run("the operator-facing remedy names what actually happens", func(t *testing.T) {
		for _, want := range []struct{ frag, why string }{
			{"were revoked", "this edit already revoked the principals it demoted (F112) — an operator told to go revoke them is being sent to do work that is done"},
			{"next sign-in re-stamps the role", "the stamp is bounded-stale, not frozen: RefreshAPITokenIdentity fires from the OnLogin hook"},
			{"RefreshAPITokenIdentity", "name the mechanism, so the claim can be checked against the code that makes it true"},
			{"take effect immediately or the owner will not sign in again", "the two cases where the login bound is not enough are exactly when the lever is the right answer (CHANGELOG 0.7's own wording)"},
		} {
			if !strings.Contains(roleSnapshotWarnRemedy, want.frag) {
				t.Errorf("the role-mapping WARN remedy does not say %q — %s\nremedy=%s", want.frag, want.why, roleSnapshotWarnRemedy)
			}
		}
	})

	// The inverted claims, banned by their exact wording. The OnLogin
	// re-stamp covers the group snapshot as well as the role
	// (store.RefreshAPITokenIdentity), so "frozen at mint"/"signing in again
	// does not refresh it" is false for both halves, and every ban below
	// applies to either.
	t.Run("no source claim contradicts the refresh", func(t *testing.T) {
		banned := []struct{ frag, why string }{
			{"no sign-in refreshes it", "RefreshAPITokenIdentity is fired by the OnLogin hook for both columns now"},
			{"has no such bound", "the login re-stamp IS the bound; the SSH lane's is a TTL, which is a different bound, not a missing one"},
			{"read verbatim forever", "verbatim until the owner's next login re-stamps it, or until this deployment's demotion revokes it"},
			{"frozen at mint", "the stamp refreshes at the owner's next sign-in; the only unbounded case is an owner who never signs in again"},
			{"does not touch groups", "RefreshAPITokenIdentity re-stamps groups together with role, in the same call"},
		}
		for _, file := range []string{"apitokens.go", "access.go"} {
			src, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("read %s: %v", file, err)
			}
			for _, b := range banned {
				if strings.Contains(string(src), b.frag) {
					t.Errorf("%s still claims %q — %s", file, b.frag, b.why)
				}
			}
		}
	})
}
