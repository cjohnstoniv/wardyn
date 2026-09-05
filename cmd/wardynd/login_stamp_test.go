// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// loginStampCall records one re-stamp exactly as the store received it — which
// is the point: the defect this file pins is not a missing call, it is a call
// made with the right method and the wrong ARGUMENTS.
type loginStampCall struct {
	principal string
	role      string
	checkedAt time.Time
}

type recordingLoginStampStore struct {
	sshCalls   []loginStampCall
	tokenCalls []loginStampCall
	sshErr     error
	tokenErr   error
}

func (r *recordingLoginStampStore) RefreshSSHKeyRoles(_ context.Context, principal, role string, checkedAt time.Time) error {
	r.sshCalls = append(r.sshCalls, loginStampCall{principal: principal, role: role, checkedAt: checkedAt})
	return r.sshErr
}

func (r *recordingLoginStampStore) RefreshAPITokenRoles(_ context.Context, principal, role string) error {
	r.tokenCalls = append(r.tokenCalls, loginStampCall{principal: principal, role: role})
	return r.tokenErr
}

// TestRefreshLoginStampsPassesPrincipalAndRoleInOrder is the demoted-admin bound,
// driven rather than grepped.
//
// The only guard this bound had was apitoken_stamp_doc_test.go's
// strings.Contains(boot_deps.go, "RefreshAPITokenRoles"): a check that the METHOD
// NAME appears in the file. Transposing the two string arguments —
// RefreshAPITokenRoles(ctx, role, sub) — leaves that grep satisfied, stamps the
// role column of whatever principal happens to be literally named "member", and
// leaves every demoted admin's outstanding wdn_ tokens authenticating as an
// admin. cmd/wardynd, internal/auth/oidc and internal/store all stayed green
// through exactly that edit.
//
// The two arguments are both plain strings, adjacent, and in the same order in
// both signatures, which is what makes the transposition survivable. So the
// fixture picks values that could not be confused for one another by a human
// reading a failure, and asserts the position of each.
func TestRefreshLoginStampsPassesPrincipalAndRoleInOrder(t *testing.T) {
	const sub, role = "auth0|demoted-admin", "member"
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

	st := &recordingLoginStampStore{}
	refreshLoginStamps(context.Background(), st, sub, role, now)

	for _, c := range []struct {
		lane  string
		calls []loginStampCall
	}{
		{"ssh_public_keys", st.sshCalls},
		{"api_tokens", st.tokenCalls},
	} {
		if len(c.calls) != 1 {
			t.Fatalf("%s: %d re-stamps, want exactly 1 — a login must bound both credential lanes", c.lane, len(c.calls))
		}
		got := c.calls[0]
		if got.principal != sub {
			t.Errorf("%s: re-stamped principal %q, want %q — the arguments are transposed, so this stamps the role "+
				"column of a principal literally named %q and leaves the demoted human's credentials frozen at admin",
				c.lane, got.principal, sub, role)
		}
		if got.role != role {
			t.Errorf("%s: re-stamped role %q, want %q", c.lane, got.role, role)
		}
	}
	// The key lane's stamp carries the caller's clock, not its own: OnLogin
	// passes time.Now().UTC(), and WARDYN_SSH_ROLE_TTL measures staleness from
	// exactly this value.
	if !st.sshCalls[0].checkedAt.Equal(now) {
		t.Errorf("ssh_public_keys: checked_at = %s, want the caller's %s", st.sshCalls[0].checkedAt, now)
	}
}

// TestRefreshLoginStampsIsBestEffortInBothDirections pins the contract that makes
// this a callback rather than a gate: a store hiccup must not fail the login, and
// — the half that is easy to lose in a refactor — a failure of the FIRST stamp
// must not skip the SECOND. They bound two independent credential lanes; one
// being unreachable is no reason to leave the other stale.
func TestRefreshLoginStampsIsBestEffortInBothDirections(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sshErr   error
		tokenErr error
	}{
		{"the ssh lane errors", errors.New("ssh stamp: connection refused"), nil},
		{"the token lane errors", nil, errors.New("token stamp: connection refused")},
		{"both lanes error", errors.New("ssh down"), errors.New("tokens down")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &recordingLoginStampStore{sshErr: tc.sshErr, tokenErr: tc.tokenErr}
			refreshLoginStamps(context.Background(), st, "auth0|someone", "admin", time.Now().UTC())
			if len(st.sshCalls) != 1 || len(st.tokenCalls) != 1 {
				t.Fatalf("ssh=%d token=%d re-stamps — a failing lane must be logged and stepped over, "+
					"never allowed to skip the other lane's bound", len(st.sshCalls), len(st.tokenCalls))
			}
		})
	}
}
