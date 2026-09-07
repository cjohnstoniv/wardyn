// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// upstreamAllowAllProxy builds the exact shape F105 is about: a corp upstream
// configured (so egressTarget hands the destination to the corp proxy BY NAME,
// unresolved and unpinned) with allow_all_egress, so nothing but the
// unconditional literal-IP guard stands between the sandbox and the target.
func upstreamAllowAllProxy(t *testing.T) *Proxy {
	t.Helper()
	up, err := parseUpstreamProxy("http://corp-proxy.internal:3128")
	if err != nil {
		t.Fatalf("parseUpstreamProxy: %v", err)
	}
	return newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(types.RunPolicySpec{AllowAllEgress: true}),
		Sink:     &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 64)},
		Resolver: publicResolver{},
		Upstream: up,
	})
}

// TestNonCanonicalLiteralIPIsDeniedLikeItsCanonicalSpelling pins the bound
// THREAT-MODEL.md states for the corp-upstream relaxation: "a literal
// private/loopback/link-local/metadata IP is still denied at the literal-IP
// guard". net.ParseIP accepts ONLY the canonical dotted-quad, while inet_aton
// — and therefore glibc getaddrinfo, and therefore the corp proxy that
// actually dials — also reads 127.1, 0x7f000001, 2130706433 and 0251.0376.0.1
// as 127.0.0.1. Every one of those spellings used to walk past step 0 as an
// ordinary "hostname" and be handed to the operator's proxy verbatim.
func TestNonCanonicalLiteralIPIsDeniedLikeItsCanonicalSpelling(t *testing.T) {
	p := upstreamAllowAllProxy(t)

	blocked := []struct{ host, why string }{
		{"127.1", "dotted-double loopback"},
		{"127.0.1", "dotted-triple loopback"},
		{"0x7f000001", "hexadecimal loopback"},
		{"2130706433", "bare 32-bit loopback"},
		{"0251.0376.0.1", "octal 169.254.0.1 (link-local)"},
		{"0xa9fea9fe", "hexadecimal 169.254.169.254 (cloud metadata)"},
		{"2852039166", "bare 32-bit 169.254.169.254 (cloud metadata)"},
		{"0xa000001", "hexadecimal 10.0.0.1 (RFC1918)"},
		{"167772161", "bare 32-bit 10.0.0.1 (RFC1918)"},
	}
	for _, tc := range blocked {
		t.Run(tc.host, func(t *testing.T) {
			d, target, _ := p.evaluate(context.Background(), tc.host, 443, http.MethodConnect, "")
			if d != egress.Deny {
				t.Fatalf("evaluate(%q) [%s] = %v target=%q, want Deny: every inet_aton spelling of a "+
					"blocked address must be denied at the literal-IP guard, or the corp proxy resolves it for us",
					tc.host, tc.why, d, target)
			}
			if _, _, err := p.egressTarget(tc.host, 443); err == nil {
				t.Fatalf("egressTarget(%q) [%s] returned no error: serveMITMRequest and the two brokers "+
					"reach egressTarget WITHOUT going through evaluate, so the guard has to hold here too",
					tc.host, tc.why)
			}
		})
	}

	// Canonical control: unchanged behaviour, both spellings of the same address
	// must agree.
	for _, host := range []string{"127.0.0.1", "169.254.169.254", "10.0.0.1"} {
		if d, _, _ := p.evaluate(context.Background(), host, 443, http.MethodConnect, ""); d != egress.Deny {
			t.Fatalf("evaluate(%q) = %v, want Deny (canonical control)", host, d)
		}
	}

	// NEGATIVE control — the guard must not over-deny. A non-canonical spelling
	// of a PUBLIC address, and an ordinary hostname, stay reachable.
	for _, host := range []string{"134744072" /* 8.8.8.8 */, "0x8080808" /* 8.8.8.8 */, "api.example.com"} {
		if d, _, _ := p.evaluate(context.Background(), host, 443, http.MethodConnect, ""); d != egress.Allow {
			t.Fatalf("evaluate(%q) = %v, want Allow: only BLOCKED addresses may be refused by this guard", host, d)
		}
	}
}
