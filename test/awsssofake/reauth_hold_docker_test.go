// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package awsssofake

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// THE MEASUREMENT (0.7.6, Finding 4). The mid-run credential hold's whole
// premise is that the sandbox's SDK will WAIT while its owner signs in again,
// and the length of that patience is the one number this tree cannot derive:
// aws-sdk-js-v3 awaits the credential provider inside the signing middleware
// with no request timeout of its own by default, and botocore uses a 60s read
// timeout times three retries — but neither is the number an operator needs,
// which is "how long will MY agent image tolerate a parked
// GetRoleCredentials".
//
// So it is measured, against the REAL agent image, with the fake standing in
// for the proxy's hold. The fake parking the answer is behaviourally identical
// to the proxy holding it: from the SDK's side both are a GetRoleCredentials
// that has not answered yet.
//
// What this does NOT need, deliberately: wardynd, a proxy sidecar, a
// substrate. Putting the whole stack in the way would measure the stack.

// claudeCodeImage is the agent image whose SDK the measurement is about.
const claudeCodeImage = "wardyn/agent-claude-code:local"

// reauthPlaceholderToken mirrors internal/api's awsSSOPlaceholderToken. It is
// spelled here rather than imported because test/awsssofake must not import the
// control plane (the fake is what the control plane is tested AGAINST).
const reauthPlaceholderToken = "wardyn-proxy-injected"

func requireImage(t *testing.T, image string) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker binary not found on PATH; skipping")
	}
	if err := exec.Command("docker", "image", "inspect", image).Run(); err != nil {
		t.Skipf("image %s not present locally (build it with `make agent-images`); skipping", image)
	}
}

// syntheticAWSHome is the sandbox ~/.aws Wardyn generates for the
// captured-AWS-SSO lane — the PROXY-INJECTED shape, whose token cache carries
// the inert placeholder. Mirrors internal/api's awsSSOConfigFileContents +
// awsSSOCacheFileContents(blob, true).
func syntheticAWSHome(sessionName, startURL, region, accountID, roleName string, proxyInjected bool, realToken string) map[string]string {
	sum := sha1.Sum([]byte(sessionName))
	cacheName := hex.EncodeToString(sum[:])
	token, expires := realToken, time.Now().Add(time.Hour)
	if proxyInjected {
		token, expires = reauthPlaceholderToken, time.Now().Add(30*24*time.Hour)
	}
	cache, _ := json.Marshal(map[string]any{
		"startUrl": startURL, "region": region,
		"accessToken": token, "expiresAt": expires.UTC().Format(time.RFC3339),
	})
	return map[string]string{
		".aws/config": fmt.Sprintf(
			"[sso-session %[1]s]\nsso_start_url = %[2]s\nsso_region = %[3]s\nsso_registration_scopes = sso:account:access\n\n"+
				"[profile %[1]s]\nsso_session = %[1]s\nsso_account_id = %[4]s\nsso_role_name = %[5]s\noutput = json\n",
			sessionName, startURL, region, accountID, roleName),
		".aws/sso/cache/" + cacheName + ".json": string(cache),
	}
}

