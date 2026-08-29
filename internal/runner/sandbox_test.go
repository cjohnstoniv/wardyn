// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/uuid"
)

func TestIsKnownNonVaultRuntime(t *testing.T) {
	nonVault := []string{"runc", "crun", "crun-foo", "sysbox", "sysbox-runc", "runsc", "runsc-kvm"}
	for _, name := range nonVault {
		if !IsKnownNonVaultRuntime(name) {
			t.Errorf("IsKnownNonVaultRuntime(%q) = false, want true", name)
		}
	}
	vault := []string{"kata", "kata-qemu", "krun"}
	for _, name := range vault {
		if IsKnownNonVaultRuntime(name) {
			t.Errorf("IsKnownNonVaultRuntime(%q) = true, want false (must stay CC3-eligible)", name)
		}
	}
}

func TestRecorderArgv(t *testing.T) {
	runID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	agent := []string{"claude", "code", "--task", "fix bug"}
	const outMount = "/wardyn/recordings"

	// Delivery mount set, no upload => -out-dir flag present with the mount target.
	withOut := RecorderArgv("/var/log/wardyn", outMount, "", runID, agent)
	foundOut := false
	for i, a := range withOut {
		if a == "-out-dir" && i+1 < len(withOut) && withOut[i+1] == outMount {
			foundOut = true
		}
	}
	if !foundOut {
		t.Errorf("want -out-dir %s in argv, got %v", outMount, withOut)
	}

	// Enabled => wrapped, agent argv after the "--" separator, run id present,
	// and the brokered upload route passed via -upload-url (default delivery).
	uploadURL := "http://wardyn-proxy:3128/wardyn/v1/recordings/22222222-2222-2222-2222-222222222222"
	got := RecorderArgv("/var/log/wardyn", "", uploadURL, runID, agent)
	if got[0] != "wardyn-rec" {
		t.Fatalf("want wardyn-rec first, got %v", got)
	}
	foundUp := false
	for i, a := range got {
		if a == "-upload-url" && i+1 < len(got) && got[i+1] == uploadURL {
			foundUp = true
		}
	}
	if !foundUp {
		t.Errorf("want -upload-url %s in argv, got %v", uploadURL, got)
	}
	sep := -1
	for i, a := range got {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep == -1 {
		t.Fatalf("missing -- separator: %v", got)
	}
	if !slices.Equal(got[sep+1:], agent) {
		t.Errorf("agent argv after -- = %v, want %v", got[sep+1:], agent)
	}
	if !slices.Contains(got[:sep], runID.String()) {
		t.Errorf("run id must appear before --, got %v", got[:sep])
	}

	// HIGH-finding: both delivery paths offered => the masked upload wins, the
	// unmasked shared-mount -out-dir is dropped (never both landing at once).
	both := RecorderArgv("/var/log/wardyn", outMount, uploadURL, runID, agent)
	if slices.Contains(both, "-out-dir") {
		t.Errorf("masked upload configured: argv must NOT carry an unmasked -out-dir, got %v", both)
	}
	if !slices.Contains(both, "-upload-url") {
		t.Errorf("masked upload configured: argv must keep -upload-url, got %v", both)
	}
}

func TestBuildProxyConfig(t *testing.T) {
	runID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	pc := ProxyConfig{
		RunToken:        "tok",
		ControlPlaneURL: "http://cp:8080",
		MITMLLM:         true,
	}
	b, err := BuildProxyConfig(runID, pc, ProxyListenPort)
	if err != nil {
		t.Fatalf("BuildProxyConfig: %v", err)
	}
	var got struct {
		RunID           string `json:"run_id"`
		RunToken        string `json:"run_token"`
		ControlPlaneURL string `json:"control_plane_url"`
		Listen          string `json:"listen"`
		MITMLLM         bool   `json:"mitm_llm"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("BuildProxyConfig output not valid JSON: %v", err)
	}
	if got.RunID != runID.String() {
		t.Errorf("run_id = %q, want %q", got.RunID, runID.String())
	}
	if got.RunToken != "tok" {
		t.Errorf("run_token = %q, want tok", got.RunToken)
	}
	if got.ControlPlaneURL != "http://cp:8080" {
		t.Errorf("control_plane_url = %q", got.ControlPlaneURL)
	}
	if got.Listen != ":3128" {
		t.Errorf("listen = %q, want :3128", got.Listen)
	}
	if !got.MITMLLM {
		t.Error("mitm_llm = false, want true")
	}
}

// TestBuildProxyConfig_TrustedCAPEM covers the WARDYN_TRUSTED_CA_FILE forward
// leg: ProxyConfig.TrustedCAPEM must reach the marshaled trusted_ca_pem key
// verbatim, and — the negative control — an empty ProxyConfig.TrustedCAPEM
// must leave that key ABSENT (omitempty), never an empty string, so an older
// proxy binary with no field for it round-trips identically.
func TestBuildProxyConfig_TrustedCAPEM(t *testing.T) {
	runID := uuid.MustParse("55555555-5555-5555-5555-555555555555")

	b, err := BuildProxyConfig(runID, ProxyConfig{
		RunToken:        "tok",
		ControlPlaneURL: "http://cp:8080",
		TrustedCAPEM:    "-----BEGIN CERTIFICATE-----\ncorp\n-----END CERTIFICATE-----\n",
	}, ProxyListenPort)
	if err != nil {
		t.Fatalf("BuildProxyConfig: %v", err)
	}
	var got struct {
		TrustedCAPEM string `json:"trusted_ca_pem"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("BuildProxyConfig output not valid JSON: %v", err)
	}
	if want := "-----BEGIN CERTIFICATE-----\ncorp\n-----END CERTIFICATE-----\n"; got.TrustedCAPEM != want {
		t.Errorf("trusted_ca_pem = %q, want %q", got.TrustedCAPEM, want)
	}

	// Negative control: empty TrustedCAPEM omits the key entirely.
	b2, err := BuildProxyConfig(runID, ProxyConfig{RunToken: "tok", ControlPlaneURL: "http://cp:8080"}, ProxyListenPort)
	if err != nil {
		t.Fatalf("BuildProxyConfig: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b2, &raw); err != nil {
		t.Fatalf("BuildProxyConfig output not valid JSON: %v", err)
	}
	if _, present := raw["trusted_ca_pem"]; present {
		t.Errorf("trusted_ca_pem key present with an empty ProxyConfig.TrustedCAPEM, want absent (omitempty)")
	}
}
