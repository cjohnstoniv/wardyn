// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package awsssofake

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// DefaultImage is the AWS SSO login image (deploy/images/aws-sso/Dockerfile),
// built by `make agent-images` (or the aws-sso-specific target) as
// wardyn/agent-aws-sso:local. The `aws` CLI v2 (device-code capable) exists
// ONLY inside this image, so exercising a real `aws sso login` against the
// fake requires Docker, not just a local `aws` binary.
const DefaultImage = "wardyn/agent-aws-sso:local"

// SkipUnlessDocker centralizes the WARDYN_TEST_DOCKER=1 skip-guard (same
// convention as test/conformance/conformance_docker_test.go and
// cmd/wardyn-runner/standalone_docker_test.go's sdSkipNoDocker) so every
// docker-backed SSO test skips cleanly without Docker and MUST pass when
// WARDYN_TEST_DOCKER=1 is set and a daemon is reachable via DOCKER_HOST.
func SkipUnlessDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 (and DOCKER_HOST, if the default socket isn't the live daemon) to run the AWS SSO fake docker e2e")
	}
}

// LoginResult is what a completed `aws sso login` run against the fake
// produced.
type LoginResult struct {
	// CacheDir is the host directory bind-mounted as the container's
	// ~/.aws/sso/cache — the REAL cache file(s) `aws sso login` wrote are
	// still there after the container exits, so a caller can read them with
	// its own package-internal parser (see cmd/wardyn-aws-sso/main_test.go)
	// without a docker cp round trip.
	CacheDir string
	// CacheFile is the single SSO-token cache file's path within CacheDir.
	CacheFile string
	// RawCacheJSON is CacheFile's contents, for convenience.
	RawCacheJSON []byte
}

// RunDeviceCodeLogin drives the REAL `aws sso login --no-browser
// --use-device-code` inside DefaultImage against the fake server s, waits
// for the device-authorization request to land and calls s.Approve(), then
// waits for the CLI to finish and returns the real cache file it wrote.
//
// Uses --network host so the containerized CLI can reach the fake (bound to
// 127.0.0.1 on the host) without any CA/TLS plumbing — the fake serves plain
// HTTP and AWS_ENDPOINT_URL_SSO_OIDC/AWS_ENDPOINT_URL_SSO accept a plain
// http:// override (verified empirically here; see SkipUnlessDocker callers'
// doc comments for the verdict).
//
// startURL/region are the sso-session values written into the container's
// generated ~/.aws/config; sessionName/profileName name the sso-session /
// profile blocks (callers pass awsSSOProfileName's value to mirror
// production, or an arbitrary name for a synthetic-fixture test).
func RunDeviceCodeLogin(t *testing.T, s *Server, sessionName, profileName, startURL, region string) LoginResult {
	t.Helper()
	requireDockerBinary(t)

	awsDir := t.TempDir()
	configPath := filepath.Join(awsDir, "config")
	config := fmt.Sprintf(
		"[sso-session %[1]s]\nsso_start_url = %[3]s\nsso_region = %[4]s\nsso_registration_scopes = sso:account:access\n\n"+
			"[profile %[2]s]\nsso_session = %[1]s\noutput = json\n",
		sessionName, profileName, startURL, region,
	)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write fake ~/.aws/config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "run", "--rm",
		"--network", "host",
		"-e", "AWS_ENDPOINT_URL_SSO_OIDC="+s.URL(),
		"-e", "AWS_ENDPOINT_URL_SSO="+s.URL(),
		"-e", "AWS_PAGER=",
		"-v", awsDir+":/home/agent/.aws",
		DefaultImage,
		"aws", "sso", "login", "--profile", profileName, "--no-browser", "--use-device-code",
	)
	out, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	cmd.Stdout = cmd.Stderr // aws CLI writes the verification prompt to stdout
	if err := cmd.Start(); err != nil {
		t.Fatalf("start docker run: %v", err)
	}
	// Drain (and discard, keeping only for failure diagnostics) stderr/stdout
	// concurrently so the CLI's pipe never fills and blocks it.
	logCh := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 0, 4096)
		tmp := make([]byte, 4096)
		for {
			n, rerr := out.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
			}
			if rerr != nil {
				break
			}
		}
		logCh <- buf
	}()

	// Wait for the CLI to hit StartDeviceAuthorization, then approve. Poll
	// briefly rather than sleeping a fixed guess.
	deadline := time.Now().Add(30 * time.Second)
	for s.StartURLSeen() == "" {
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatalf("aws sso login never reached StartDeviceAuthorization within 30s")
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.Approve()

	waitErr := cmd.Wait()
	cliLog := <-logCh
	if waitErr != nil {
		t.Fatalf("aws sso login failed: %v\n--- container output ---\n%s", waitErr, cliLog)
	}

	// The AWS CLI writes TWO files to ~/.aws/sso/cache: a client-registration
	// cache (clientId/clientSecret/expiresAt/scopes, no accessToken) and the
	// SSO TOKEN cache we actually want (has accessToken) — confirmed
	// empirically against the real image. Same discrimination
	// cmd/wardyn-aws-sso's isSSOToken applies (accessToken present); pick the
	// one with the newest mtime among those, mirroring newestSSOToken.
	cacheDir := filepath.Join(awsDir, "sso", "cache")
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatalf("read sso cache dir %s after login: %v\n--- container output ---\n%s", cacheDir, err, cliLog)
	}
	var cacheFile string
	var newestMod time.Time
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		p := filepath.Join(cacheDir, e.Name())
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			continue
		}
		var probe struct {
			AccessToken string `json:"accessToken"`
		}
		if json.Unmarshal(raw, &probe) != nil || probe.AccessToken == "" {
			continue // registration cache, or unparseable — skip
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		if cacheFile == "" || info.ModTime().After(newestMod) {
			cacheFile, newestMod = p, info.ModTime()
		}
	}
	if cacheFile == "" {
		t.Fatalf("no SSO-token .json cache file (with accessToken) written to %s after login\n--- container output ---\n%s", cacheDir, cliLog)
	}
	raw, err := os.ReadFile(cacheFile)
	if err != nil {
		t.Fatalf("read cache file %s: %v", cacheFile, err)
	}
	return LoginResult{CacheDir: cacheDir, CacheFile: cacheFile, RawCacheJSON: raw}
}

