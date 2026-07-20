// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command wardyn-aws-sso is Wardyn's in-sandbox AWS SSO credential capture
// helper. It runs INSIDE the aws-sso container-login run (see
// internal/api/harnesscred.go's awsSSOAgent row) AFTER the operator completes
// `aws sso login --no-browser --use-device-code` in the attach terminal, and
// uploads the resulting SSO token cache to the control plane.
//
// Unlike the Anthropic container-login flow (which prints a token to the PTY
// for the operator to paste), `aws sso login` writes its credential to a FILE
// (~/.aws/sso/cache/<sha1>.json), so capture here is read-file-then-upload
// rather than terminal scraping — see harnesscred.go's captureViaHelper doc.
//
// Upload contract (mirrors wardyn-scan/wardyn-verify's brokered upload — the
// proxy injects the run token, so the sandbox NEVER holds it):
//
//	PUT ${WARDYN_PROXY_URL}/wardyn/v1/sso-token/${WARDYN_RUN_ID}
//	Content-Type: application/json
//	body: json(ssoBlob)  -- field tags match internal/api's awsSSOBlob
//
// forwarded by the proxy's brokered sso-token route to the control plane's
// PUT-authenticated /api/v1/internal/sso-token/{runID}, which rejects a runID
// that doesn't match the token's run (cross-run pollution guard) and requires
// the run to be the aws-sso harness-login run.
//
// Env:
//
//	WARDYN_PROXY_URL  proxy base URL (required)
//	WARDYN_RUN_ID     this run's id (required)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/sidecar"
)

// ssoCacheSubdir is where `aws sso login` writes the SSO token cache, and
// (per the AWS CLI) also where separate role-credential caches land — see
// ssoCacheFile.isSSOToken for how the two are told apart.
const ssoCacheSubdir = ".aws/sso/cache"

// resolveTimeout bounds the best-effort account/role lookup so a slow or
// hanging `aws` CLI can never hang the upload.
const resolveTimeout = 15 * time.Second

func main() {
	if err := run(); err != nil {
		// Fail loud only on setup errors (missing env, no cache file to read).
		// Delivery failures are handled non-fatally inside run() (logged, exit
		// 0), matching wardyn-scan/wardyn-verify: a login run that can't
		// upload leaves nothing connected, an honest signal, without crashing
		// the throwaway run.
		fmt.Fprintln(os.Stderr, "wardyn-aws-sso:", err)
		os.Exit(1)
	}
}

func run() error {
	url, err := proxyURL()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}
	cache, err := newestSSOToken(filepath.Join(home, ssoCacheSubdir))
	if err != nil {
		return err
	}
	blob, err := toBlob(cache)
	if err != nil {
		return err
	}
	// Best-effort account/role resolution (ponytail: a failure just leaves
	// these blank; the server accepts that — see awsSSOBlob.valid).
	if accountID, roleName, ok := resolveAccountRole(blob.AccessToken, blob.Region); ok {
		blob.AccountID, blob.RoleName = accountID, roleName
	}

	body, err := json.Marshal(blob)
	if err != nil {
		return fmt.Errorf("marshal sso token: %w", err)
	}
	if derr := sidecar.Upload(url, body); derr != nil {
		fmt.Fprintln(os.Stderr, "wardyn-aws-sso: sso-token upload failed (non-fatal):", derr)
	}
	return nil
}

// proxyURL mirrors sidecar.ProxyRunURL's env validation, but targets the
// plain-noun sso-token route (like /wardyn/v1/recordings/) rather than the
// *-results/ convention: this PUTs a single captured credential, not a
// derived-facts result.
func proxyURL() (string, error) {
	base := strings.TrimRight(os.Getenv("WARDYN_PROXY_URL"), "/")
	if base == "" {
		return "", fmt.Errorf("WARDYN_PROXY_URL is required")
	}
	runID := os.Getenv("WARDYN_RUN_ID")
	if runID == "" {
		return "", fmt.Errorf("WARDYN_RUN_ID is required")
	}
	return base + "/wardyn/v1/sso-token/" + runID, nil
}

// ssoCacheFile is the JSON shape of an AWS CLI SSO TOKEN cache file
// (~/.aws/sso/cache/<sha1>.json).
type ssoCacheFile struct {
	AccessToken           string `json:"accessToken"`
	RefreshToken          string `json:"refreshToken"`
	ClientID              string `json:"clientId"`
	ClientSecret          string `json:"clientSecret"`
	StartURL              string `json:"startUrl"`
	Region                string `json:"region"`
	ExpiresAt             string `json:"expiresAt"`
	RegistrationExpiresAt string `json:"registrationExpiresAt"`
	// AccessKeyID is present only on a ROLE-credential cache file (the same
	// directory also holds those), never on an SSO token cache file — the
	// discriminator in isSSOToken.
	AccessKeyID string `json:"accessKeyId"`
}

// isSSOToken reports whether f is an SSO TOKEN cache file (has an access
// token bound to a start URL / region) rather than a role-credential cache
// file (has accessKeyId, no accessToken).
func (f ssoCacheFile) isSSOToken() bool {
	return f.AccessToken != "" && (f.StartURL != "" || f.Region != "") && f.AccessKeyID == ""
}

