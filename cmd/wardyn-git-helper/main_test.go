// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeProxy is a lightweight httptest server that mocks the proxy's local
// routes under /wardyn/v1/.
type fakeProxy struct {
	t   *testing.T
	mux *http.ServeMux
	srv *httptest.Server
}

func newFakeProxy(t *testing.T, mux *http.ServeMux) *fakeProxy {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &fakeProxy{t: t, mux: mux, srv: srv}
}

func (f *fakeProxy) URL() string { return f.srv.URL }

// -- helpers ------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// -- tests --------------------------------------------------------------------

// TestGetSuccess verifies that a 200 mint response produces the expected
// git credential lines on stdout.
func TestGetSuccess(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/wardyn/v1/credentials/mint", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "want POST", http.StatusMethodNotAllowed)
			return
		}
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["grant_id"] != "test-grant-1" {
			http.Error(w, "wrong grant_id", http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, mintResponse{
			Kind:      "github_token",
			Token:     "ghs_faketoken",
			JTI:       "jti-1",
			ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})

	fp := newFakeProxy(t, mux)

	t.Setenv("WARDYN_PROXY_URL", fp.URL())
	t.Setenv("WARDYN_GITHUB_GRANT_ID", "test-grant-1")

	stdin := strings.NewReader("protocol=https\nhost=github.com\n\n")
	var stdout, stderr bytes.Buffer

	if err := run("get", "", stdin, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v", err)
	}

	out := stdout.String()
	assertContains(t, out, "username=x-access-token")
	assertContains(t, out, "password=ghs_faketoken")
	assertContains(t, out, "protocol=https")
	assertContains(t, out, "host=github.com")
}

// TestGetApprovalThenSuccess verifies the 409-pending -> poll -> APPROVED ->
// re-mint flow.
func TestGetApprovalThenSuccess(t *testing.T) {
	approvalID := "approval-uuid-42"
	var mintCalls atomic.Int32
	var pollCalls atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/wardyn/v1/credentials/mint", func(w http.ResponseWriter, r *http.Request) {
		n := int(mintCalls.Add(1))
		if n == 1 {
			// First call: return 409 pending.
			writeJSON(w, http.StatusConflict, pendingResponse{ApprovalID: approvalID})
			return
		}
		// Second call (after approval): return the token.
		writeJSON(w, http.StatusOK, mintResponse{
			Kind:      "github_token",
			Token:     "ghs_approved_token",
			JTI:       "jti-2",
			ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	mux.HandleFunc("/wardyn/v1/approvals/"+approvalID, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "want GET", http.StatusMethodNotAllowed)
			return
		}
		n := int(pollCalls.Add(1))
		if n < 2 {
			// First poll: still pending.
			writeJSON(w, http.StatusOK, approvalResponse{ID: approvalID, State: "PENDING"})
			return
		}
		// Second poll: approved.
		writeJSON(w, http.StatusOK, approvalResponse{ID: approvalID, State: "APPROVED"})
	})

	fp := newFakeProxy(t, mux)

	t.Setenv("WARDYN_PROXY_URL", fp.URL())
	t.Setenv("WARDYN_GITHUB_GRANT_ID", "grant-999")
	// Short approval timeout so the test doesn't hang.
	t.Setenv("WARDYN_APPROVAL_TIMEOUT", "30s")

	stdin := strings.NewReader("protocol=https\nhost=github.com\n\n")
	var stdout, stderr bytes.Buffer

	if err := run("get", "", stdin, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v", err)
	}

	out := stdout.String()
	assertContains(t, out, "password=ghs_approved_token")
	if mintCalls.Load() < 2 {
		t.Errorf("expected at least 2 mint calls, got %d", mintCalls.Load())
	}
}

// TestGetDenied verifies that a 409 with "denied:true" propagates as an error.
func TestGetDenied(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/wardyn/v1/credentials/mint", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusConflict, pendingResponse{
			ApprovalID: "denial-uuid",
			Denied:     true,
			Reason:     "operator denied the request",
		})
	})

	fp := newFakeProxy(t, mux)
	t.Setenv("WARDYN_PROXY_URL", fp.URL())
	t.Setenv("WARDYN_GITHUB_GRANT_ID", "grant-denied")

	stdin := strings.NewReader("protocol=https\nhost=github.com\n\n")
	var stdout, stderr bytes.Buffer

	err := run("get", "", stdin, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for denied grant, got nil")
	}
	if !strings.Contains(err.Error(), "denied") {
		t.Errorf("error should mention denial, got: %v", err)
	}
	if stdout.Len() > 0 {
		t.Errorf("stdout should be empty on denial, got: %q", stdout.String())
	}
}

