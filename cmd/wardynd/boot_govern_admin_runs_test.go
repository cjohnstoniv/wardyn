// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
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

// TestParseGovernAdminRunsExempt: the only value is "recording"; anything else
// exits 2; set without the switch it warns that it does nothing.
func TestParseGovernAdminRunsExempt(t *testing.T) {
	for _, tc := range []struct {
		name, csv  string
		on         bool
		want       []string
		wantExit2  bool
		wantWarned bool
	}{
		{"unset", "", true, nil, false, false},
		{"recording", "recording", true, []string{"recording"}, false, false},
		{"spaces and a repeat", " recording , recording", true, []string{"recording"}, false, false},
		{"recording without the switch", "recording", false, []string{"recording"}, false, true},
		{"unset without the switch", "", false, nil, false, false},
		{"another lane", "runs", true, nil, true, false},
		{"recording and another lane", "recording,runs", true, nil, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			got, err := parseGovernAdminRunsExempt(tc.csv, tc.on)

			if tc.wantExit2 {
				var ec *exitCodeError
				if !errors.As(err, &ec) || ec.code != 2 {
					t.Fatalf("err = %v, want an exit-2 refusal", err)
				}
				return
			}
			if err != nil || strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
			if warned := strings.Contains(buf.String(), "does nothing"); warned != tc.wantWarned {
				t.Errorf("warned = %v, want %v; log: %q", warned, tc.wantWarned, buf.String())
			}
		})
	}
}
