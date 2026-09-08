// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/test/awsssofake"
)

// TestRun_NeverInvokesAWSCLIForAccountRoleLookup covers F160: run() used to
// best-effort shell out to `aws sso list-accounts`/`list-account-roles
// --access-token <token>`, putting the live SSO access token on that child
// process's own argv — readable by any /proc reader in the sandbox sharing
// its PID namespace, not just same-uid. A stub `aws` on PATH stands in for
// the real CLI: if run() ever execs it, the token would have been on its
// command line (see the finding's own repro, which greps /proc/self/cmdline).
//
// It also pins the fix's completeness: resolveAccountRole was rewritten to
// call the SSO portal over HTTP (Bearer header, never argv) rather than
// dropped outright, so a normal capture still uploads non-blank
// account_id/role_name — a blank pair is exactly the shape
// internal/api/ssotoken_test.go's TestUploadSSOToken_HalfResolvedCaptureRejected
// pins the control plane 400ing on, so a capture that could not actually be
// stored must never again be this test's own "success".
//
// Red-first: pre-fix, run() calls resolveAccountRole -> runAWSJSON, which
// execs the stub (present on PATH) with --access-token <token>; the marker
// file it touches on invocation exists afterward, so the assertion fails.
func TestRun_NeverInvokesAWSCLIForAccountRoleLookup(t *testing.T) {
	home := t.TempDir()
	cacheDir := filepath.Join(home, ssoCacheSubdir)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Repo's own fake SSO portal (test/awsssofake) rather than an ad-hoc
	// stub: it enforces the REAL x-amz-sso_bearer_token contract
	// (checkBearer, per botocore's sso/2019-06-10/service-2.json), so a
	// build that authenticates with the wrong header 401s here exactly as
	// it would against the real portal.sso.<region>.amazonaws.com.
	portal := awsssofake.New()
	t.Cleanup(portal.Close)
	token := portal.AccessToken()
	cache := `{"accessToken":"` + token + `","startUrl":"https://x.awsapps.com/start","region":"us-east-1","expiresAt":"2100-01-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(cacheDir, "abc123.json"), []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "aws-cli-invoked")
	stub := "#!/bin/sh\ntouch '" + marker + "'\necho '{\"accountList\":[],\"roleList\":[]}'\n"
	if err := os.WriteFile(filepath.Join(binDir, "aws"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	prevBase := ssoPortalBase
	ssoPortalBase = func(string) string { return portal.URL() }
	t.Cleanup(func() { ssoPortalBase = prevBase })

	var uploaded []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploaded, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	t.Setenv("HOME", home)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("WARDYN_PROXY_URL", srv.URL)
	t.Setenv("WARDYN_RUN_ID", "run-1")

	if err := run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the aws CLI stub was invoked — the live SSO access token would have been placed on its argv (F160)")
	}
	if uploaded == nil {
		t.Fatal("expected an sso-token upload to have happened")
	}
	var got struct {
		AccountID string `json:"account_id"`
		RoleName  string `json:"role_name"`
	}
	if err := json.Unmarshal(uploaded, &got); err != nil {
		t.Fatalf("decode uploaded body: %v", err)
	}
	// This is exactly the predicate internal/api/harnesscred.go's
	// awsSSOBlob.valid applies server-side (AccountID != "" && RoleName != "");
	// a capture with either blank is a shape the control plane 400s (see
	// ssotoken_test.go's TestUploadSSOToken_HalfResolvedCaptureRejected), so
	// asserting it here means this test cannot pass on a build that dropped
	// resolution instead of reworking it to avoid argv.
	if got.AccountID == "" || got.RoleName == "" {
		t.Fatalf("uploaded body has blank account_id/role_name (%+v) — the control plane's awsSSOBlob.valid rejects exactly this shape with 400", got)
	}
	fixture := portal.Account()
	if got.AccountID != fixture.AccountID || got.RoleName != fixture.RoleName {
		t.Errorf("uploaded account_id/role_name = %q/%q, want the fake portal's fixture %q/%q", got.AccountID, got.RoleName, fixture.AccountID, fixture.RoleName)
	}
}

// TestResolveAccountRole_LeavesBlankOnPortalFailure covers the non-fatal
// residual: any portal failure (network, non-2xx, decode, empty list) must
// leave the caller free to upload a blank account/role rather than erroring
// out of run() entirely — a resolution failure must never turn into an
// upload failure.
func TestResolveAccountRole_LeavesBlankOnPortalFailure(t *testing.T) {
	prevBase := ssoPortalBase
	ssoPortalBase = func(string) string { return "http://127.0.0.1:1" } // nothing listening
	t.Cleanup(func() { ssoPortalBase = prevBase })

	accountID, roleName, ok := resolveAccountRole("tok", "us-east-1")
	if ok {
		t.Fatalf("expected ok=false on an unreachable portal, got accountID=%q roleName=%q", accountID, roleName)
	}
}

func TestIsSSOToken(t *testing.T) {
	tokenFile := ssoCacheFile{AccessToken: "tok", StartURL: "https://x.awsapps.com/start", Region: "us-west-2"}
	if !tokenFile.isSSOToken() {
		t.Error("a file with accessToken+startUrl/region must be an SSO token file")
	}
	roleFile := ssoCacheFile{AccessKeyID: "AKIA...", Region: "us-west-2"}
	if roleFile.isSSOToken() {
		t.Error("a role-credential cache file (accessKeyId, no accessToken) must NOT be treated as an SSO token file")
	}
	if (ssoCacheFile{}).isSSOToken() {
		t.Error("an empty file must not be treated as an SSO token file")
	}
}

func TestParseSSOTime(t *testing.T) {
	// The AWS CLI's actual format: a literal "UTC" suffix.
	got, err := parseSSOTime("2021-05-14T18:59:22UTC")
	if err != nil {
		t.Fatalf("parseSSOTime: %v", err)
	}
	want := time.Date(2021, 5, 14, 18, 59, 22, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
	// Forward-compat: plain RFC3339 also parses.
	if _, err := parseSSOTime("2100-01-01T00:00:00Z"); err != nil {
		t.Errorf("RFC3339 fallback: %v", err)
	}
	if _, err := parseSSOTime("not-a-time"); err == nil {
		t.Error("garbage timestamp must error")
	}
}

func TestNewestSSOToken(t *testing.T) {
	dir := t.TempDir()

	// A role-credential cache file — must be skipped even though it's newest.
	write(t, dir, "role.json", `{"accessKeyId":"AKIA...","secretAccessKey":"x","sessionToken":"y"}`)
	// An older SSO token file.
	oldPath := write(t, dir, "old-token.json", `{"accessToken":"old-tok","startUrl":"https://old.awsapps.com/start","region":"us-east-1","expiresAt":"2100-01-01T00:00:00Z"}`)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(oldPath, old, old); err != nil {
		t.Fatal(err)
	}
	// The newest SSO token file — this is the one that must be selected.
	write(t, dir, "new-token.json", `{"accessToken":"new-tok","startUrl":"https://new.awsapps.com/start","region":"us-west-2","expiresAt":"2100-01-01T00:00:00Z"}`)
	// Garbage that isn't even valid JSON — must be skipped, not fatal.
	write(t, dir, "garbage.json", `not json`)

	got, err := newestSSOToken(dir)
	if err != nil {
		t.Fatalf("newestSSOToken: %v", err)
	}
	if got.AccessToken != "new-tok" {
		t.Errorf("selected %q, want the newest SSO token file (new-tok)", got.AccessToken)
	}
}

func TestNewestSSOToken_NoneFound(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "role.json", `{"accessKeyId":"AKIA..."}`)
	if _, err := newestSSOToken(dir); err == nil {
		t.Error("expected an error when no SSO token cache file is present")
	}
}

// TestParseRealAWSCLICacheFile validates this package's OWN parser
// (newestSSOToken + toBlob, including parseSSOTime) against a cache file the
// REAL AWS CLI v2 wrote — not a hand-written fixture — closing risk #1 from
// the aws-sso-fake work: a wrong cache-filename/shape assumption here is a
// SILENT failure (botocore just says "not logged in"), so this must run
// against the actual CLI's output, not our guess at its shape.
//
// Requires Docker (test/awsssofake.SkipUnlessDocker); skips cleanly without
// it so `go test ./...` stays green on a daemon-less machine.
func TestParseRealAWSCLICacheFile(t *testing.T) {
	awsssofake.SkipUnlessDocker(t)

	s := awsssofake.New()
	defer s.Close()

	result := awsssofake.RunDeviceCodeLogin(t, s, "wardyn", "wardyn", "https://fake.awsapps.com/start", "us-east-1")

	cache, err := newestSSOToken(result.CacheDir)
	if err != nil {
		t.Fatalf("newestSSOToken on the REAL cache dir: %v", err)
	}
	if !cache.isSSOToken() {
		t.Fatalf("real cache file %+v not recognized as an SSO token file", cache)
	}

	blob, err := toBlob(cache)
	if err != nil {
		t.Fatalf("toBlob on the REAL cache file: %v", err)
	}
	if blob.AccessToken != s.AccessToken() {
		t.Errorf("parsed AccessToken = %q, want the fake's issued token %q", blob.AccessToken, s.AccessToken())
	}
	if blob.StartURL != "https://fake.awsapps.com/start" {
		t.Errorf("parsed StartURL = %q, want the configured start URL", blob.StartURL)
	}
	if blob.Region != "us-east-1" {
		t.Errorf("parsed Region = %q, want us-east-1", blob.Region)
	}
	if blob.RefreshToken == "" {
		t.Error("parsed RefreshToken is empty; the real device-code CreateToken response includes one")
	}
	if blob.ClientID == "" || blob.ClientSecret == "" {
		t.Error("parsed ClientID/ClientSecret is empty; the real cache file carries the registered client")
	}
	if blob.ExpiresAt.IsZero() || !blob.ExpiresAt.After(time.Now()) {
		t.Errorf("parsed ExpiresAt = %v, want a real future timestamp (validates parseSSOTime against the CLI's ACTUAL format, whatever it turns out to be)", blob.ExpiresAt)
	}
	if blob.RegistrationExpiresAt.IsZero() {
		t.Error("parsed RegistrationExpiresAt is empty; the real cache file carries client-registration expiry")
	}
}

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSuccessMarker_UIParity pins the PTY handshake between this helper and the
// setup UI's login pane: the pane ends the login only when it sees doneMarker in
// the attach stream, so a one-character drift here leaves the operator watching
// a spinner forever with a credential already captured. Both sides are read from
// source (Go and TypeScript cannot share the constant).
func TestSuccessMarker_UIParity(t *testing.T) {
	uiPath := filepath.Join("..", "..", "ui", "src", "app", "components", "screens", "settings", "harness-login-pane.tsx")
	ts, err := os.ReadFile(uiPath)
	if err != nil {
		t.Fatalf("read %s: %v", uiPath, err)
	}
	m := regexp.MustCompile(`doneMarker:\s*"([^"]+)"`).FindStringSubmatch(string(ts))
	if m == nil {
		t.Fatalf("no doneMarker in %s", uiPath)
	}
	if m[1] != successMarker {
		t.Errorf("marker drift: UI doneMarker %q != wardyn-aws-sso successMarker %q", m[1], successMarker)
	}
}