// TestGetApprovalTimeout verifies that when an approval stays PENDING until the
// deadline, the helper returns a clear timeout error.
func TestGetApprovalTimeout(t *testing.T) {
	approvalID := "approval-timeout-uuid"

	mux := http.NewServeMux()
	mux.HandleFunc("/wardyn/v1/credentials/mint", func(w http.ResponseWriter, r *http.Request) {
		// Always return 409 pending.
		writeJSON(w, http.StatusConflict, pendingResponse{ApprovalID: approvalID})
	})
	mux.HandleFunc("/wardyn/v1/approvals/"+approvalID, func(w http.ResponseWriter, r *http.Request) {
		// Always pending.
		writeJSON(w, http.StatusOK, approvalResponse{ID: approvalID, State: "PENDING"})
	})

	fp := newFakeProxy(t, mux)
	t.Setenv("WARDYN_PROXY_URL", fp.URL())
	t.Setenv("WARDYN_GITHUB_GRANT_ID", "grant-timeout")
	// Very short timeout so the test is fast.
	t.Setenv("WARDYN_APPROVAL_TIMEOUT", "1s")

	stdin := strings.NewReader("protocol=https\nhost=github.com\n\n")
	var stdout, stderr bytes.Buffer

	// Override the poll interval via a small approval timeout to make test fast.
	// The test relies on the helper's 1s timeout expiring before the 3s poll.
	start := time.Now()
	err := run("get", "", stdin, &stdout, &stderr)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") && !strings.Contains(err.Error(), "pending") {
		t.Errorf("expected timeout message, got: %v", err)
	}
	// Should complete well within 10s.
	if elapsed > 10*time.Second {
		t.Errorf("test took too long: %v", elapsed)
	}
}

// TestGetNonGitHubHost verifies that non-GitHub hosts produce no output
// (git falls through to the next helper).
func TestGetNonGitHubHost(t *testing.T) {
	// No fake proxy needed — the helper should return before making any request.
	t.Setenv("WARDYN_PROXY_URL", "http://localhost:1")     // unreachable
	t.Setenv("WARDYN_GITHUB_GRANT_ID", "irrelevant-grant") // set but shouldn't matter

	stdin := strings.NewReader("protocol=https\nhost=gitlab.com\n\n")
	var stdout, stderr bytes.Buffer

	if err := run("get", "", stdin, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v", err)
	}
	if stdout.Len() > 0 {
		t.Errorf("expected empty stdout for non-GitHub host, got: %q", stdout.String())
	}
}

// TestGetMissingGrantID verifies that a missing WARDYN_GITHUB_GRANT_ID causes
// the helper to exit 0 with no output (fail open — git falls through).
func TestGetMissingGrantID(t *testing.T) {
	t.Setenv("WARDYN_PROXY_URL", "http://localhost:1") // unreachable
	t.Setenv("WARDYN_GITHUB_GRANT_ID", "")             // explicitly unset

	stdin := strings.NewReader("protocol=https\nhost=github.com\n\n")
	var stdout, stderr bytes.Buffer

	if err := run("get", "", stdin, &stdout, &stderr); err != nil {
		t.Fatalf("expected no error for missing grant id, got: %v", err)
	}
	if stdout.Len() > 0 {
		t.Errorf("expected empty stdout for missing grant id, got: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "WARDYN_GITHUB_GRANT_ID") {
		t.Errorf("expected helpful stderr message about missing grant id, got: %q", stderr.String())
	}
}

// TestStoreAndEraseAreNoOps verifies that store and erase consume input silently.
func TestStoreAndEraseAreNoOps(t *testing.T) {
	for _, cmd := range []string{"store", "erase"} {
		t.Run(cmd, func(t *testing.T) {
			stdin := strings.NewReader("protocol=https\nhost=github.com\nusername=x-access-token\npassword=secret\n\n")
			var stdout, stderr bytes.Buffer

			if err := run(cmd, "", stdin, &stdout, &stderr); err != nil {
				t.Fatalf("run %s: %v", cmd, err)
			}
			if stdout.Len() > 0 {
				t.Errorf("%s should produce no stdout, got: %q", cmd, stdout.String())
			}
		})
	}
}

// TestGitHubSubdomain verifies that *.github.com is also handled.
func TestGitHubSubdomain(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/wardyn/v1/credentials/mint", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, mintResponse{
			Kind:  "github_token",
			Token: "ghs_subdomain_token",
			JTI:   "jti-sub",
		})
	})

	fp := newFakeProxy(t, mux)
	t.Setenv("WARDYN_PROXY_URL", fp.URL())
	t.Setenv("WARDYN_GITHUB_GRANT_ID", "grant-sub")

	stdin := strings.NewReader("protocol=https\nhost=api.github.com\n\n")
	var stdout, stderr bytes.Buffer

	if err := run("get", "", stdin, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v", err)
	}
	assertContains(t, stdout.String(), "password=ghs_subdomain_token")
}

