// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
	if got.AccountID != fixture.AccountID || got.RoleName != fixture.Roles[0] {
		t.Errorf("uploaded account_id/role_name = %q/%q, want the fake portal's fixture %q/%q", got.AccountID, got.RoleName, fixture.AccountID, fixture.Roles[0])
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

	accountID, roleName, _, ok := pickAccountRole("tok", "us-east-1", ssoPin{})
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

// ── finding 1: the account/role a sign-in captures is chosen, never [0] ──────

// multiAccountPortal is the operator's reported shape: index 0 is an unrelated
// dev account whose roles cannot invoke the configured model, index 1 is the
// account WARDYN_BEDROCK_MODEL names. The whole of finding 1 is that a cloud
// team granting the first entitlement silently re-pointed the identity every
// run signs with — so in every test below, index 0 is the WRONG answer.
const (
	wrongAccount = "222222222222"
	rightAccount = "111111111111"
	rightRole    = "BedrockRunner"
)

func multiAccountPortal(t *testing.T) *awsssofake.Server {
	t.Helper()
	portal := awsssofake.New()
	t.Cleanup(portal.Close)
	portal.SetAccounts([]awsssofake.Account{
		{AccountID: wrongAccount, Roles: []string{"ReadOnly", "DevPower"}},
		{AccountID: rightAccount, Roles: []string{rightRole, "AdministratorAccess"}},
	})
	prevBase := ssoPortalBase
	ssoPortalBase = func(string) string { return portal.URL() }
	t.Cleanup(func() { ssoPortalBase = prevBase })
	return portal
}

// TestResolveAccountRole_PinnedAccountRoleWins: with a pin, the sign-in captures
// the PINNED pair even though the portal lists another account first. This is
// ask 1 — "a new entitlement cannot move it".
func TestResolveAccountRole_PinnedAccountRoleWins(t *testing.T) {
	portal := multiAccountPortal(t)
	acct, role, refusal, ok := pickAccountRole(portal.AccessToken(), "us-east-1",
		ssoPin{accountID: rightAccount, roleName: rightRole})
	if !ok || refusal != "" {
		t.Fatalf("pickAccountRole = (%q,%q,%q,%v), want the pinned pair accepted", acct, role, refusal, ok)
	}
	if acct != rightAccount || role != rightRole {
		t.Errorf("picked %q/%q, want the PINNED %q/%q — index 0 (%q) is the unrelated account the operator's cloud team granted", acct, role, rightAccount, rightRole, wrongAccount)
	}
}

// TestResolveAccountRole_PinnedAccountNotEntitled: a pin this session cannot
// reach is a REFUSAL, never a silent fall back to [0]. Falling back is the
// original defect wearing a pin.
func TestResolveAccountRole_PinnedAccountNotEntitled(t *testing.T) {
	portal := multiAccountPortal(t)
	acct, role, refusal, ok := pickAccountRole(portal.AccessToken(), "us-east-1",
		ssoPin{accountID: "999999999999", roleName: rightRole})
	if ok || acct != "" || role != "" {
		t.Fatalf("pickAccountRole = (%q,%q,%q,%v), want a refusal and NOTHING picked", acct, role, refusal, ok)
	}
	if !strings.Contains(refusal, "999999999999") {
		t.Errorf("refusal = %q, want it to name the pinned account the sign-in cannot reach", refusal)
	}
}

// TestResolveAccountRole_PinnedRoleNotInAccount: the role half is verified in
// the PINNED account, not in whatever account the portal listed first — which
// is only checkable because the fixture now scopes ListAccountRoles
// (TestListAccountRoles_ScopedToRequestedAccount).
func TestResolveAccountRole_PinnedRoleNotInAccount(t *testing.T) {
	portal := multiAccountPortal(t)
	// "DevPower" is a real role — in the WRONG account. Pinning it in the right
	// account must be refused, not accepted because some account has it.
	acct, role, refusal, ok := pickAccountRole(portal.AccessToken(), "us-east-1",
		ssoPin{accountID: rightAccount, roleName: "DevPower"})
	if ok || acct != "" || role != "" {
		t.Fatalf("pickAccountRole = (%q,%q,%q,%v), want a refusal and NOTHING picked", acct, role, refusal, ok)
	}
	if !strings.Contains(refusal, "DevPower") || !strings.Contains(refusal, rightAccount) {
		t.Errorf("refusal = %q, want it to name both the pinned role and the pinned account", refusal)
	}
}

// TestRun_PinnedPairUploaded is the same fact end to end: the pin arrives as
// launch env, and the blob that reaches the control plane carries the pinned
// pair.
func TestRun_PinnedPairUploaded(t *testing.T) {
	portal := multiAccountPortal(t)
	t.Setenv("WARDYN_AWS_SSO_ACCOUNT_ID", rightAccount)
	t.Setenv("WARDYN_AWS_SSO_ROLE_NAME", rightRole)
	uploaded, out := runHelper(t, portal)

	if uploaded == nil {
		t.Fatal("a pinned sign-in uploaded nothing")
	}
	var got struct {
		AccountID string `json:"account_id"`
		RoleName  string `json:"role_name"`
	}
	if err := json.Unmarshal(uploaded, &got); err != nil {
		t.Fatalf("decode uploaded body: %v", err)
	}
	if got.AccountID != rightAccount || got.RoleName != rightRole {
		t.Errorf("uploaded %q/%q, want the pinned %q/%q", got.AccountID, got.RoleName, rightAccount, rightRole)
	}
	if !strings.Contains(out, successMarker) {
		t.Errorf("stdout = %q, want the success marker", out)
	}
}

// TestRun_ChooserPicksAccountAndRole is ask 2: no pin, several accounts, a
// terminal to answer on. The person chooses; "2" is the account at index 1,
// and "1" its first role. Nothing here may fall back to index 0.
func TestRun_ChooserPicksAccountAndRole(t *testing.T) {
	portal := multiAccountPortal(t)
	withTerminal(t, "2\n1\n")
	uploaded, out := runHelper(t, portal)

	if uploaded == nil {
		t.Fatal("the chooser uploaded nothing")
	}
	var got struct {
		AccountID string `json:"account_id"`
		RoleName  string `json:"role_name"`
	}
	if err := json.Unmarshal(uploaded, &got); err != nil {
		t.Fatalf("decode uploaded body: %v", err)
	}
	if got.AccountID != rightAccount || got.RoleName != rightRole {
		t.Errorf("uploaded %q/%q, want the CHOSEN %q/%q", got.AccountID, got.RoleName, rightAccount, rightRole)
	}
	if !strings.Contains(out, wrongAccount) || !strings.Contains(out, rightAccount) {
		t.Errorf("the chooser must list every account it reaches; stdout = %q", out)
	}
}

// TestRun_ChooserSkippedWhenSingleAccountSingleRole is today's path, unchanged
// and green before this lane: one account, one role, no prompt, no refusal.
// A chooser that fired here would stall every single-account deployment.
func TestRun_ChooserSkippedWhenSingleAccountSingleRole(t *testing.T) {
	portal := awsssofake.New()
	t.Cleanup(portal.Close)
	prevBase := ssoPortalBase
	ssoPortalBase = func(string) string { return portal.URL() }
	t.Cleanup(func() { ssoPortalBase = prevBase })
	// A terminal IS present, with nothing to read: a prompt would block or
	// refuse, and either is a regression for a single-account operator.
	withTerminal(t, "")

	uploaded, out := runHelper(t, portal)
	if uploaded == nil {
		t.Fatal("a single-account sign-in uploaded nothing")
	}
	if strings.Contains(out, failMarker) {
		t.Errorf("a single-account sign-in was refused: %q", out)
	}
	if !strings.Contains(out, successMarker) {
		t.Errorf("stdout = %q, want the success marker and no prompt", out)
	}
	fixture := portal.Account()
	var got struct {
		AccountID string `json:"account_id"`
		RoleName  string `json:"role_name"`
	}
	if err := json.Unmarshal(uploaded, &got); err != nil {
		t.Fatalf("decode uploaded body: %v", err)
	}
	if got.AccountID != fixture.AccountID || got.RoleName != fixture.Roles[0] {
		t.Errorf("uploaded %q/%q, want the single fixture %q/%q", got.AccountID, got.RoleName, fixture.AccountID, fixture.Roles[0])
	}
}

// TestRun_PrintsFailMarkerOnRefusal is the "pane spins forever" bug: BEFORE
// this lane a refused upload logged to stderr and printed nothing on stdout,
// so the login pane — which ends the login only on a marker — waited out the
// sandbox's 30-minute idle cap on a credential that had already been refused.
//
// Both refusal shapes are covered: one this helper decides (no terminal, no
// pin, several accounts — nothing is uploaded) and one the CONTROL PLANE
// decides (the upload is answered 400, and the server's own sentence is what
// the person reads).
func TestRun_PrintsFailMarkerOnRefusal(t *testing.T) {
	t.Run("refused here: several accounts, no pin, no terminal", func(t *testing.T) {
		portal := multiAccountPortal(t)
		withoutTerminal(t)
		uploaded, out := runHelper(t, portal)
		if uploaded != nil {
			t.Error("a sign-in that could not choose an account uploaded one anyway")
		}
		assertFailLine(t, out)
	})

	t.Run("refused by the control plane: the server's sentence reaches the terminal", func(t *testing.T) {
		portal := multiAccountPortal(t)
		t.Setenv("WARDYN_AWS_SSO_ACCOUNT_ID", rightAccount)
		t.Setenv("WARDYN_AWS_SSO_ROLE_NAME", rightRole)
		out := runHelperAgainst(t, portal, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"this session is for account 111111111111; the configured Bedrock model lives in account 333333333333"}`))
		})
		line := assertFailLine(t, out)
		if !strings.Contains(line, "the configured Bedrock model lives in account 333333333333") {
			t.Errorf("fail line = %q, want the control plane's own sentence, not Go error plumbing", line)
		}
		if strings.Contains(line, "server returned 400") {
			t.Errorf("fail line = %q, leaks the transport wrapper", line)
		}
	})
}

// assertFailLine checks the marker contract the pane parses: ONE line on
// stdout, starting with failMarker, no success marker, at most 300 runes.
func assertFailLine(t *testing.T, out string) string {
	t.Helper()
	if strings.Contains(out, successMarker) {
		t.Fatalf("a refused capture printed the SUCCESS marker: %q", out)
	}
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, failMarker) {
			if line != "" {
				t.Fatalf("more than one fail line; the pane reads the first and the rest scroll the device code away:\n%s", out)
			}
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no fail marker on stdout — this is the pane spinning forever:\n%q", out)
	}
	if line == failMarker || !strings.HasPrefix(line, failMarker+" ") {
		t.Errorf("fail line = %q, want %q + one space + a sentence", line, failMarker)
	}
	if n := len([]rune(line)); n > maxFailLineRunes {
		t.Errorf("fail line is %d runes, want at most %d", n, maxFailLineRunes)
	}
	return line
}

// withoutTerminal is the CI / piped-shell / k8s-exec case: no person to ask.
// Forced explicitly because `go test` itself often inherits a real terminal,
// which would otherwise make this case silently exercise the chooser instead.
func withoutTerminal(t *testing.T) {
	t.Helper()
	prev := stdinIsTerminal
	stdinIsTerminal = func() bool { return false }
	t.Cleanup(func() { stdinIsTerminal = prev })
}

// withTerminal makes stdin look like a person's terminal carrying keys.
func withTerminal(t *testing.T, keys string) {
	t.Helper()
	prevIn, prevTTY := stdin, stdinIsTerminal
	stdin = strings.NewReader(keys)
	stdinIsTerminal = func() bool { return true }
	t.Cleanup(func() { stdin, stdinIsTerminal = prevIn, prevTTY })
}

// runHelper runs the whole helper against portal with an upload endpoint that
// accepts, and returns what was uploaded (nil if nothing was) plus stdout.
func runHelper(t *testing.T, portal *awsssofake.Server) ([]byte, string) {
	t.Helper()
	var uploaded []byte
	out := runHelperAgainst(t, portal, func(w http.ResponseWriter, r *http.Request) {
		uploaded, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})
	return uploaded, out
}

func runHelperAgainst(t *testing.T, portal *awsssofake.Server, upload http.HandlerFunc) string {
	t.Helper()
	home := t.TempDir()
	cacheDir := filepath.Join(home, ssoCacheSubdir)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cache := `{"accessToken":"` + portal.AccessToken() + `","startUrl":"https://x.awsapps.com/start","region":"us-east-1","expiresAt":"2100-01-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(cacheDir, "abc123.json"), []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(upload)
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	prevOut := stdout
	stdout = &buf
	t.Cleanup(func() { stdout = prevOut })

	t.Setenv("HOME", home)
	t.Setenv("WARDYN_PROXY_URL", srv.URL)
	t.Setenv("WARDYN_RUN_ID", "run-1")
	if err := run(); err != nil {
		t.Fatalf("run: %v — a refusal is never a process failure (the pane reads a marker, not an exit code)", err)
	}
	return buf.String()
}

// TestFailMarker_UIParity is TestSuccessMarker_UIParity's sibling, and it
// pins the OTHER half of the PTY handshake. A drift here is the same bug the
// fail marker exists to fix, one level up: the pane would keep waiting for a
// line this helper no longer prints.
func TestFailMarker_UIParity(t *testing.T) {
	uiPath := filepath.Join("..", "..", "ui", "src", "app", "components", "screens", "settings", "harness-login-pane.tsx")
	ts, err := os.ReadFile(uiPath)
	if err != nil {
		t.Fatalf("read %s: %v", uiPath, err)
	}
	m := regexp.MustCompile(`failMarker:\s*"([^"]+)"`).FindStringSubmatch(string(ts))
	if m == nil {
		t.Fatalf("no failMarker in %s", uiPath)
	}
	if m[1] != failMarker {
		t.Errorf("marker drift: UI failMarker %q != wardyn-aws-sso failMarker %q", m[1], failMarker)
	}
}