// writeAWSHome materializes the synthetic ~/.aws layout under dir before it is
// bind-mounted into the agent image at /home/agent. World-readable (0o755/
// 0o644), not the 0o700/0o600 a real credentials directory would get: the
// image's agent user is a FIXED uid (1000, deploy/images/claude-code/
// Dockerfile), but the host uid writing these files is whatever runs `go
// test` — a GitHub-hosted runner's default user is uid 1001, not 1000, so a
// file mode that only the WRITER can read left the container's read of its
// own bind-mounted home permission-denied on every nightly (#511/F6): not
// just the direct read in (c), but the SDK's own read of the SSO token cache
// in (a)/(b), which is why those hung for zero GetRoleCredentials calls
// rather than reusing the cached token. This is a throwaway t.TempDir() this
// one test process owns for its own lifetime, on a runner or laptop nobody
// else's containers share, so there is no boundary a permissive mode weakens.
func writeAWSHome(t *testing.T, dir string, home map[string]string) {
	t.Helper()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod %s: %v", dir, err)
	}
	for rel, contents := range home {
		dst := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(dst, []byte(contents), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

// runClaudeAgainstFake starts `claude -p` in the agent image, pointed at the
// fake for BOTH the SSO services and the Bedrock data plane, and returns the
// running command plus a buffer collecting its output. The caller waits.
func runClaudeAgainstFake(t *testing.T, s *Server, home map[string]string, timeout time.Duration) (*exec.Cmd, *bytes.Buffer, context.CancelFunc) {
	t.Helper()
	dir := t.TempDir()
	writeAWSHome(t, dir, home)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm",
		"--network", "host",
		"-e", "CLAUDE_CODE_USE_BEDROCK=1",
		"-e", "AWS_REGION=eu-west-2",
		"-e", "AWS_PROFILE=wardyn",
		"-e", "AWS_CONFIG_FILE=/home/agent/.aws/config",
		"-e", "AWS_SHARED_CREDENTIALS_FILE=/home/agent/.aws/credentials",
		"-e", "AWS_ENDPOINT_URL_SSO_OIDC="+s.URL(),
		"-e", "AWS_ENDPOINT_URL_SSO="+s.URL(),
		"-e", "ANTHROPIC_BEDROCK_BASE_URL="+s.URL(),
		"-e", "AWS_ENDPOINT_URL_BEDROCK_RUNTIME="+s.URL(),
		"-e", "ANTHROPIC_MODEL=us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		"-v", dir+":/home/agent",
		"--entrypoint", "claude", claudeCodeImage,
		"-p", "say hi",
	)
	buf := &bytes.Buffer{}
	cmd.Stdout = buf
	cmd.Stderr = buf
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start claude: %v", err)
	}
	return cmd, buf, cancel
}

