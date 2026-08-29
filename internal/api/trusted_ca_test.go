// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"maps"
	"testing"
)

// TestInstallSandboxTrustedCA_AppendsToExistingMITMCert covers a MITM'd run:
// provisionDispatchMITMCA already staged the per-run interception CA and the
// four bundle vars: installSandboxTrustedCA must APPEND the corp PEM (a
// 2-cert bundle) and leave the already-set bundle vars untouched.
func TestInstallSandboxTrustedCA_AppendsToExistingMITMCert(t *testing.T) {
	env := map[string]string{
		"WARDYN_MITM_CA_PEM":  "run-ca-pem",
		"NODE_EXTRA_CA_CERTS": "/tmp/wardyn/mitm-ca.pem",
		"SSL_CERT_FILE":       "/tmp/wardyn/ca-bundle.pem",
		"REQUESTS_CA_BUNDLE":  "/tmp/wardyn/ca-bundle.pem",
		"CURL_CA_BUNDLE":      "/tmp/wardyn/ca-bundle.pem",
	}
	installSandboxTrustedCA("corp-ca-pem", env)

	if want := "run-ca-pem\ncorp-ca-pem"; env["WARDYN_MITM_CA_PEM"] != want {
		t.Errorf("WARDYN_MITM_CA_PEM = %q, want %q (append, not replace)", env["WARDYN_MITM_CA_PEM"], want)
	}
	for _, k := range []string{"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"} {
		if env[k] != "/tmp/wardyn/mitm-ca.pem" && env[k] != "/tmp/wardyn/ca-bundle.pem" {
			t.Errorf("%s = %q, want unchanged (provisionDispatchMITMCA's paths must not be clobbered)", k, env[k])
		}
	}
}

// TestInstallSandboxTrustedCA_SeedsOnNonMITMRun covers a run with NO Wardyn-
// side MITM: WARDYN_MITM_CA_PEM would otherwise be entirely absent (a single
// cert once this runs), and the four bundle vars must be SEEDED — install_mitm_ca
// (agent-run-lib.sh) is a no-op without WARDYN_MITM_CA_PEM, so without this
// seeding a non-MITM run's OpenSSL-shaped clients would never see the corp CA.
func TestInstallSandboxTrustedCA_SeedsOnNonMITMRun(t *testing.T) {
	env := map[string]string{}
	installSandboxTrustedCA("corp-ca-pem", env)

	if env["WARDYN_MITM_CA_PEM"] != "corp-ca-pem" {
		t.Errorf("WARDYN_MITM_CA_PEM = %q, want %q (a single cert, created)", env["WARDYN_MITM_CA_PEM"], "corp-ca-pem")
	}
	for k, want := range map[string]string{
		"NODE_EXTRA_CA_CERTS": "/tmp/wardyn/mitm-ca.pem",
		"SSL_CERT_FILE":       "/tmp/wardyn/ca-bundle.pem",
		"REQUESTS_CA_BUNDLE":  "/tmp/wardyn/ca-bundle.pem",
		"CURL_CA_BUNDLE":      "/tmp/wardyn/ca-bundle.pem",
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q (seeded so install_mitm_ca actually runs)", k, env[k], want)
		}
	}
}

// TestInstallSandboxTrustedCA_EmptyIsNoOp is the negative control the whole
// feature rests on: WARDYN_TRUSTED_CA_FILE unset must leave sandboxEnv BYTE-
// IDENTICAL, on both a MITM'd run and a non-MITM run.
func TestInstallSandboxTrustedCA_EmptyIsNoOp(t *testing.T) {
	mitmEnv := map[string]string{
		"WARDYN_MITM_CA_PEM":  "run-ca-pem",
		"NODE_EXTRA_CA_CERTS": "/tmp/wardyn/mitm-ca.pem",
		"SSL_CERT_FILE":       "/tmp/wardyn/ca-bundle.pem",
		"REQUESTS_CA_BUNDLE":  "/tmp/wardyn/ca-bundle.pem",
		"CURL_CA_BUNDLE":      "/tmp/wardyn/ca-bundle.pem",
	}
	before := maps.Clone(mitmEnv)
	installSandboxTrustedCA("", mitmEnv)
	if !maps.Equal(before, mitmEnv) {
		t.Errorf("a MITM run's env changed with corpPEM=\"\": got %v, want unchanged %v", mitmEnv, before)
	}

	nonMITMEnv := map[string]string{}
	installSandboxTrustedCA("", nonMITMEnv)
	if len(nonMITMEnv) != 0 {
		t.Errorf("a non-MITM run's env gained a key with corpPEM=\"\": got %v, want empty", nonMITMEnv)
	}
}
