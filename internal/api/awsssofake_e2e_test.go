// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// awsssofake_e2e_test.go retires risk #2 of the aws-sso-fake validation work
// (see runs_bedrock.go's awsSSOCacheFileName / awsSSOConfigFileContents doc
// comments): does the ~/.aws/config + ~/.aws/sso/cache/<hash>.json shape this
// package SYNTHESIZES for a captured SSO credential (the ssoInject branch of
// resolveBedrockAuth) actually get accepted by real botocore? A wrong
// cache-filename hash convention or a malformed config block is a SILENT
// failure — the SDK just reports "not logged in", with no pointer back at
// Wardyn — so this has to be proven against the REAL AWS CLI, not asserted
// against our own understanding of the format.
//
// This test does NOT touch the real device-code login flow itself (that's
// covered by test/awsssofake's own docker_test.go and
// cmd/wardyn-aws-sso/main_test.go's TestParseRealAWSCLICacheFile) — it only
// needs a plausible awsSSOBlob, which it builds directly from a real cache
// file's raw JSON fields (parsed locally here, not via cmd/wardyn-aws-sso's
// parser — that parser is validated separately; this test's job is the
// GENERATOR, not the parser).

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/test/awsssofake"
)

// TestAWSSSOConfigAcceptedByRealBotocore is the critical round-trip: take a
// real captured SSO credential, run it through the REAL
// awsSSOConfigFileContents + awsSSOCacheFileName + awsSSOCacheFileContents,
// write that synthetic ~/.aws into a FRESH container HOME (one that never
// ran `aws sso login` itself), and confirm real botocore resolves credentials
// from it against the fake sso portal.
//
// Requires Docker (test/awsssofake.SkipUnlessDocker); skips cleanly without
// it so `go test ./...` stays green on a daemon-less machine.
func TestAWSSSOConfigAcceptedByRealBotocore(t *testing.T) {
	awsssofake.SkipUnlessDocker(t)

	s := awsssofake.New()
	defer s.Close()

	// Phase 1: a real device-code login gives us a real, CLI-shaped SSO
	// token cache file to build the blob from.
	login := awsssofake.RunDeviceCodeLogin(t, s, "src-session", "src-profile", "https://fake.awsapps.com/start", "us-east-1")

	blob := blobFromRealCacheFile(t, login.RawCacheJSON)
	acct := s.Account()
	blob.AccountID = acct.AccountID
	blob.RoleName = acct.Roles[0]

	// Phase 2: the REAL generator this test exists to validate.
	config := awsSSOConfigFileContents(blob)
	cacheJSON := awsSSOCacheFileContents(blob)
	cacheName := awsSSOCacheFileName(awsSSOProfileName)
	t.Logf("generated ~/.aws/config:\n%s", config)
	t.Logf("generated cache filename: sso/cache/%s.json", cacheName)

	homeFiles := map[string]string{
		".aws/config":                           config,
		".aws/sso/cache/" + cacheName + ".json": cacheJSON,
	}

	// Phase 3: a FRESH container HOME (never ran `aws sso login`) + our
	// synthetic ~/.aws only. `aws configure export-credentials` resolves
	// credentials through the exact same profile chain a real Bedrock SDK
	// call would use (sso-session lookup -> cache-file-by-hash lookup ->
	// GetRoleCredentials) and stops there — no further signed call, so it
	// isolates exactly the config+cache-shape question this test is about.
	out, runErr := awsssofake.RunAWSCommand(t, s, homeFiles,
		"aws", "configure", "export-credentials", "--profile", awsSSOProfileName)

	t.Logf("aws configure export-credentials output:\n%s", out)

	if runErr != nil || strings.Contains(out, "Unable to retrieve credentials") || strings.Contains(out, "not logged in") {
		t.Errorf("VERDICT: real botocore REJECTED Wardyn's generated ~/.aws (runErr=%v).\n"+
			"generated config:\n%s\ngenerated cache filename: sso/cache/%s.json\noutput:\n%s",
			runErr, config, cacheName, out)
		return
	}
	if !strings.Contains(out, "AccessKeyId") {
		t.Errorf("VERDICT: export-credentials succeeded but output doesn't look like a credentials JSON: %s", out)
		return
	}
	t.Logf("VERDICT: real botocore ACCEPTED Wardyn's generated ~/.aws/config + sso/cache/%s.json — awsSSOCacheFileName + awsSSOConfigFileContents match real botocore's convention.", cacheName)
}

// rawSSOCacheFile mirrors cmd/wardyn-aws-sso's ssoCacheFile — a standalone
// copy is deliberate (see file doc comment: this test validates the
// GENERATOR, independent of that package's parser).
type rawSSOCacheFile struct {
	AccessToken           string `json:"accessToken"`
	RefreshToken          string `json:"refreshToken"`
	ClientID              string `json:"clientId"`
	ClientSecret          string `json:"clientSecret"`
	StartURL              string `json:"startUrl"`
	Region                string `json:"region"`
	ExpiresAt             string `json:"expiresAt"`
	RegistrationExpiresAt string `json:"registrationExpiresAt"`
}

