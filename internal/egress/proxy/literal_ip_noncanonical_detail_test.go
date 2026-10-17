// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestNonCanonicalLiteralDenialIsNeverLiftable: the step-0 guard refuses every
// non-canonical spelling of a blocked address, so the internal_hosts remedy is
// never true for one. A spelling of a never-liftable address says so; a spelling
// of a liftable one (10.1 is 10.0.0.1) says to write it the standard way.
func TestNonCanonicalLiteralDenialIsNeverLiftable(t *testing.T) {
	pol := CompilePolicy(types.RunPolicySpec{})
	for _, host := range []string{"127.1", "0x7f.1", "fe80::1%25eth0"} {
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
	want := "literal IP 10.1 is a non-standard way to write 10.0.0.1. The proxy refuses every non-standard spelling, " +
		"and no internal_hosts entry lifts that; write the address as 10.0.0.1."
	if got := literalIPDenialDetail("10.1", 443, pol, blockPrivate); got != want {
		t.Errorf("10.1: detail %q, want %q", got, want)
	}
}