// waitForRoleCredCall blocks until the fake has seen at least n calls.
func waitForRoleCredCall(t *testing.T, s *Server, n int, within time.Duration, out *bytes.Buffer) []time.Time {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		calls := s.RoleCredentialCallTimes()
		if len(calls) >= n {
			return calls
		}
		if time.Now().After(deadline) {
			// The agent's OWN output, or this failure says nothing about why:
			// an image that cannot read its config, or one that refused the
			// endpoint override, looks identical to one that is waiting.
			t.Fatalf("the agent made %d GetRoleCredentials calls in %v, want at least %d\n--- agent output ---\n%s",
				len(calls), within, n, out.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// (a) THE TOLERANCE. How long the real agent image's SDK leaves a parked
// GetRoleCredentials outstanding before it gives up. The plan's provisional
// default for WARDYN_CREDENTIAL_REAUTH_TIMEOUT is 600s; this is the number that
// confirms or lowers it, and the test REPORTS it rather than asserting a
// guessed figure.
//
// The floor it does assert is the one the feature needs to be worth having: a
// hold is pointless if the SDK gives up in seconds.
func TestDocker_SDKToleratesAParkedCredentialExchange(t *testing.T) {
	SkipUnlessDocker(t)
	requireImage(t, claudeCodeImage)

	s := New()
	defer s.Close()
	// Park far longer than any plausible tolerance: the measurement is where
	// the AGENT gives up, not where the fake does.
	s.SetParkRoleCreds(30 * time.Minute)

	// THE REAL TOKEN, not the placeholder. In production the PROXY puts the
	// session on the wire and the portal sees a valid bearer; here there is no
	// proxy, so the sandbox carries it. A placeholder would be 401'd at the
	// door and this would measure the fake's bearer check, not the SDK's
	// patience — which is exactly what the first run of this measured.
	_, _, access, _ := signIn(t, s)
	if access != s.AccessToken() {
		t.Fatalf("the sign-in's token is not the one the portal accepts (%q vs %q)", access, s.AccessToken())
	}
	home := syntheticAWSHome("wardyn", "https://fake.awsapps.com/start", "eu-west-2",
		"111111111111", "AdministratorAccess", false, access)
	cmd, out, cancel := runClaudeAgainstFake(t, s, home, 12*time.Minute)
	defer cancel()

	calls := waitForRoleCredCall(t, s, 1, 90*time.Second, out)
	parkedAt := calls[0]

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var gaveUpAfter time.Duration
	select {
	case <-done:
		gaveUpAfter = time.Since(parkedAt)
	case <-time.After(11 * time.Minute):
		_ = cmd.Process.Kill()
		gaveUpAfter = time.Since(parkedAt)
		t.Logf("MEASURED TOLERANCE: the agent was STILL waiting after %v — it outlasts the whole test budget", gaveUpAfter)
	}

	cadence := s.RoleCredentialCallTimes()
	gaps := make([]string, 0, len(cadence))
	for i := 1; i < len(cadence); i++ {
		gaps = append(gaps, cadence[i].Sub(cadence[i-1]).Round(time.Second).String())
	}
	t.Logf("MEASURED TOLERANCE (%s): the SDK left a parked GetRoleCredentials outstanding for %v before giving up",
		claudeCodeImage, gaveUpAfter.Round(time.Second))
	t.Logf("MEASURED RE-CALL CADENCE: %d GetRoleCredentials calls, gaps %v", len(cadence), gaps)
	t.Logf("agent output (tail): %s", tail(out.String(), 600))

	// The floor the feature needs. 180s is the plan's own bar: below it, a
	// human-in-the-loop sign-in cannot land inside the hold and the knob's
	// default must come down to match.
	if gaveUpAfter < 180*time.Second {
		t.Errorf("the SDK gave up after %v, under the 180s floor a human-in-the-loop hold needs — "+
			"WARDYN_CREDENTIAL_REAUTH_TIMEOUT's default must come down to below this number, and the "+
			"copy must stop implying minutes", gaveUpAfter)
	}
}

// (b) THE RESUME. The same process, on the new credential, with the pinned
// account and role — no new run, no new container, no second sign-in.
func TestDocker_TheSameRunResumesWhenTheHoldReleases(t *testing.T) {
	SkipUnlessDocker(t)
	requireImage(t, claudeCodeImage)

	s := New()
	defer s.Close()
	s.SetAccounts([]Account{{AccountID: "222222222222", Roles: []string{"WardynDev"}}})
	s.SetParkRoleCreds(30 * time.Minute)

	_, _, access, _ := signIn(t, s)
	home := syntheticAWSHome("wardyn", "https://fake.awsapps.com/start", "eu-west-2",
		"222222222222", "WardynDev", false, access)
	cmd, out, cancel := runClaudeAgainstFake(t, s, home, 8*time.Minute)
	defer cancel()

	calls := waitForRoleCredCall(t, s, 1, 90*time.Second, out)
	parkedAt := calls[0]
	// The sign-in lands. Everything parked is freed at once.
	time.Sleep(20 * time.Second)
	s.ReleaseParkedRoleCreds()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		t.Logf("the agent finished %v after its credential exchange was parked (err=%v)",
			time.Since(parkedAt).Round(time.Second), err)
	case <-time.After(5 * time.Minute):
		_ = cmd.Process.Kill()
		t.Fatalf("the agent never finished after the hold was released\n--- output ---\n%s", tail(out.String(), 2000))
	}

	seen := s.RoleCredentialsSeen()
	if seen.AccountID != "222222222222" || len(seen.Roles) == 0 || seen.Roles[0] != "WardynDev" {
		t.Errorf("GetRoleCredentials was asked for %+v, want the PINNED account/role — a resume must not "+
			"re-authorize against a different account or role", seen)
	}
	if s.BedrockCalls() == 0 {
		t.Errorf("the agent made no model call after the hold released; output:\n%s", tail(out.String(), 2000))
	}
}

// (c) THE CACHE FILE NEVER HOLDS THE REAL TOKEN. Phase B's whole claim, checked
// against what actually lands in the container rather than against the
// generator's unit test.
func TestDocker_TheSandboxCacheHoldsOnlyThePlaceholder(t *testing.T) {
	SkipUnlessDocker(t)
	requireImage(t, claudeCodeImage)

	s := New()
	defer s.Close()
	const realToken = "the-real-sso-access-token-must-not-be-here"
	home := syntheticAWSHome("wardyn", "https://fake.awsapps.com/start", "eu-west-2",
		"111111111111", "AdministratorAccess", true, realToken)

	dir := t.TempDir()
	writeAWSHome(t, dir, home)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "run", "--rm",
		"-v", dir+":/home/agent",
		"--entrypoint", "sh", claudeCodeImage,
		"-c", "cat /home/agent/.aws/sso/cache/*.json",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("read the container's cache file: %v\n%s", err, out)
	}
	if strings.Contains(string(out), realToken) {
		t.Fatalf("the REAL SSO access token is in the sandbox's token cache — Phase B's whole point is that it is not:\n%s", out)
	}
	if !strings.Contains(string(out), reauthPlaceholderToken) {
		t.Errorf("the cache does not carry the inert placeholder: %s", out)
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