// TestMint422RequiresSPIRE verifies that a 422 response is surfaced as an error.
func TestMint422RequiresSPIRE(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/wardyn/v1/credentials/mint", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "requires_spire"})
	})

	fp := newFakeProxy(t, mux)
	t.Setenv("WARDYN_PROXY_URL", fp.URL())
	t.Setenv("WARDYN_GITHUB_GRANT_ID", "grant-spire")

	stdin := strings.NewReader("protocol=https\nhost=github.com\n\n")
	var stdout, stderr bytes.Buffer

	err := run("get", "", stdin, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for 422, got nil")
	}
	if !strings.Contains(err.Error(), "SPIRE") {
		t.Errorf("error should mention SPIRE, got: %v", err)
	}
}

// TestMint401Unauthorized verifies that a 401 response is surfaced as an error.
func TestMint401Unauthorized(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/wardyn/v1/credentials/mint", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})

	fp := newFakeProxy(t, mux)
	t.Setenv("WARDYN_PROXY_URL", fp.URL())
	t.Setenv("WARDYN_GITHUB_GRANT_ID", "grant-unauth")

	stdin := strings.NewReader("protocol=https\nhost=github.com\n\n")
	var stdout, stderr bytes.Buffer

	err := run("get", "", stdin, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for 401, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention 401, got: %v", err)
	}
}

// TestApprovalDeniedViaPolling verifies denial detected during polling.
func TestApprovalDeniedViaPolling(t *testing.T) {
	approvalID := "poll-denied-uuid"
	var mintCalls atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/wardyn/v1/credentials/mint", func(w http.ResponseWriter, r *http.Request) {
		mintCalls.Add(1)
		// Always return 409 pending.
		writeJSON(w, http.StatusConflict, pendingResponse{ApprovalID: approvalID})
	})
	mux.HandleFunc("/wardyn/v1/approvals/"+approvalID, func(w http.ResponseWriter, r *http.Request) {
		// Return DENIED on first poll.
		writeJSON(w, http.StatusOK, approvalResponse{ID: approvalID, State: "DENIED"})
	})

	fp := newFakeProxy(t, mux)
	t.Setenv("WARDYN_PROXY_URL", fp.URL())
	t.Setenv("WARDYN_GITHUB_GRANT_ID", "grant-polldeny")
	t.Setenv("WARDYN_APPROVAL_TIMEOUT", "30s")

	stdin := strings.NewReader("protocol=https\nhost=github.com\n\n")
	var stdout, stderr bytes.Buffer

	err := run("get", "", stdin, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for denied approval, got nil")
	}
	if !strings.Contains(err.Error(), "denied") {
		t.Errorf("expected denial message, got: %v", err)
	}
	if stdout.Len() > 0 {
		t.Errorf("stdout should be empty on denial, got: %q", stdout.String())
	}
}

// TestParseInput verifies the stdin parser.
func TestParseInput(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantHost string
		wantProt string
	}{
		{
			name:     "basic",
			input:    "protocol=https\nhost=github.com\n\n",
			wantHost: "github.com",
			wantProt: "https",
		},
		{
			name:     "host with port",
			input:    "protocol=https\nhost=github.com:443\n\n",
			wantHost: "github.com",
			wantProt: "https",
		},
		{
			name:     "empty",
			input:    "\n",
			wantHost: "",
			wantProt: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ci := parseInput(strings.NewReader(c.input))
			if ci.Host != c.wantHost {
				t.Errorf("host: got %q want %q", ci.Host, c.wantHost)
			}
			if ci.Protocol != c.wantProt {
				t.Errorf("protocol: got %q want %q", ci.Protocol, c.wantProt)
			}
		})
	}
}

