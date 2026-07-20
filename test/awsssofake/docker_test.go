// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package awsssofake

import (
	"encoding/json"
	"testing"
	"time"
)

// TestDeviceCodeLoginRealCLI proves the fake is good enough to fool the REAL
// AWS CLI v2 (deploy/images/aws-sso/Dockerfile, wardyn/agent-aws-sso:local):
// a real `aws sso login --no-browser --use-device-code` against it completes
// after Approve() and writes a real ~/.aws/sso/cache/*.json file with the
// shape cmd/wardyn-aws-sso's parser expects (see that package's
// TestParseRealAWSCLICacheFile for the parser-level assertion).
//
// Requires Docker: WARDYN_TEST_DOCKER=1 and DOCKER_HOST pointed at a live
// daemon (see SkipUnlessDocker); skips cleanly otherwise.
func TestDeviceCodeLoginRealCLI(t *testing.T) {
	SkipUnlessDocker(t)

	s := New()
	defer s.Close()

	result := RunDeviceCodeLogin(t, s, "wardyn", "wardyn", "https://fake.awsapps.com/start", "us-east-1")

	var cache struct {
		AccessToken string `json:"accessToken"`
		StartURL    string `json:"startUrl"`
		Region      string `json:"region"`
		ExpiresAt   string `json:"expiresAt"`
	}
	if err := json.Unmarshal(result.RawCacheJSON, &cache); err != nil {
		t.Fatalf("unmarshal real cache file %s: %v\nraw: %s", result.CacheFile, err, result.RawCacheJSON)
	}
	if cache.AccessToken == "" {
		t.Error("real cache file has no accessToken")
	}
	if cache.AccessToken != s.AccessToken() {
		t.Errorf("real cache file accessToken = %q, want it to match the fake's issued token %q", cache.AccessToken, s.AccessToken())
	}
	if cache.StartURL != "https://fake.awsapps.com/start" {
		t.Errorf("real cache file startUrl = %q, want the configured start URL", cache.StartURL)
	}
	if cache.Region != "us-east-1" {
		t.Errorf("real cache file region = %q, want us-east-1", cache.Region)
	}
	// Empirically, AWS CLI v2.31.13 (deploy/images/aws-sso/Dockerfile's
	// pinned AWS_CLI_VERSION) writes a STANDARD RFC3339 "Z"-suffixed
	// timestamp here, not the legacy literal "...UTC" suffix
	// cmd/wardyn-aws-sso/main.go's parseSSOTime specifically special-cases
	// (that format is real — older botocore emitted it — but this CLI
	// version has moved on). parseSSOTime's RFC3339 fallback branch handles
	// this correctly regardless; see
	// cmd/wardyn-aws-sso/main_test.go's TestParseRealAWSCLICacheFile for the
	// parser-level assertion this exercises.
	if _, perr := time.Parse(time.RFC3339, cache.ExpiresAt); perr != nil {
		t.Errorf("real cache file expiresAt = %q, want a parseable RFC3339 timestamp: %v", cache.ExpiresAt, perr)
	}
}
