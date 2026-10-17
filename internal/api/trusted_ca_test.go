// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

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
