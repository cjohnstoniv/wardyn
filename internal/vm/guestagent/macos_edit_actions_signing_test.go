// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Trust the list's CAs also trusts the guest agent's signing certificate, for code signing only and only
// while this guest does not already trust it; it is never trusted for SSL. With nothing else to trust, a
// trusted signing certificate leaves the request refused.
func TestEditTrustCAsAddsTheSigningCertForCodeSigningOnly(t *testing.T) {
	rig, dir, stage := newEditRig(t)
	stageCA(t, stage, AgentSigningItem, "Wardyn guest agent signing", true)
	verify := []string{"/usr/bin/security", "verify-cert", "-L", "-p", "codeSign", "-c", filepath.Join(stage, AgentSigningItem, ItemCert)}
	trusted := false
	rig.t.run = func(_ context.Context, argv []string) ([]byte, int, error) {
		rig.ran = append(rig.ran, argv)
		if slices.Equal(argv, verify) && !trusted {
			return nil, 1, nil
		}
		return nil, 0, nil
	}
	line := func(name, policy string) string {
		return "/usr/bin/su - " + MacAdminUser + " -c '/usr/bin/sudo /usr/bin/security add-trusted-cert -d -r trustRoot -p " + policy + " -k " +
			MacSystemKeychain + " " + filepath.Join(stage, name, ItemCert) + "'"
	}
	trust := func() (int, []string, int) {
		rig.ran = nil
		w := rig.do("POST", "/edit/trust-cas", "")
		var res EditActionResult
		_ = json.Unmarshal(w.Body.Bytes(), &res)
		if w.Code != 200 {
			return w.Code, nil, 0
		}
		b, err := os.ReadFile(filepath.Join(dir, EditTrustCAsScript))
		if err != nil {
			t.Fatal(err)
		}
		var su []string
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "/usr/bin/su ") {
				su = append(su, l)
			}
		}
		return w.Code, su, res.Certificates
	}

	// Only the signing certificate is staged, and the guest does not trust it.
	if code, su, n := trust(); code != 200 || n != 1 || !slices.Equal(su, []string{line(AgentSigningItem, "codeSign")}) {
		t.Errorf("signing certificate alone, not trusted: %d, %d certificates, su lines %q", code, n, su)
	}
	if len(rig.ran) == 0 || !slices.Equal(rig.ran[0], verify) {
		t.Errorf("ran %q, want the codeSign verify first", rig.ran)
	}
	// Trusted already, with no ca item: nothing to do.
	trusted = true
	if code, _, _ := trust(); code != 409 {
		t.Errorf("signing certificate alone, trusted: %d, want 409", code)
	}
	if len(rig.ran) != 1 {
		t.Errorf("a refused request ran %q, want only the verify", rig.ran)
	}
	// With a ca item: the CA for SSL, and the signing certificate only while it is not trusted.
	stageCA(t, stage, "corp", "Corp Root", false)
	if code, su, n := trust(); code != 200 || n != 1 || !slices.Equal(su, []string{line("corp", "ssl")}) {
		t.Errorf("CA with a trusted signing certificate: %d, %d certificates, su lines %q", code, n, su)
	}
	trusted = false
	if code, su, n := trust(); code != 200 || n != 2 || !slices.Equal(su, []string{line("corp", "ssl"), line(AgentSigningItem, "codeSign")}) {
		t.Errorf("CA with an untrusted signing certificate: %d, %d certificates, su lines %q", code, n, su)
	}
}
