// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"slices"
	"testing"
)

// An address already in the static list is not added again from the resolved
// targets, whatever its case; an invalid one is skipped.
func TestGapCovMailRecipientsDropsCaseInsensitiveDuplicates(t *testing.T) {
	got := mailRecipients(
		[]string{"Ops@Example.test", "ops@example.test", "not an address"},
		[]recipient{{Role: "requester", Email: "OPS@example.TEST"}, {Role: "approver", Email: "lead@example.test"}},
	)
	if want := []string{"Ops@Example.test", "lead@example.test"}; !slices.Equal(got, want) {
		t.Fatalf("mailRecipients = %v, want %v", got, want)
	}
}
