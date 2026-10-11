// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recordmode

import (
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A network-fault row is a destination policy ALLOWED and the connection then
// lost — never a refusal. Record Mode must not read one as a deny (which fails
// a CleanReplay that caught nothing and drops the host from the synthesized
// allowlist as "only DENIED") nor as an open-recording anomaly (which pages an
// operator for a blip). The predicate is the one the deny counter and
// observed-egress promotion read, egress.IsNetworkFault.
func TestCapture_NetworkFaultIsNeitherDenyNorAnomaly(t *testing.T) {
	for _, tc := range []struct {
		source string
		host   string
	}{
		{egress.RuleSourceDialFailed, "dial-lost.example"},
		{egress.RuleSourceTunnelFailed, "tunnel-lost.example"},
	} {
		t.Run(tc.source, func(t *testing.T) {
			// An allow then the fault: the shape the proxy emits for a tunnel
			// that opened and died, and for a request whose dial failed.
			events := []types.AuditEvent{
				egressEvent(egress.Allow, tc.host, "CONNECT", "policy"),
				egressEvent(egress.Deny, tc.host, "CONNECT", tc.source),
			}

			obs := Capture(events, false, KernelWindow{}) // false = an OPEN recording
			if len(obs.Anomalies) != 0 {
				t.Errorf("a network fault raised %v; a lost connection is not an anomaly", obs.Anomalies)
			}
			if len(obs.Domains) != 1 {
				t.Fatalf("want the host still observed from its allow, got %+v", obs.Domains)
			}
			if d := obs.Domains[0]; d.DenyCount != 0 || d.AllowCount != 1 {
				t.Errorf("domain agg = %+v; the fault must not count as a deny", d)
			}
			if !CleanReplay(obs.Domains, false) {
				t.Error("CleanReplay failed on a capture whose only deny was a network fault")
			}

			// The synthesized policy keeps the host: policy allowed it, so it
			// belongs in allowed_domains rather than being dropped as a denial.
			spec, _ := Synthesize(obs, nil, types.AgentRun{ID: uuid.New()})
			if len(spec.AllowedDomains) != 1 || spec.AllowedDomains[0] != tc.host {
				t.Errorf("allowed_domains = %v, want [%s]", spec.AllowedDomains, tc.host)
			}
		})
	}
}

// A fault row on its own — the policy allowed the host and the CONNECT dial
// never reached it, so no allow row exists — must leave no domain behind: the
// run never used the host, and Synthesize naming it would put a host nothing
// reached into the least-privilege allowlist.
func TestCapture_NetworkFaultAloneSynthesizesNothing(t *testing.T) {
	obs := Capture([]types.AuditEvent{
		egressEvent(egress.Deny, "never-reached.example", "CONNECT", egress.RuleSourceDialFailed),
	}, false, KernelWindow{})

	if len(obs.Domains) != 0 {
		t.Errorf("domains = %+v, want none for a host the run never reached", obs.Domains)
	}
	if len(obs.Anomalies) != 0 {
		t.Errorf("anomalies = %v, want none", obs.Anomalies)
	}
	spec, warnings := Synthesize(obs, nil, types.AgentRun{ID: uuid.New()})
	if len(spec.AllowedDomains) != 0 {
		t.Errorf("allowed_domains = %v, want none", spec.AllowedDomains)
	}
	for _, w := range warnings {
		if containsSubstr([]string{w}, "never-reached.example") {
			t.Errorf("a network-fault host was named in synthesis: %q", w)
		}
	}
}

// The classification is one shared predicate, not a copy per consumer: the
// sources Record Mode skips are exactly the ones the deny counter excludes.
func TestIsNetworkFaultCoversExactlyTheSkippedSources(t *testing.T) {
	for _, src := range []string{egress.RuleSourceDialFailed, egress.RuleSourceTunnelFailed} {
		if !egress.IsNetworkFault(src) {
			t.Errorf("IsNetworkFault(%q) = false; a network fault must be skipped", src)
		}
	}
	// Refusals stay refusals — each of these is deliberately counted.
	for _, src := range []string{"policy", "builtin:private-ip", "builtin:resolve-failed", ""} {
		if egress.IsNetworkFault(src) {
			t.Errorf("IsNetworkFault(%q) = true; a refusal is not a network fault", src)
		}
	}
}
