// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestNonCanonicalLiteralDenialIsNeverLiftable: the step-0 guard refuses every
// non-canonical spelling of a blocked address unconditionally, so the
// internal_hosts remedy is never true for one, even when the decoded address is
// in a liftable range (10.1 is 10.0.0.1).
func TestNonCanonicalLiteralDenialIsNeverLiftable(t *testing.T) {
	pol := CompilePolicy(types.RunPolicySpec{})
	for _, host := range []string{"127.1", "0x7f.1", "10.1", "fe80::1%25eth0"} {
		got := literalIPDenialDetail(host, 443, pol, blockPrivate)
		if !strings.Contains(got, egressNeverLiftableRemedy) {
			t.Errorf("%s: detail %q lacks the never-liftable sentence", host, got)
		}
		if !strings.HasPrefix(got, "literal IP "+host+" decodes to ") {
			t.Errorf("%s: detail %q is not a sentence that names what the spelling decodes to", host, got)
		}
		if strings.Contains(got, egressInternalHostsRemedy) {
			t.Errorf("%s: detail %q offers the internal_hosts remedy", host, got)
		}
	}
}