// TestIsGitHubHost tests the host matching logic.
func TestIsGitHubHost(t *testing.T) {
	yes := []string{"github.com", "GITHUB.COM", "api.github.com", "raw.github.com", "github.com."}
	no := []string{"gitlab.com", "bitbucket.org", "github.org", "notgithub.com", ""}
	for _, h := range yes {
		if !isGitHubHost(h) {
			t.Errorf("isGitHubHost(%q) = false, want true", h)
		}
	}
	for _, h := range no {
		if isGitHubHost(h) {
			t.Errorf("isGitHubHost(%q) = true, want false", h)
		}
	}
}

// -- caller authentication ----------------------------------------------------

// writeTempSecret writes value to a 0400 file in a per-test temp dir and returns
// its path. It mirrors the agent-run provisioning of the per-run secret file.
func writeTempSecret(t *testing.T, value string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "git-helper.secret")
	if err := os.WriteFile(p, []byte(value), 0o400); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	return p
}

// authMintMux returns a mux whose mint route flips minted=true and returns a
// token, so a test can assert whether the helper attempted a mint at all.
func authMintMux(minted *atomic.Bool, token string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/wardyn/v1/credentials/mint", func(w http.ResponseWriter, r *http.Request) {
		minted.Store(true)
		writeJSON(w, http.StatusOK, mintResponse{Kind: "github_token", Token: token})
	})
	return mux
}

// TestAuthCorrectSecretMints verifies that when a secret file is provisioned and
// the caller presents the matching WARDYN_GIT_HELPER_SECRET, the token is minted
// and emitted as normal.
func TestAuthCorrectSecretMints(t *testing.T) {
	const secret = "per-run-secret-correct"
	var minted atomic.Bool
	fp := newFakeProxy(t, authMintMux(&minted, "ghs_auth_ok"))

	t.Setenv("WARDYN_PROXY_URL", fp.URL())
	t.Setenv("WARDYN_GITHUB_GRANT_ID", "grant-auth")
	t.Setenv("WARDYN_GIT_HELPER_SECRET", secret)
	sf := writeTempSecret(t, secret)

	stdin := strings.NewReader("protocol=https\nhost=github.com\n\n")
	var stdout, stderr bytes.Buffer
	if err := run("get", sf, stdin, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v", err)
	}
	assertContains(t, stdout.String(), "password=ghs_auth_ok")
	if !minted.Load() {
		t.Errorf("expected mint to be attempted with a correct secret")
	}
}

// TestAuthWrongSecretFailsClosed verifies that a wrong presented secret yields NO
// token, does NOT call mint, and does NOT error git (graceful fall-through).
func TestAuthWrongSecretFailsClosed(t *testing.T) {
	var minted atomic.Bool
	fp := newFakeProxy(t, authMintMux(&minted, "ghs_should_not_mint"))

	t.Setenv("WARDYN_PROXY_URL", fp.URL())
	t.Setenv("WARDYN_GITHUB_GRANT_ID", "grant-auth")
	t.Setenv("WARDYN_GIT_HELPER_SECRET", "the-wrong-secret")
	sf := writeTempSecret(t, "the-real-secret")

	stdin := strings.NewReader("protocol=https\nhost=github.com\n\n")
	var stdout, stderr bytes.Buffer
	if err := run("get", sf, stdin, &stdout, &stderr); err != nil {
		t.Fatalf("auth failure must not error git, got: %v", err)
	}
	if strings.Contains(stdout.String(), "password=") {
		t.Errorf("wrong secret must yield no token, got stdout: %q", stdout.String())
	}
	if minted.Load() {
		t.Errorf("helper must not call mint when caller auth fails")
	}
}

// TestAuthMissingPresentedSecretFailsClosed verifies that a provisioned secret
// file with NO presented WARDYN_GIT_HELPER_SECRET yields no token and no error.
func TestAuthMissingPresentedSecretFailsClosed(t *testing.T) {
	var minted atomic.Bool
	fp := newFakeProxy(t, authMintMux(&minted, "ghs_should_not_mint"))

	t.Setenv("WARDYN_PROXY_URL", fp.URL())
	t.Setenv("WARDYN_GITHUB_GRANT_ID", "grant-auth")
	t.Setenv("WARDYN_GIT_HELPER_SECRET", "") // caller presents nothing
	sf := writeTempSecret(t, "the-real-secret")

	stdin := strings.NewReader("protocol=https\nhost=github.com\n\n")
	var stdout, stderr bytes.Buffer
	if err := run("get", sf, stdin, &stdout, &stderr); err != nil {
		t.Fatalf("missing presented secret must not error git, got: %v", err)
	}
	if stdout.Len() > 0 {
		t.Errorf("missing presented secret must yield no token, got: %q", stdout.String())
	}
	if minted.Load() {
		t.Errorf("helper must not call mint when no secret is presented")
	}
}

