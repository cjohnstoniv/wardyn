// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func runApprovalList(t *testing.T, url string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := rootCmd()
	out, errOut := &strings.Builder{}, &strings.Builder{}
	root.SetArgs(append([]string{"approval", "list"}, append(args, "--url", url, "--token", "tok")...))
	root.SetOut(out)
	root.SetErr(errOut)
	err = root.Execute()
	return out.String(), errOut.String(), err
}

func TestApprovalList_WaitingOnMe(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Wardyn-Truncated", "true")
		_ = json.NewEncoder(w).Encode([]types.ApprovalRequest{{
			ID: uuid.New(), RunID: uuid.New(), Kind: types.ApprovalEgressDomain, State: types.ApprovalPending,
			RequestedAt: time.Now().UTC(), RequestedBy: "sub-owner",
		}})
	}))
	defer srv.Close()
	out, errOut, err := runApprovalList(t, srv.URL, "--waiting-on-me")
	if err != nil {
		t.Fatalf("approval list --waiting-on-me: %v", err)
	}
	if gotQuery != "decidable_by=me" {
		t.Errorf("query = %q, want decidable_by=me and nothing else", gotQuery)
	}
	if !strings.Contains(out, "REQUESTED_BY") || !strings.Contains(out, "sub-owner") {
		t.Errorf("stdout = %q, want a REQUESTED_BY column with the creator", out)
	}
	if !strings.Contains(errOut, "truncated") {
		t.Errorf("stderr = %q, want a truncation warning", errOut)
	}
}

func TestApprovalList_WaitingOnMeRefusesFiltersTheServerIgnores(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer srv.Close()
	for _, flag := range [][]string{{"--state", "PENDING"}, {"--run", uuid.NewString()}, {"--limit", "5"}, {"--offset", "5"}} {
		if _, _, err := runApprovalList(t, srv.URL, append([]string{"--waiting-on-me"}, flag...)...); err == nil {
			t.Errorf("--waiting-on-me with %v: want a refusal", flag)
		}
	}
	if calls != 0 {
		t.Errorf("server was called %d times", calls)
	}
}
