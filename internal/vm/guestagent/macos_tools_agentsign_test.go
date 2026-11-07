// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"errors"
	"testing"
)

// Tools mode trusts and checks the guest agent's signing certificate for code signing, and only that
// certificate: a base item's certificate stays SSL only.
func TestToolsAllowsTrustingOnlyTheAgentSigningCertForCodeSigning(t *testing.T) {
	cert := ItemDir(AgentSigningItem) + "/" + ItemCert
	other := ItemDir("org-root") + "/" + ItemCert
	allowed := [][]string{
		{"/usr/bin/security", "add-trusted-cert", "-d", "-r", "trustRoot", "-p", "codeSign", "-k", MacSystemKeychain, cert},
		{"/usr/bin/security", "verify-cert", "-L", "-p", "codeSign", "-c", cert},
		{"/usr/bin/security", "add-trusted-cert", "-d", "-r", "trustRoot", "-p", "ssl", "-k", MacSystemKeychain, other},
	}
	for _, argv := range allowed {
		if err := ToolsExecAllowed(argv, "/usr/local/bin"); err != nil {
			t.Errorf("ToolsExecAllowed(%q) = %v", argv, err)
		}
	}
	refused := [][]string{
		{"/usr/bin/security", "add-trusted-cert", "-d", "-r", "trustRoot", "-p", "codeSign", "-k", MacSystemKeychain, other},
		{"/usr/bin/security", "verify-cert", "-L", "-p", "codeSign", "-c", other},
		{"/usr/bin/security", "add-trusted-cert", "-d", "-r", "trustRoot", "-p", "basic", "-k", MacSystemKeychain, cert},
		{"/usr/bin/security", "add-trusted-cert", "-d", "-r", "trustRoot", "-k", MacSystemKeychain, cert},
	}
	for _, argv := range refused {
		if err := ToolsExecAllowed(argv, "/usr/local/bin"); !errors.Is(err, ErrToolsRefused) {
			t.Errorf("ToolsExecAllowed(%q) = %v, want ErrToolsRefused", argv, err)
		}
	}
}
