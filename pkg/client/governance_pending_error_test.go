// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The message names every held change by id, and says so when dependent writes
// were deferred or prune did not run, so an operator can find the changes to
// approve and knows the apply is incomplete.
func TestPendingApprovalErrorMessage(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	changes := []client.GovernanceChange{{ID: a}, {ID: b}}

	plain := (&client.PendingApprovalError{Changes: changes[:1]}).Error()
	if want := "pending approval: 1 governance change(s) stored, not applied (" + a.String() + ")"; plain != want {
		t.Errorf("message = %q, want %q", plain, want)
	}

	full := (&client.PendingApprovalError{
		Changes:      changes,
		Deferred:     []client.GovernanceDeferredWrite{{Profile: "child", Base: "base"}},
		PruneSkipped: true,
	}).Error()
	for _, want := range []string{"2 governance change(s)", a.String() + ", " + b.String(), "1 dependent write(s) deferred", "prune skipped"} {
		if !strings.Contains(full, want) {
			t.Errorf("message %q lacks %q", full, want)
		}
	}
}
