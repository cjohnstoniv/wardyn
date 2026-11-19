// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auditreceipt"
)

// TestReceiptRedactableActionsMatchHostViews: the offline verifier accepts a withheld row only for an action
// the host-view projector redacts, so its list must be exactly auditHostViews' actions plus their legacy names.
func TestReceiptRedactableActionsMatchHostViews(t *testing.T) {
	want := map[string]bool{}
	for a := range auditHostViews {
		want[a] = true
	}
	for legacy, canonical := range legacyAuditActions {
		if _, ok := auditHostViews[canonical]; ok {
			want[legacy] = true
		}
	}
	for a := range want {
		if !auditreceipt.RedactableActions[a] {
			t.Errorf("%s is served redacted but auditreceipt.RedactableActions does not list it", a)
		}
	}
	for a := range auditreceipt.RedactableActions {
		if !want[a] {
			t.Errorf("auditreceipt.RedactableActions lists %s, which is never served redacted", a)
		}
	}
}
