// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestWarnGovernAdminRunsUnbound: WARDYN_GOVERN_ADMIN_RUNS binds nobody without
// OIDC (local mode and admin-token-only mode carry no person), so boot says so;
// it stays silent whenever the switch is off or SSO is configured.
func TestWarnGovernAdminRunsUnbound(t *testing.T) {
	for _, tc := range []struct {
		name       string
		on, oidc   bool
		wantWarned bool
	}{
		{"switch on, OIDC unset", true, false, true},
		{"switch on, OIDC configured", true, true, false},
		{"switch off, OIDC unset", false, false, false},
		{"switch off, OIDC configured", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			warnGovernAdminRunsUnbound(tc.on, tc.oidc)

			out := buf.String()
			if warned := strings.Contains(out, "WARDYN_GOVERN_ADMIN_RUNS") && strings.Contains(out, "binds nobody"); warned != tc.wantWarned {
				t.Errorf("warned = %v, want %v; log: %q", warned, tc.wantWarned, out)
			}
			if tc.wantWarned && !strings.Contains(out, "level=WARN") {
				t.Errorf("want a WARN, got %q", out)
			}
		})
	}
}