// TestAuthSecretFileAbsentFailsOpen verifies the legacy/interactive path: when a
// --secret-file path is configured but the file does not exist (no gate
// provisioned), the helper falls OPEN and mints, so legitimate git is not
// blocked. This is the interactive-run / un-wired-deployment case.
func TestAuthSecretFileAbsentFailsOpen(t *testing.T) {
	var minted atomic.Bool
	fp := newFakeProxy(t, authMintMux(&minted, "ghs_legacy_ok"))

	t.Setenv("WARDYN_PROXY_URL", fp.URL())
	t.Setenv("WARDYN_GITHUB_GRANT_ID", "grant-auth")
	// No WARDYN_GIT_HELPER_SECRET and a path that does not exist.
	absent := filepath.Join(t.TempDir(), "does-not-exist.secret")

	stdin := strings.NewReader("protocol=https\nhost=github.com\n\n")
	var stdout, stderr bytes.Buffer
	if err := run("get", absent, stdin, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v", err)
	}
	assertContains(t, stdout.String(), "password=ghs_legacy_ok")
	if !minted.Load() {
		t.Errorf("expected legacy fail-open mint when no secret file is provisioned")
	}
}

// TestParseArgs verifies that git's "<flags> <op>" argv is split into the
// --secret-file path and the operation, in both the gated and legacy forms.
func TestParseArgs(t *testing.T) {
	cases := []struct {
		name           string
		argv           []string
		wantSecretFile string
		wantOp         string
	}{
		{"flag then op", []string{"--secret-file", "/p/s", "get"}, "/p/s", "get"},
		{"flag equals form", []string{"--secret-file=/p/s", "store"}, "/p/s", "store"},
		{"legacy op only", []string{"get"}, "", "get"},
		{"no op", []string{"--secret-file", "/p/s"}, "/p/s", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sf, op, err := parseArgs(c.argv)
			if err != nil {
				t.Fatalf("parseArgs: %v", err)
			}
			if sf != c.wantSecretFile {
				t.Errorf("secretFile: got %q want %q", sf, c.wantSecretFile)
			}
			if op != c.wantOp {
				t.Errorf("op: got %q want %q", op, c.wantOp)
			}
		})
	}
}

// -- git_pat (non-GitHub host) routing -----------------------------------------

// TestGetPATHostResolvesAndEmits verifies a host present in WARDYN_GIT_PAT_GRANTS
// mints via the broker and emits the mint response's username + the PAT as the
// password. github.com is NOT set here, proving the PAT path is independent.
func TestGetPATHostResolvesAndEmits(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/wardyn/v1/credentials/mint", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["grant_id"] != "pat-grant-1" {
			http.Error(w, "wrong grant_id", http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, mintResponse{
			Kind:     "git_pat",
			Token:    "azdo-pat-value-12345",
			Username: "pat",
			JTI:      "jti-pat",
		})
	})
	fp := newFakeProxy(t, mux)

	t.Setenv("WARDYN_PROXY_URL", fp.URL())
	t.Setenv("WARDYN_GITHUB_GRANT_ID", "") // no github grant — PAT path only
	t.Setenv("WARDYN_GIT_PAT_GRANTS", `{"dev.azure.com":"pat-grant-1"}`)

	stdin := strings.NewReader("protocol=https\nhost=dev.azure.com\n\n")
	var stdout, stderr bytes.Buffer
	if err := run("get", "", stdin, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := stdout.String()
	assertContains(t, out, "username=pat")
	assertContains(t, out, "password=azdo-pat-value-12345")
	assertContains(t, out, "host=dev.azure.com")
}

// TestGetPATUnmatchedHostEmitsNothing verifies a host that is neither github nor
// present in WARDYN_GIT_PAT_GRANTS emits nothing (git falls through) — the
// global-helper fall-through must never break unrelated git auth.
func TestGetPATUnmatchedHostEmitsNothing(t *testing.T) {
	t.Setenv("WARDYN_PROXY_URL", "http://localhost:1") // unreachable — must not be hit
	t.Setenv("WARDYN_GITHUB_GRANT_ID", "irrelevant")
	t.Setenv("WARDYN_GIT_PAT_GRANTS", `{"dev.azure.com":"pat-grant-1"}`)

	stdin := strings.NewReader("protocol=https\nhost=bitbucket.org\n\n")
	var stdout, stderr bytes.Buffer
	if err := run("get", "", stdin, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v", err)
	}
	if stdout.Len() > 0 {
		t.Errorf("unmatched host must emit nothing, got: %q", stdout.String())
	}
}

