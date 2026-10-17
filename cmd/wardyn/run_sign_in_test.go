// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// `wardyn run sign-in` reads once and prints the page and code when waiting, a
// plain line when not, and names a refusal's reason.
func TestRunSignIn_PrintsTheAnswer(t *testing.T) {
	const link = "https://device.sso.us-east-1.amazonaws.com/?user_code=ABCD-EFGH"
	for _, tc := range []struct {
		name    string
		status  int
		body    map[string]any
		want    []string
		wantErr string
	}{
		{"waiting", http.StatusOK, map[string]any{"state": "waiting", "verification_url": link, "user_code": "ABCD-EFGH"},
			[]string{"waiting", "page: " + link, "code: ABCD-EFGH"}, ""},
		{"not waiting", http.StatusOK, map[string]any{"state": "not_waiting"}, []string{"not waiting"}, ""},
		{"owner only", http.StatusForbidden, map[string]any{"error": "only the person who started this run can open it interactively", "reason": "run_owner_only"},
			nil, "run_owner_only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := outputServer(t, tc.status, tc.body)
			root := rootCmd()
			root.SetArgs([]string{"run", "sign-in", uuid.NewString(), "--url", srv.URL, "--token", "tok"})
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&bytes.Buffer{})
			err := root.Execute()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one naming %s", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("stdout = %q, want %q", out.String(), w)
				}
			}
		})
	}
}