// RunAWSCommand runs an arbitrary `aws` invocation inside DefaultImage with a
// FRESH container HOME seeded from homeFiles (host-relative-to-container-HOME
// path -> file contents — e.g. ".aws/config", ".aws/sso/cache/<hash>.json"),
// pointed at the fake's endpoints, and returns its combined output and exit
// error. Used to prove a SYNTHETIC (Wardyn-generated) ~/.aws is accepted by
// real botocore, as opposed to RunDeviceCodeLogin's real CLI-written cache.
func RunAWSCommand(t *testing.T, s *Server, homeFiles map[string]string, args ...string) (output string, err error) {
	t.Helper()
	requireDockerBinary(t)

	home := t.TempDir()
	for relpath, contents := range homeFiles {
		dst := filepath.Join(home, relpath)
		if mkErr := os.MkdirAll(filepath.Dir(dst), 0o700); mkErr != nil {
			t.Fatalf("mkdir for %s: %v", relpath, mkErr)
		}
		if wErr := os.WriteFile(dst, []byte(contents), 0o600); wErr != nil {
			t.Fatalf("write %s: %v", relpath, wErr)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dockerArgs := append([]string{
		"run", "--rm",
		"--network", "host",
		"-e", "AWS_ENDPOINT_URL_SSO_OIDC=" + s.URL(),
		"-e", "AWS_ENDPOINT_URL_SSO=" + s.URL(),
		"-e", "AWS_PAGER=",
		"-v", home + ":/home/agent",
		DefaultImage,
	}, args...)
	out, cmdErr := exec.CommandContext(ctx, "docker", dockerArgs...).CombinedOutput()
	return string(out), cmdErr
}

func requireDockerBinary(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker binary not found on PATH; skipping")
	}
}
