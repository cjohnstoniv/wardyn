// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"syscall"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// refusalOfDialError says whose refusal a dial-shaped failure of an ALLOWED host was.
// A peer's explicit "no" is outside Wardyn: the corporate proxy refusing the CONNECT,
// a refused or reset connection. A timeout, a TLS failure, a close and anything else
// leaves no cause Wardyn can read, so the answer is unknown, never a guess at a wall.
//
// ponytail: errno and typed-error checks only; a refusal that arrives as bare text
// reads as unknown until it earns a typed error.
func refusalOfDialError(err error) *egress.Refusal {
	outside := func(reason string) *egress.Refusal {
		return &egress.Refusal{By: egress.RefusedByOutside, Reason: reason}
	}
	unknown := func(reason string) *egress.Refusal {
		return &egress.Refusal{By: egress.RefusedByUnknown, Reason: reason}
	}
	var connectRefused *upstreamConnectRefusedError
	var netErr net.Error
	var opErr *net.OpError
	var notTLS tls.RecordHeaderError
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	switch {
	case errors.As(err, &connectRefused):
		return outside("upstream_proxy_refused")
	case errors.Is(err, syscall.ECONNREFUSED):
		return outside("connection_refused")
	case errors.Is(err, syscall.ECONNRESET):
		return outside("connection_reset")
	case errors.As(err, &opErr) && opErr.Op == "remote error":
		return outside("tls_alert")
	case errors.As(err, &notTLS):
		return outside("upstream_not_tls")
	case errors.As(err, &netErr) && netErr.Timeout():
		return unknown("timed_out")
	case errors.As(err, &unknownAuthority), errors.As(err, &hostname), errors.As(err, &invalid),
		strings.Contains(err.Error(), "tls:"):
		return unknown("tls_failed")
	}
	return unknown("dial_failed")
}

// stampRefusal classifies a deny the caller left unclassified. A deny the policy or a
// guard decided is Wardyn's, and only a policy-decided one carries the layer the run is attributed to; an unresolved
// name is unknown; every other rule_source stays unclassified (egress.WardynRefusal says why).
func (s *decisionSink) stampRefusal(log *egress.DecisionLog) {
	if log.Decision != egress.Deny || log.Refusal != nil {
		return
	}
	if log.RuleSource == "builtin:resolve-failed" {
		log.Refusal = &egress.Refusal{By: egress.RefusedByUnknown, Reason: "resolve_failed"}
		return
	}
	if r := egress.WardynRefusal(log.RuleSource); r != nil {
		if egress.PolicyDecided(r) {
			r.Layer, r.LayerName = s.layer, s.layerName
		}
		log.Refusal = r
	}
}
