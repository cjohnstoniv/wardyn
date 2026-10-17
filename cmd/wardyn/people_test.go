// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func peopleFixture() types.PersonList {
	seen := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	gone := seen.Add(time.Hour)
	return types.PersonList{
		People: []types.PersonSummary{
			{Principal: "entra:t:o", Email: "pat@corp.example", IssuerKind: "entra", PreCreated: true, Role: "admin"},
			{Principal: "sub-2", Email: "sam@corp.example", IssuerKind: "oidc", Role: "user", LastSignedInAt: &seen,
				ActiveSessions: 1, APITokens: 2, SSHKeys: 3, Credentials: 4, ActiveRuns: 5},
			{Principal: "sub-3", IssuerKind: "oidc", DeactivatedAt: &gone, LastSignedInAt: &seen},
		},
		NextCursor: "c3ViLTM",
	}
}

func TestPeopleListCmd_PrintsTable(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, peopleFixture())
	root := rootCmd()
	out, errOut := &strings.Builder{}, &strings.Builder{}
	root.SetArgs([]string{"people", "list", "--limit", "3", "--q", "p", "--state", "active", "--url", srv.URL, "--token", "tok"})
	root.SetOut(out)
	root.SetErr(errOut)
	if err := root.Execute(); err != nil {
		t.Fatalf("people list: %v", err)
	}
	got := srv.last()
	if got.method != http.MethodGet || got.path != "/api/v1/people" || got.auth != "Bearer tok" {
		t.Errorf("got %s %s auth %q, want GET /api/v1/people with the bearer token", got.method, got.path, got.auth)
	}
	for _, want := range []string{"limit=3", "q=p", "state=active"} {
		if !strings.Contains(got.query, want) {
			t.Errorf("query %q lacks %s", got.query, want)
		}
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("table has %d lines, want a header and 3 rows:\n%s", len(lines), out)
	}
	for i, want := range [][]string{
		{"PRINCIPAL", "EMAIL", "ISSUER", "ROLE", "STATE", "LAST", "SIGN-IN", "SESSIONS", "TOKENS", "SSH", "KEYS", "CREDENTIALS", "RUNS"},
		{"entra:t:o", "pat@corp.example", "entra", "admin", "pre-created", "never", "0", "0", "0", "0", "0"},
		{"sub-2", "sam@corp.example", "oidc", "user", "active", "2026-10-01", "1", "2", "3", "4", "5"},
		{"sub-3", "oidc", "deactivated", "2026-10-01"},
	} {
		if fields := strings.Fields(lines[i]); !containsInOrder(fields, want) {
			t.Errorf("line %d = %q, want fields %v in order", i, lines[i], want)
		}
	}
	if !strings.Contains(errOut.String(), "--cursor c3ViLTM") {
		t.Errorf("stderr = %q, want the next-page cursor", errOut)
	}
}

func TestPeopleListCmd_JSON(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, types.PersonList{})
	root := rootCmd()
	out := &strings.Builder{}
	root.SetArgs([]string{"people", "list", "--json", "--cursor", "abc", "--url", srv.URL, "--token", "tok"})
	root.SetOut(out)
	root.SetErr(&strings.Builder{})
	if err := root.Execute(); err != nil {
		t.Fatalf("people list --json: %v", err)
	}
	if !strings.Contains(srv.last().query, "cursor=abc") {
		t.Errorf("query = %q, want the cursor sent", srv.last().query)
	}
	if !strings.Contains(out.String(), `"people": []`) {
		t.Errorf("output = %q, want an empty people array, never null", out)
	}
}

func TestPeopleListCmd_ServerRefusal(t *testing.T) {
	srv := newCmdServer(t, http.StatusForbidden, map[string]string{"error": "forbidden"})
	if err := execCmd(t, "people", "list", "--url", srv.URL, "--token", "tok"); err == nil {
		t.Fatal("people list against a 403 exited 0")
	}
}

// containsInOrder reports whether want appears in fields as a subsequence.
func containsInOrder(fields, want []string) bool {
	i := 0
	for _, f := range fields {
		if i < len(want) && f == want[i] {
			i++
		}
	}
	return i == len(want)
}
