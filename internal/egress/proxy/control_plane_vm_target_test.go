// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"net"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// vmEngineProxy builds a proxy for a laptop runner whose control plane is
// reached at a VM-engine host address (cp). localSubnets is the docker bridge,
// which does NOT contain cp, so the control-plane entry — not the own-subnet
// entry — is the only thing that can close it. vmControlHost and vmControlOKHost resolve to cp
// and to the positive control; the allow list carries the exact literal IPs and the
// hostnames, and a 192.168.0.0/16 InternalHosts lift covers both addresses.
func vmEngineProxy(t *testing.T, cp string, unknown bool) *Proxy {
	t.Helper()
	_, bridge, err := net.ParseCIDR("172.17.0.0/16")
	if err != nil {
		t.Fatalf("parse bridge: %v", err)
	}
	return newProxy(Options{
		RunID: uuid.New(),
		Policy: CompilePolicy(types.RunPolicySpec{
			AllowedDomains: []string{cp, vmControlOK, vmControlHost, vmControlOKHost},
		}),
		Sink: &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 16)},
		Resolver: fakeResolver{m: map[string][]net.IP{
			vmControlHost:   ips(cp),
			vmControlOKHost: ips(vmControlOK),
		}},
		LocalSubnets:     []*net.IPNet{bridge},
		ControlPlaneIPs:  ips(cp),
		InternalHosts:    []types.InternalHost{{HostSuffix: "vm.corp.internal", CIDRs: []string{"192.168.0.0/16"}}},
		ExclusionUnknown: unknown,
	})
}

const (
	vmControlOK     = "192.168.7.7" // positive control: private, in the lift, NOT the control plane
	vmControlHost   = "cp.vm.corp.internal"
	vmControlOKHost = "ok.vm.corp.internal"
)

// TestVMEngineControlPlaneAddressStaysClosedToTheAgent pins that a control
// plane reached at a Lima (192.168.5.2) or Rancher Desktop WSL2 (192.168.143.2)
// host address is closed to the agent's egress on every port, even with an
// exact literal-IP allow and an operator internal-host lift covering it, via
// both spellings (literal and a name that resolves to it). 192.168.7.7 under
// the same allow/lift is admitted, so the denials are the control-plane clamp
// and not a blanket refusal. With the startup capture unknown, nothing is
// admitted.
func TestVMEngineControlPlaneAddressStaysClosedToTheAgent(t *testing.T) {
	for _, cp := range []string{"192.168.5.2", "192.168.143.2"} {
		t.Run(cp, func(t *testing.T) {
			p := vmEngineProxy(t, cp, false)
			ctx := context.Background()

			// (a) literal dial, exact allow, both ports.
			for _, port := range []int{443, 22} {
				if tgt, src, err := p.egressTarget(cp, port); err == nil {
					t.Errorf("egressTarget(%s:%d) = %q via %q, want refused", cp, port, tgt, src)
				}
				if d, _, log := p.evaluate(ctx, cp, port, "CONNECT", ""); d != egress.Deny || log.RuleSource != "builtin:private-ip" {
					t.Errorf("evaluate(%s:%d) = %q rule=%q, want deny by builtin:private-ip", cp, port, d, log.RuleSource)
				}
			}
			// (b) a name resolving to it, under the 192.168.0.0/16 lift.
			for _, port := range []int{443, 22} {
				if tgt, src, err := p.egressTarget(vmControlHost, port); err == nil {
					t.Errorf("egressTarget(%s:%d) = %q via %q, want refused: the lift must not reach the control plane",
						vmControlHost, port, tgt, src)
				}
				if d, _, log := p.evaluate(ctx, vmControlHost, port, "CONNECT", ""); d != egress.Deny {
					t.Errorf("evaluate(%s:%d) = %q rule=%q, want deny", vmControlHost, port, d, log.RuleSource)
				}
			}
			// (c) positive control: same allow and lift, not the control plane.
			for _, port := range []int{443, 22} {
				want := net.JoinHostPort(vmControlOK, strconv.Itoa(port))
				if tgt, src, err := p.egressTarget(vmControlOK, port); err != nil || tgt != want || src != ruleSourceEgressRedirect {
					t.Errorf("egressTarget(%s:%d) = %q/%q/%v, want %s via %q", vmControlOK, port, tgt, src, err, want, ruleSourceEgressRedirect)
				}
				if d, _, log := p.evaluate(ctx, vmControlOK, port, "CONNECT", ""); d != egress.Allow {
					t.Errorf("evaluate(%s:%d) = %q rule=%q, want allow", vmControlOK, port, d, log.RuleSource)
				}
				if _, src, err := p.egressTarget(vmControlOKHost, port); err != nil || src != ruleSourceInternalHost {
					t.Errorf("egressTarget(%s:%d) via %q err=%v, want the internal-host lift to admit it", vmControlOKHost, port, src, err)
				}
				if d, _, log := p.evaluate(ctx, vmControlOKHost, port, "CONNECT", ""); d != egress.Allow {
					t.Errorf("evaluate(%s:%d) = %q rule=%q, want allow", vmControlOKHost, port, d, log.RuleSource)
				}
			}
		})

		t.Run(cp+"/exclusion-unknown", func(t *testing.T) {
			p := vmEngineProxy(t, cp, true)
			ctx := context.Background()
			for _, host := range []string{cp, vmControlOK, vmControlHost, vmControlOKHost} {
				for _, port := range []int{443, 22} {
					if tgt, src, err := p.egressTarget(host, port); err == nil {
						t.Errorf("egressTarget(%s:%d) = %q via %q, want refused (fail closed)", host, port, tgt, src)
					}
					if d, _, log := p.evaluate(ctx, host, port, "CONNECT", ""); d != egress.Deny {
						t.Errorf("evaluate(%s:%d) = %q rule=%q, want deny (fail closed)", host, port, d, log.RuleSource)
					}
				}
			}
		})
	}
}
