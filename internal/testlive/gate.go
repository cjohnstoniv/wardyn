// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package testlive

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The live suites' environment. Values that are secrets are never read from
// the environment: the *_FILE names hold a path to a file outside the repo.
const (
	EnvEntra      = "WARDYN_LIVE_ENTRA"
	EnvADO        = "WARDYN_LIVE_ADO"
	EnvADOWrite   = "WARDYN_LIVE_ADO_WRITE" // LL2b: pushes and deletes one scratch branch
	EnvBedrock    = "WARDYN_LIVE_BEDROCK"
	EnvAWSSSO     = "WARDYN_LIVE_AWS_SSO"
	EnvBaseURL    = "WARDYN_LIVE_BASE_URL"
	EnvIdentities = "WARDYN_LIVE_IDENTITIES_FILE"

	EnvADOOrg     = "WARDYN_LIVE_ADO_ORG"
	EnvADOProject = "WARDYN_LIVE_ADO_PROJECT"
	EnvADORepo    = "WARDYN_LIVE_ADO_REPO"
	// The LL2 project and repository whose names carry a space (#485);
	// optional, defaulting to the fixture names docs/LIVE-TESTS.md lists.
	EnvADOSpacedProject = "WARDYN_LIVE_ADO_SPACED_PROJECT"
	EnvADOSpacedRepo    = "WARDYN_LIVE_ADO_SPACED_REPO"

	// LL2c, the personal-access-token mint probe, on its own throwaway app.
	EnvADOPATProbe       = "WARDYN_LIVE_ADO_PAT_PROBE"
	EnvADOPATProbeTenant = "WARDYN_LIVE_ADO_PAT_PROBE_TENANT_ID"
	EnvADOPATProbeClient = "WARDYN_LIVE_ADO_PAT_PROBE_CLIENT_ID"
	EnvADOPATProbeScope  = "WARDYN_LIVE_ADO_PAT_PROBE_SCOPE"

	EnvSSORegion    = "WARDYN_LIVE_AWS_SSO_REGION"
	EnvSSOTokenFile = "WARDYN_LIVE_AWS_SSO_TOKEN_FILE"

	EnvBedrockAccount  = "WARDYN_LIVE_BEDROCK_ACCOUNT_ID"
	EnvBedrockRole     = "WARDYN_LIVE_BEDROCK_ROLE_NAME"
	EnvBedrockRegion   = "WARDYN_LIVE_BEDROCK_REGION"
	EnvBedrockModel    = "WARDYN_LIVE_BEDROCK_MODEL"
	EnvBedrockMaxCalls = "WARDYN_LIVE_BEDROCK_MAX_CALLS"
)

// Require skips t unless gate is "1"; that is the only condition a skip may
// mean "prove nothing, on purpose". Once the operator has opted in by setting
// gate, a missing name is a broken invocation, not an absence of intent, so it
// is Fatalf, not Skipf — a skip and a pass must never share an exit code on a
// run the operator explicitly asked to prove something (#463). The message
// names the variables and never prints a value.
func Require(t testing.TB, gate string, names ...string) {
	t.Helper()
	if os.Getenv(gate) != "1" {
		t.Skipf("live: needs %s=1 (docs/LIVE-TESTS.md)", gate)
		return
	}
	var unset []string
	for _, n := range names {
		if os.Getenv(n) == "" {
			unset = append(unset, n)
		}
	}
	if len(unset) > 0 {
		t.Fatalf("live: %s=1 but unset: %s (docs/LIVE-TESTS.md)",
			gate, strings.Join(unset, ", "))
	}
}

// repoRoot is the checkout this package was compiled from.
func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// ReadSecretFile reads the file whose path is in env var name. It refuses a
// file inside the checkout, where a stray `git add` would publish it, and its
// errors name the variable, never the path or the contents.
func ReadSecretFile(name string) ([]byte, error) {
	p := os.Getenv(name)
	if p == "" {
		return nil, fmt.Errorf("%s is unset", name)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return nil, fmt.Errorf("%s: unusable path", name)
	}
	if rel, err := filepath.Rel(repoRoot(), abs); err == nil && !strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("%s points inside the repository; keep secret files outside it", name)
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("%s: file not readable", name)
	}
	return b, nil
}

// Identity is one test identity's entry in WARDYN_LIVE_IDENTITIES_FILE. The
// Playwright suites read storage_state; the Go suites read api_token.
type Identity struct {
	StorageState string `json:"storage_state"`
	APIToken     string `json:"api_token"`
}

// LoadIdentities parses WARDYN_LIVE_IDENTITIES_FILE: an object keyed by role
// ("admin", "member", "norole").
func LoadIdentities() (map[string]Identity, error) {
	b, err := ReadSecretFile(EnvIdentities)
	if err != nil {
		return nil, err
	}
	var ids map[string]Identity
	if err := json.Unmarshal(b, &ids); err != nil {
		return nil, fmt.Errorf("%s: not a JSON object of identities", EnvIdentities)
	}
	return ids, nil
}