// TestGetPATCallerAuthFailsClosed verifies the caller-auth gate applies to the
// PAT path too: a wrong presented secret yields no credential and no error.
func TestGetPATCallerAuthFailsClosed(t *testing.T) {
	var minted atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/wardyn/v1/credentials/mint", func(w http.ResponseWriter, r *http.Request) {
		minted.Store(true)
		writeJSON(w, http.StatusOK, mintResponse{Kind: "git_pat", Token: "should-not-mint", Username: "oauth2"})
	})
	fp := newFakeProxy(t, mux)

	t.Setenv("WARDYN_PROXY_URL", fp.URL())
	t.Setenv("WARDYN_GIT_PAT_GRANTS", `{"gitlab.com":"pat-grant-gl"}`)
	t.Setenv("WARDYN_GIT_HELPER_SECRET", "the-wrong-secret")
	sf := writeTempSecret(t, "the-real-secret")

	stdin := strings.NewReader("protocol=https\nhost=gitlab.com\n\n")
	var stdout, stderr bytes.Buffer
	if err := run("get", sf, stdin, &stdout, &stderr); err != nil {
		t.Fatalf("caller-auth failure must not error git, got: %v", err)
	}
	if strings.Contains(stdout.String(), "password=") {
		t.Errorf("wrong secret must yield no credential, got stdout: %q", stdout.String())
	}
	if minted.Load() {
		t.Errorf("helper must not mint when caller auth fails")
	}
}

// TestGetGitHubFallsThroughToPATWhenNoAppGrant verifies that when
// WARDYN_GITHUB_GRANT_ID is unset but a git_pat grant for github.com exists in
// WARDYN_GIT_PAT_GRANTS, the helper mints the PAT instead of falling through to
// nothing — unblocking a plain GitHub PAT for operators who don't configure
// the GitHub App.
func TestGetGitHubFallsThroughToPATWhenNoAppGrant(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/wardyn/v1/credentials/mint", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["grant_id"] != "gh-pat-grant" {
			http.Error(w, "wrong grant_id", http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, mintResponse{
			Kind:     "git_pat",
			Token:    "ghp_plainpattoken",
			Username: "x-access-token",
			JTI:      "jti-gh-pat",
		})
	})
	fp := newFakeProxy(t, mux)

	t.Setenv("WARDYN_PROXY_URL", fp.URL())
	t.Setenv("WARDYN_GITHUB_GRANT_ID", "") // no App grant configured
	t.Setenv("WARDYN_GIT_PAT_GRANTS", `{"github.com":"gh-pat-grant"}`)

	stdin := strings.NewReader("protocol=https\nhost=github.com\n\n")
	var stdout, stderr bytes.Buffer
	if err := run("get", "", stdin, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := stdout.String()
	assertContains(t, out, "password=ghp_plainpattoken")
	assertContains(t, out, "username=x-access-token")
}

// TestGetGitHubAppGrantWinsOverPAT verifies the App-grant path is checked
// first and still wins when BOTH WARDYN_GITHUB_GRANT_ID and a github.com
// git_pat grant are configured — the fallthrough is additive, not a
// replacement.
func TestGetGitHubAppGrantWinsOverPAT(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/wardyn/v1/credentials/mint", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["grant_id"] != "gh-app-grant" {
			http.Error(w, "expected the App grant id, got wrong grant_id", http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, mintResponse{
			Kind:  "github_token",
			Token: "ghs_apptoken",
			JTI:   "jti-gh-app",
		})
	})
	fp := newFakeProxy(t, mux)

	t.Setenv("WARDYN_PROXY_URL", fp.URL())
	t.Setenv("WARDYN_GITHUB_GRANT_ID", "gh-app-grant")
	t.Setenv("WARDYN_GIT_PAT_GRANTS", `{"github.com":"gh-pat-grant-should-not-be-used"}`)

	stdin := strings.NewReader("protocol=https\nhost=github.com\n\n")
	var stdout, stderr bytes.Buffer
	if err := run("get", "", stdin, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := stdout.String()
	assertContains(t, out, "password=ghs_apptoken")
	assertContains(t, out, "username=x-access-token")
}

// assertContains is a test helper that fails if substr is not in s.
func assertContains(t *testing.T, s, substr string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Errorf("expected %q to contain %q", s, substr)
	}
}