func blobFromRealCacheFile(t *testing.T, raw []byte) awsSSOBlob {
	t.Helper()
	var f rawSSOCacheFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("unmarshal real cache file: %v\nraw: %s", err, raw)
	}
	expiresAt, err := parseRawSSOTime(f.ExpiresAt)
	if err != nil {
		t.Fatalf("parse real expiresAt %q: %v", f.ExpiresAt, err)
	}
	var regExpiresAt time.Time
	if f.RegistrationExpiresAt != "" {
		if regExpiresAt, err = parseRawSSOTime(f.RegistrationExpiresAt); err != nil {
			t.Fatalf("parse real registrationExpiresAt %q: %v", f.RegistrationExpiresAt, err)
		}
	}
	return awsSSOBlob{
		AccessToken: f.AccessToken, RefreshToken: f.RefreshToken,
		ClientID: f.ClientID, ClientSecret: f.ClientSecret,
		StartURL: f.StartURL, Region: f.Region,
		ExpiresAt: expiresAt, RegistrationExpiresAt: regExpiresAt,
	}
}

// parseRawSSOTime accepts both the AWS CLI's legacy literal-"UTC"-suffix
// format and standard RFC3339 (the current AWS CLI v2.31.13 emits RFC3339 —
// confirmed empirically in test/awsssofake/docker_test.go).
func parseRawSSOTime(v string) (time.Time, error) {
	if strings.HasSuffix(v, "UTC") {
		if t, err := time.Parse("2006-01-02T15:04:05", strings.TrimSuffix(v, "UTC")); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Parse(time.RFC3339, v)
}

// TestAWSSSOConfigAcceptedByRealBotocore_MultiAccountPinned is the
// multi-account half: a blob carrying the PINNED account/role — the one at
// index 1, never index 0 — generates an ~/.aws that real botocore resolves
// AGAINST THAT ACCOUNT.
//
// This used to say it REPLACED a live walk, on the grounds that the login
// sandbox had no AWS_ENDPOINT_URL_SSO* passthrough and that adding one "would
// be a production escape hatch". The second half was true of an UNGATED
// passthrough only. 0.7.4 adds a gated one — WARDYN_AWS_SSO_ENDPOINT_OVERRIDE,
// refused unless WARDYN_ALLOW_TEST_ENDPOINTS=true, WARNing on every boot, and
// published as residual #45 (internal/api/awssso_endpoint.go) — so the live
// walk exists: scripts/kind-sso-walk.sh signs two Dex principals in on a kind
// cluster against test/awsssofake and asserts, from the fake's own /_seen, that
// the MEMBER's pinned pair is what botocore asked for.
//
// This test is therefore no longer a substitute for that walk; it is its
// hermetic, Docker-only-gated FLOOR. It runs with no cluster, in seconds, and
// it isolates the GENERATOR: an identical-shaped config naming the wrong
// account passes the sibling test above and fails this one, because the
// assertion is on what botocore asked the portal to mint. A real two-entitlement
// AWS tenant remains owner-hardware-only.
//
// The sibling test above proves the generator's SHAPE with one account. This
// one proves its CONTENT: an identical-shaped config that named the wrong
// account would pass that test and fail this one, because the assertion is on
// what botocore asked the portal to mint, not on what Wardyn wrote down.
//
// Requires Docker (test/awsssofake.SkipUnlessDocker); skips cleanly without it.
func TestAWSSSOConfigAcceptedByRealBotocore_MultiAccountPinned(t *testing.T) {
	awsssofake.SkipUnlessDocker(t)

	s := awsssofake.New()
	defer s.Close()
	// The operator's reported shape: index 0 is an unrelated dev account whose
	// roles cannot invoke the configured model; index 1 is the account
	// WARDYN_BEDROCK_MODEL names. Index 0 is always the wrong answer.
	const pinnedAccount, pinnedRole = "111111111111", "BedrockRunner"
	s.SetAccounts([]awsssofake.Account{
		{AccountID: "222222222222", Roles: []string{"ReadOnly", "DevPower"}},
		{AccountID: pinnedAccount, Roles: []string{pinnedRole, "AdministratorAccess"}},
	})

	login := awsssofake.RunDeviceCodeLogin(t, s, "src-session", "src-profile", "https://fake.awsapps.com/start", "us-east-1")
	blob := blobFromRealCacheFile(t, login.RawCacheJSON)
	blob.AccountID = pinnedAccount
	blob.RoleName = pinnedRole

	config := awsSSOConfigFileContents(blob)
	cacheName := awsSSOCacheFileName(awsSSOProfileName)
	t.Logf("generated ~/.aws/config:\n%s", config)

	out, runErr := awsssofake.RunAWSCommand(t, s, map[string]string{
		".aws/config":                           config,
		".aws/sso/cache/" + cacheName + ".json": awsSSOCacheFileContents(blob),
	}, "aws", "configure", "export-credentials", "--profile", awsSSOProfileName)
	t.Logf("aws configure export-credentials output:\n%s", out)

	if runErr != nil || !strings.Contains(out, "AccessKeyId") {
		t.Fatalf("real botocore did not resolve the PINNED pair (runErr=%v)\nconfig:\n%s\noutput:\n%s", runErr, config, out)
	}
	asked := s.RoleCredentialsSeen()
	if asked.AccountID != pinnedAccount {
		t.Errorf("botocore asked the portal for account %q, want the PINNED %q — index 0 (222222222222) is the unrelated account the operator's cloud team granted", asked.AccountID, pinnedAccount)
	}
	if len(asked.Roles) != 1 || asked.Roles[0] != pinnedRole {
		t.Errorf("botocore asked for role %v, want the PINNED %q", asked.Roles, pinnedRole)
	}
	t.Logf("VERDICT: real botocore minted credentials for the PINNED %s/%s, not for AccountList[0].", asked.AccountID, pinnedRole)
}
