// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/test/awsssofake"
)

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
