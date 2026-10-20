// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The record card is member-visible: a dispatch failure names the audited
// reason code at most, never the error text the audit row carries.
func TestDispatchFailureReason_NamesOnlyTheReasonCode(t *testing.T) {
	fail := func(action, data string) types.AuditEvent {
		return types.AuditEvent{Action: action, Outcome: "failure", Data: []byte(data)}
	}
	raw := `failed to connect to host=db.internal port=5432 SQLSTATE 57P01 /var/lib/docker`
	for name, tc := range map[string]struct {
		events []types.AuditEvent
		want   string
	}{
		"code and error":   {[]types.AuditEvent{fail("run.create", `{"error":"`+raw+`","reason":"sandbox_create"}`)}, "(sandbox_create)"},
		"error only":       {[]types.AuditEvent{fail("run.dispatch", `{"error":"list the run's grants: `+raw+`"}`)}, "audit events carry the detail"},
		"note only":        {[]types.AuditEvent{fail("run.create", `{"note":"`+raw+`"}`)}, "audit events carry the detail"},
		"free-text reason": {[]types.AuditEvent{fail("run.create", `{"reason":"`+raw+`"}`)}, "audit events carry the detail"},
		"last code wins": {[]types.AuditEvent{
			fail("run.create", `{"reason":"image_arch"}`), fail("run.dispatch", `{"error":"`+raw+`","reason":"placement_denied"}`),
		}, "(placement_denied)"},
	} {
		got := dispatchFailureReason(tc.events)
		if !strings.Contains(got, tc.want) || strings.Contains(got, "db.internal") || strings.Contains(got, "/var/lib") {
			t.Errorf("%s: hint %q, want %q and no error text", name, got, tc.want)
		}
	}
}
