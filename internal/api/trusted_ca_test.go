// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"testing"
)

// TestInstallSandboxTrustedCA_AppendsToExistingMITMCert covers a MITM'd run:
// provisionDispatchMITMCA already staged the per-run interception CA and FOUR
// bundle vars (it sets no AWS_CA_BUNDLE): installSandboxTrustedCA must APPEND
// the corp PEM (a 2-cert bundle), leave those four untouched, and still SEED
// the fifth.
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
	// provisionDispatchMITMCA stages no AWS_CA_BUNDLE, so even on a MITM'd run
	// the seeding branch here is what points AWS CLI v2 at the combined bundle.
	if env["AWS_CA_BUNDLE"] != "/tmp/wardyn/ca-bundle.pem" {
		t.Errorf("AWS_CA_BUNDLE = %q, want %q (seeded: provisionDispatchMITMCA never sets it)", env["AWS_CA_BUNDLE"], "/tmp/wardyn/ca-bundle.pem")
	}
}

// TestInstallSandboxTrustedCA_SeedsOnNonMITMRun covers a run with NO Wardyn-
// side MITM: WARDYN_MITM_CA_PEM would otherwise be entirely absent (a single
// cert once this runs), and the five bundle vars must be SEEDED — install_mitm_ca
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
		"AWS_CA_BUNDLE":       "/tmp/wardyn/ca-bundle.pem",
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q (seeded so install_mitm_ca actually runs)", k, env[k], want)
		}
	}
}

// TestInstallSandboxTrustedCA_PinsTheCompleteVarSet is the closed-set pin.
// "Install the CA" is one action PER TOOLCHAIN, and this list drifted once
// already: AWS CLI v2 ships its own Python and its own CA store, reads none of
// the other four, and was missing — so a MITM'd Bedrock/STS call failed in the
// lane the product drives itself. Adding OR removing a variable in
// installSandboxTrustedCA must fail here; that is the whole point. Update this
// list, deploy/images/README.md's per-toolchain table and docs/ENV.md's
// WARDYN_TRUSTED_CA_FILE row together.
func TestInstallSandboxTrustedCA_PinsTheCompleteVarSet(t *testing.T) {
	env := map[string]string{}
	installSandboxTrustedCA("corp-ca-pem", env)

	want := []string{
		"AWS_CA_BUNDLE",       // AWS CLI v2 (own Python + own store)
		"CURL_CA_BUNDLE",      // curl
		"NODE_EXTRA_CA_CERTS", // Node
		"REQUESTS_CA_BUNDLE",  // Python requests
		"SSL_CERT_FILE",       // OpenSSL / Python ssl
		"WARDYN_MITM_CA_PEM",  // the PEM itself, consumed by install_mitm_ca
	}
	if got := slices.Sorted(maps.Keys(env)); !slices.Equal(got, want) {
		t.Errorf("installSandboxTrustedCA set %v, want exactly %v", got, want)
	}

	// AWS_CA_BUNDLE REPLACES the trust store (unlike additive NODE_EXTRA_CA_CERTS),
	// so it must name the COMBINED bundle SSL_CERT_FILE names. "Tidying" it to the
	// bare mitm-ca.pem would silently leave the AWS CLI distrusting every public root.
	if env["AWS_CA_BUNDLE"] != env["SSL_CERT_FILE"] {
		t.Errorf("AWS_CA_BUNDLE = %q, want SSL_CERT_FILE's value %q (the full bundle)", env["AWS_CA_BUNDLE"], env["SSL_CERT_FILE"])
	}
	if env["AWS_CA_BUNDLE"] == env["NODE_EXTRA_CA_CERTS"] {
		t.Errorf("AWS_CA_BUNDLE = %q, the bare cert only additive NODE_EXTRA_CA_CERTS may carry", env["AWS_CA_BUNDLE"])
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

// TestSetupStatus_TrustedCACerts covers the /setup/status console signal:
// the count is derived from Config.TrustedCAPEM, never a second boot field.
func TestSetupStatus_TrustedCACerts(t *testing.T) {
	twoCerts := "-----BEGIN CERTIFICATE-----\ncorpA\n-----END CERTIFICATE-----\n" +
		"-----BEGIN CERTIFICATE-----\ncorpB\n-----END CERTIFICATE-----\n"
	srv := New(Config{LocalMode: true, LocalOperator: "local:test", LocalLoopback: true, TrustedCAPEM: twoCerts})
	w := do(t, srv, http.MethodGet, "/api/v1/setup/status", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	var st SetupStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	if st.TrustedCACerts != 2 {
		t.Errorf("trusted_ca_certs = %d, want 2", st.TrustedCACerts)
	}

	// Negative control: unset Config.TrustedCAPEM omits the key entirely
	// (omitempty) rather than emitting a literal 0 — byte-identical to a
	// SetupStatus built before this field existed.
	srv2 := New(Config{LocalMode: true, LocalOperator: "local:test", LocalLoopback: true})
	w2 := do(t, srv2, http.MethodGet, "/api/v1/setup/status", "", "")
	if w2.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", w2.Code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w2.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v; body=%s", err, w2.Body.String())
	}
	if _, present := raw["trusted_ca_certs"]; present {
		t.Errorf("trusted_ca_certs key present with unset Config.TrustedCAPEM, want absent (omitempty)")
	}
}
