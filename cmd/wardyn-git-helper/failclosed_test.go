// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestAuthUnreadableSecretFileFailsClosed: a caller-auth secret file that is
// present but unreadable (mode 0000) means the gate is provisioned and cannot
// be checked, so the helper refuses — no mint, no token — even for a caller
// presenting the right secret. Treating it as "not provisioned" would open the
// gate to any caller.
func TestAuthUnreadableSecretFileFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file; the unreadable arm cannot be reached")
	}
	const secret = "per-run-secret"
	sf := writeTempSecret(t, secret)
	if err := os.Chmod(sf, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(sf, 0o600) })

	if _, provisioned, err := loadExpectedSecret(sf); !provisioned || err == nil {
		t.Fatalf("loadExpectedSecret(0000 file) = provisioned %v, err %v; want provisioned with an error", provisioned, err)
	}

	var minted atomic.Bool
	fp := newFakeProxy(t, authMintMux(&minted, "ghs_should_not_mint"))
	t.Setenv("WARDYN_PROXY_URL", fp.URL())
	t.Setenv("WARDYN_GITHUB_GRANT_ID", "")
	t.Setenv("WARDYN_GIT_PAT_GRANTS", `{"github.com":"grant-auth"}`)
	t.Setenv("WARDYN_GIT_HELPER_SECRET", secret)

	ok, reason := authenticateCaller(sf)
	if ok || !strings.Contains(reason, "unreadable") {
		t.Fatalf("authenticateCaller(0000 file) = (%v, %q), want refused as unreadable", ok, reason)
	}

	var stdout, stderr bytes.Buffer
	if err := run("get", sf, strings.NewReader("protocol=https\nhost=github.com\n\n"), &stdout, &stderr); err != nil {
		t.Fatalf("a refused caller must not error git, got: %v", err)
	}
	if strings.Contains(stdout.String(), "password=") {
		t.Errorf("an unreadable secret file must yield no token, got stdout: %q", stdout.String())
	}
	if minted.Load() {
		t.Error("the helper minted although the provisioned secret could not be read")
	}
}

// TestAuthEmptySecretFileIsNotProvisioned pins the other documented arm: an
// empty (or whitespace-only) file means no gate is configured for this run.
func TestAuthEmptySecretFileIsNotProvisioned(t *testing.T) {
	for _, content := range []string{"", " \n\t\n"} {
		sf := writeTempSecret(t, content)
		secret, provisioned, err := loadExpectedSecret(sf)
		if provisioned || err != nil || secret != "" {
			t.Errorf("loadExpectedSecret(%q) = (%q, %v, %v), want not provisioned", content, secret, provisioned, err)
		}
		t.Setenv("WARDYN_GIT_HELPER_SECRET", "")
		if ok, reason := authenticateCaller(sf); !ok {
			t.Errorf("authenticateCaller(empty file %q) refused (%q); an empty file is no gate", content, reason)
		}
	}
}

// mintApprovalProxy answers the first mint with 409 pending on approvalID,
// every poll with pollState, and any later mint with remint (nil: a second 409
// pending, which carries no token).
func mintApprovalProxy(t *testing.T, approvalID, pollState string, remint any) *httptest.Server {
	t.Helper()
	var mints atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/wardyn/v1/credentials/mint", func(w http.ResponseWriter, r *http.Request) {
		if mints.Add(1) == 1 || remint == nil {
			writeJSON(w, http.StatusConflict, pendingResponse{Code: mintConflictPending, ApprovalID: approvalID})
			return
		}
		writeJSON(w, http.StatusOK, remint)
	})
	mux.HandleFunc("/wardyn/v1/approvals/"+approvalID, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, approvalResponse{ID: approvalID, State: pollState})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestMintWithApproval_TerminalArms: every terminal poll state ends the wait at
// the first poll with its own sentence, rather than falling into the default
// "keep polling" arm and blocking git until the timeout. CANCELLED is the one
// a killed run depends on. A re-mint that comes back without a token after
// APPROVED is refused, never emitted as an empty credential.
func TestMintWithApproval_TerminalArms(t *testing.T) {
	const approvalTimeout = 8 * time.Second // > pollInterval, < two polls past it
	for _, tc := range []struct {
		name, state string
		remint      any
		want        string
	}{
		{"denied", "DENIED", nil, "was denied by the operator"},
		{"expired", "EXPIRED", nil, "expired before a decision was made"},
		{"cancelled", "CANCELLED", nil, "was cancelled: the run ended before anyone decided it"},
		{"re-mint pending again", "APPROVED", nil, "re-mint after approval returned no token"},
		{"re-mint 200 without token", "APPROVED", mintResponse{Kind: "git_pat"}, "re-mint after approval: mint response missing token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := mintApprovalProxy(t, "ap-"+tc.state, tc.state, tc.remint)
			start := time.Now()
			tok, _, err := mintWithApproval(context.Background(), srv.Client(), srv.URL, "grant", approvalTimeout, io.Discard)
			if err == nil || tok != "" {
				t.Fatalf("mintWithApproval = (%q, %v), want no token and an error", tok, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %q, want it to contain %q", err, tc.want)
			}
			if elapsed := time.Since(start); elapsed > pollInterval+2*time.Second {
				t.Fatalf("a terminal %s took %s; it must end at the first poll, not wait out the timeout", tc.state, elapsed)
			}
		})
	}

	t.Run("409 without approval_id", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusConflict, pendingResponse{Code: mintConflictPending})
		}))
		t.Cleanup(srv.Close)
		tok, _, err := mintWithApproval(context.Background(), srv.Client(), srv.URL, "grant", approvalTimeout, io.Discard)
		if tok != "" || err == nil || !strings.Contains(err.Error(), "409 without approval_id") {
			t.Fatalf("mintWithApproval = (%q, %v), want the missing approval_id refusal", tok, err)
		}
	})
}