// ssoBlob is the JSON body PUT to the control plane. Field tags match
// internal/api's awsSSOBlob exactly — that JSON shape IS the contract between
// this sandbox helper and the control plane; there is deliberately no shared
// Go type across that trust boundary.
type ssoBlob struct {
	AccessToken           string    `json:"access_token"`
	RefreshToken          string    `json:"refresh_token,omitempty"`
	ClientID              string    `json:"client_id,omitempty"`
	ClientSecret          string    `json:"client_secret,omitempty"`
	StartURL              string    `json:"start_url"`
	Region                string    `json:"region"`
	AccountID             string    `json:"account_id,omitempty"`
	RoleName              string    `json:"role_name,omitempty"`
	ExpiresAt             time.Time `json:"expires_at"`
	RegistrationExpiresAt time.Time `json:"registration_expires_at,omitempty"`
}

// newestSSOToken reads dir and returns the most-recently-modified SSO TOKEN
// cache file's contents, skipping role-credential cache files and anything
// unparseable.
func newestSSOToken(dir string) (ssoCacheFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ssoCacheFile{}, fmt.Errorf("read sso cache dir %s: %w", dir, err)
	}
	var best ssoCacheFile
	var bestMod time.Time
	found := false
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			continue
		}
		var f ssoCacheFile
		if jerr := json.Unmarshal(raw, &f); jerr != nil || !f.isSSOToken() {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		if !found || info.ModTime().After(bestMod) {
			best, bestMod, found = f, info.ModTime(), true
		}
	}
	if !found {
		return ssoCacheFile{}, fmt.Errorf("no SSO token cache file found under %s (run `aws sso login` first)", dir)
	}
	return best, nil
}

// toBlob converts the raw cache file into the upload shape, parsing the AWS
// CLI's non-standard timestamp format (see parseSSOTime).
func toBlob(f ssoCacheFile) (ssoBlob, error) {
	expiresAt, err := parseSSOTime(f.ExpiresAt)
	if err != nil {
		return ssoBlob{}, fmt.Errorf("parse expiresAt: %w", err)
	}
	var regExpiresAt time.Time
	if f.RegistrationExpiresAt != "" {
		if regExpiresAt, err = parseSSOTime(f.RegistrationExpiresAt); err != nil {
			return ssoBlob{}, fmt.Errorf("parse registrationExpiresAt: %w", err)
		}
	}
	return ssoBlob{
		AccessToken: f.AccessToken, RefreshToken: f.RefreshToken,
		ClientID: f.ClientID, ClientSecret: f.ClientSecret,
		StartURL: f.StartURL, Region: f.Region,
		ExpiresAt: expiresAt, RegistrationExpiresAt: regExpiresAt,
	}, nil
}

// parseSSOTime parses the AWS CLI's SSO cache timestamps. v2.31.13 (the version
// pinned in deploy/images/aws-sso/Dockerfile) writes plain RFC3339 ("...Z") —
// verified against a real cache file in TestParseRealAWSCLICacheFile. Older
// botocore wrote a LITERAL "UTC" suffix ("2021-05-14T18:59:22UTC"), which is
// still accepted so an older CLI in a corp mirror doesn't break capture.
func parseSSOTime(s string) (time.Time, error) {
	if strings.HasSuffix(s, "UTC") {
		if t, err := time.Parse("2006-01-02T15:04:05", strings.TrimSuffix(s, "UTC")); err == nil {
			return t.UTC(), nil
		}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("unrecognized timestamp %q", s)
}

// resolveAccountRole best-effort shells out to `aws sso list-accounts` /
// `list-account-roles` and picks (first account, first role) — good enough
// for a single-account SSO setup; a multi-account operator can leave this
// blank and resolve later. Any failure (aws CLI missing, no accounts,
// timeout, malformed output) returns ok=false — callers must treat that as
// "leave it empty", not a fatal error (see run()).
func resolveAccountRole(accessToken, region string) (accountID, roleName string, ok bool) {
	if accessToken == "" || region == "" {
		return "", "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()

	var accounts struct {
		AccountList []struct {
			AccountID string `json:"accountId"`
		} `json:"accountList"`
	}
	if !runAWSJSON(ctx, &accounts, "sso", "list-accounts", "--access-token", accessToken, "--region", region) ||
		len(accounts.AccountList) == 0 {
		return "", "", false
	}
	accountID = accounts.AccountList[0].AccountID

	var roles struct {
		RoleList []struct {
			RoleName string `json:"roleName"`
		} `json:"roleList"`
	}
	if !runAWSJSON(ctx, &roles, "sso", "list-account-roles", "--access-token", accessToken,
		"--account-id", accountID, "--region", region) || len(roles.RoleList) == 0 {
		return "", "", false
	}
	return accountID, roles.RoleList[0].RoleName, true
}

// runAWSJSON runs `aws <args> --output json` and decodes stdout into dst,
// reporting false on any exec or decode failure.
func runAWSJSON(ctx context.Context, dst any, args ...string) bool {
	out, err := exec.CommandContext(ctx, "aws", append(args, "--output", "json")...).Output()
	if err != nil {
		return false
	}
	return json.Unmarshal(out, dst) == nil
}
