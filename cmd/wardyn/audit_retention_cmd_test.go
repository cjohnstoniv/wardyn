// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// execCmdCapture is execCmd that also returns what the command wrote to its stdout.
func execCmdCapture(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := rootCmd()
	root.SetArgs(args)
	var out strings.Builder
	root.SetOut(&out)
	root.SetErr(&strings.Builder{})
	err := root.Execute()
	return out.String(), err
}

// `wardyn audit retention` shows the policy and each partition's eligibility; `retention drop` posts the
// partition and the digest it was given, and says why a refusal was refused.
func TestAuditRetentionCommands(t *testing.T) {
	var dropBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/audit/retention":
			_, _ = w.Write([]byte(`{"policy":{"days":0,"effective_days":0,"pending_days":90,"pending_effective_at":"2026-11-01T00:00:00Z"},` +
				`"cutover":"2026-10-01T00:00:00Z","months_ahead":12,"partitions":[` +
				`{"name":"audit_events_legacy","rows":5,"state":"closed","eligible":false,"refusal":"audit_retention_inside_window"}]}`))
		case "POST /api/v1/audit/retention/drop":
			_ = json.NewDecoder(r.Body).Decode(&dropBody)
			if dropBody["digest"] != "abc" {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"the digest does not match","reason":"audit_retention_digest_mismatch"}`))
				return
			}
			_, _ = w.Write([]byte(`{"partition":"audit_events_legacy","rows":5,"seq_lo":1,"seq_hi":5,"digest":"abc","event_seq":9}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	base := []string{"--url", srv.URL, "--token", "tok"}

	out, err := execCmdCapture(t, append([]string{"audit", "retention"}, base...)...)
	if err != nil {
		t.Fatalf("audit retention: %v", err)
	}
	for _, want := range []string{"retention: forever (a decrease to 90 days takes effect 2026-11-01", "audit_events_legacy", "audit_retention_inside_window"} {
		if !strings.Contains(out, want) {
			t.Errorf("audit retention output lacks %q:\n%s", want, out)
		}
	}

	if err := execCmd(t, append([]string{"audit", "retention", "drop", "audit_events_legacy"}, base...)...); err == nil ||
		!strings.Contains(err.Error(), "--digest is required") {
		t.Errorf("drop without a digest: %v, want it refused locally", err)
	}
	if err := execCmd(t, append([]string{"audit", "retention", "drop", "audit_events_legacy", "--digest", "bad"}, base...)...); err == nil ||
		!strings.Contains(err.Error(), "the digest does not match") {
		t.Errorf("drop with a wrong digest: %v, want the server's refusal", err)
	}
	out, err = execCmdCapture(t, append([]string{"audit", "retention", "drop", "audit_events_legacy", "--digest", "abc"}, base...)...)
	if err != nil {
		t.Fatalf("audit retention drop: %v", err)
	}
	if dropBody["partition"] != "audit_events_legacy" || dropBody["digest"] != "abc" || !strings.Contains(out, "dropped audit_events_legacy: 5 rows") {
		t.Errorf("drop sent %v and printed %q", dropBody, out)
	}
}
