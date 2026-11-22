// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net"
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// The denial a refused request gets: its detail sentence and the 403 both forward paths write.
// Split out of policy.go for scripts/check-file-size.sh; the security invariants in policy.go's header govern it.

// clampDetail is the denial sentence for an address onOwnSubnetOrControlPlane refused, or "" for any
// other kind: the internal_hosts and allowed_domains remedies are wrong for it, since neither lifts it.
func clampDetail(subject string, kind blockKind) string {
	switch kind {
	case blockOwnNetwork:
		return subject + " is on this proxy's own network or is the control plane's address. A sandbox never " +
			"reaches the proxy's neighbours, so no internal_hosts entry, allowed_domains entry or egress_redirect " +
			"lifts it, however it is declared; the service has to be reachable at an address off those networks " +
			"(OPERATIONS.md, internal_hosts)."
	case blockExclusionUnknown:
		return subject + " is a private address, and this proxy could not capture its own network or the " +
			"control plane's address when it started, so for this run it lifts no internal_hosts entry and " +
			"trusts no private literal, however they are declared (OPERATIONS.md, internal_hosts). The sidecar's " +
			"log names the failed capture; a new run tries it again."
	}
	return ""
}

// neverLiftableDetail composes the never-liftable sentence for `subject` ("this
// host's address", or "literal IP 10.0.0.1"). Returns "" when kind IS liftable,
// so the single caller can fall through to the remedy text.
func neverLiftableDetail(subject string, kind blockKind) string {
	class, ok := neverLiftableClass[kind]
	if !ok {
		return ""
	}
	return subject + " is " + class + ", " + egressNeverLiftableRemedy
}

// literalIPDenialDetail is egressHeaderDetail's value for a builtin:private-ip
// refusal of host: which of the three causes fired, and the one place to fix
// it. Returns "" when host is neither a literal IP nor a hostname.
//
// kind is the GUARD CLASS that refused, carried out of the vet rather than
// re-derived (re-deriving for a hostname means resolving it a second time,
// the work B6's memo exists to avoid). blockPrivate (the only liftable class)
// gets the four-sentence remedy verbatim; every other class gets
// neverLiftableDetail, and the clamp's kinds get clampDetail. A LITERAL host
// re-derives its range and reads only the clamp's kinds from the parameter.
func literalIPDenialDetail(host string, port int, pol *Policy, kind blockKind, hint string) string {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "" {
		return ""
	}
	ip := net.ParseIP(h)
	if ip == nil {
		// A non-canonical spelling (inet_aton form, zoned IPv6) is refused by the
		// step-0 guard whatever its range, so the internal_hosts remedy is never true
		// for it. Where the decoded address is in a liftable range, the way out is to
		// write it canonically; where it is never liftable, that is the sentence.
		if dec := nonCanonicalLiteralIP(h); dec != nil {
			if kind == blockHardDeny {
				return neverLiftableDetail("literal IP "+h+", which decodes to "+dec.String()+",", kind)
			}
			if k, _ := isBlockedIP(dec); neverLiftableClass[k] == "" {
				return "literal IP " + h + " is a non-standard way to write " + dec.String() + ". The proxy refuses every non-standard spelling, " +
					"and no internal_hosts entry lifts that; write the address as " + dec.String() + "."
			}
			return "literal IP " + h + " decodes to " + dec.String() + ", " + egressNeverLiftableRemedy
		}
		if clamped := clampDetail("this host's address", kind); clamped != "" {
			return clamped
		}
		if never := neverLiftableDetail("this host's address", kind); never != "" {
			return never
		}
		// The join site for the four sentences above: cause, remedy,
		// hint, lifetime. Any wording change is an edit to this expression and
		// the constant it names, nowhere else. A refusal that knows the host
		// and the range it resolved into (hint, internalHostsLiftHint) names
		// both instead of the generic remedy and its "leave cidrs empty"
		// caution: the range is what the sandbox side resolved, the evidence
		// that caution asks for.
		if hint != "" {
			return egressPrivateRangeCause + "; " + hint + ". " + egressDenialSuffix
		}
		return egressPrivateRangeCause + "; " + egressInternalHostsRemedy + ". " +
			siteInternalHostsCIDRHint + " " + egressDenialSuffix
	}
	if kind == blockHardDeny {
		return neverLiftableDetail("literal IP "+ip.String(), kind)
	}
	if pol != nil {
		if _, denied := pol.deniedExact[ip.String()]; denied {
			return "literal IP " + ip.String() + " is on denied_domains, and a deny always beats an allow"
		}
		if _, denied := pol.deniedExactPort[hostPortKey(ip.String(), port)]; denied {
			return "literal IP " + ip.String() + " is on denied_domains for this port, and a deny always beats an allow"
		}
	}
	if clamped := clampDetail("literal IP "+ip.String(), kind); clamped != "" {
		return clamped
	}
	// Only blockPrivate is reachable by an EXACT allowed_domains entry, so
	// telling a 127.0.0.1 / 169.254.169.254 / NAT64 literal to list itself
	// there describes a fix that cannot work. Re-derived here: the address is
	// the host.
	litKind, _ := isBlockedIP(ip)
	if never := neverLiftableDetail("literal IP "+ip.String(), litKind); never != "" {
		return never
	}
	return "literal IP " + ip.String() + " is in a private/reserved range, so only an EXACT allowed_domains entry for that address can reach it — " +
		"it is not listed; an egress_redirects \"to\" pointing at this address adds that entry automatically for the runs it covers"
}

// resolveFailedDetail is egressHeaderDetail's value for a builtin:resolve-failed
// refusal: the proxy never learned an address, so nothing was vetted and
// nothing about policy or the address-range guard explains the deny. A FIXED
// sentence like the ones above, never echoing the host.
//
// Exists so a resolver outage is never labelled builtin:private-ip with
// literalIPDenialDetail's "declare it under internal_hosts" advice attached —
// advice that can't fix a resolver outage and points at loosening an SSRF
// control for a fault that is neither.
const resolveFailedDetail = "this host did not resolve (DNS failure, no such name, or no address records), so no address " +
	"could be vetted; this is a name-resolution fault, not the private-address guard — check the sandbox's resolver, " +
	"not the allowlist"

// writeEgressDeny writes the 403 both forward paths (handlePlain,
// handleConnect) give a DENIED request: the static refusal headers, plus — for
// the one refusal an operator reliably misreads — the cause and where to fix
// it. Every other refusal reason already says all there is to say. The body is
// JSON, {"wardyn":{"reason":"egress_denied","host":...,"rule_source":...}}, and
// X-Wardyn-Reason repeats the reason for a CONNECT client that drops the body.
//
// Lives here rather than in proxy.go beside its two callers so it sits with
// the rule it explains (and keeps proxy.go under the 1000-line split gate).
//
// memoed is the CALLER's declaration that this refusal can have come out of the
// per-run private-IP memo — true at the two sites that hand on an evaluate()
// verdict, false at a lane that built the deny log itself.
func (p *Proxy) writeEgressDeny(w http.ResponseWriter, host string, port int, log *egress.DecisionLog, memoed bool) {
	p.denyEgress(w, host, port, log, memoed, shapePlain)
}

// writeEgressDenyLLM is writeEgressDeny for a request on an LLM lane (a
// brokered /wardyn/llm/* route, or an inner request of a terminated tunnel):
// the body is shape (shapeOf: the route's, else the host's; an AWS host's is a
// 403 AccessDeniedException), so the SDK reads the refusal instead of failing
// to parse text. shapePlain still gets the forward-lane body.
func (p *Proxy) writeEgressDenyLLM(w http.ResponseWriter, shape errShape, host string, port int, log *egress.DecisionLog) {
	p.denyEgress(w, host, port, log, false, shape)
}

func (p *Proxy) denyEgress(w http.ResponseWriter, host string, port int, log *egress.DecisionLog, memoed bool, shape errShape) {
	detail := "egress denied by policy"
	reason := decisionReason(log)
	// An evaluate() DENY carrying no decision log is B6's memoed private-ip
	// refusal: evaluate() answered an identical repeat out of the per-run
	// memo, so the 403 has to be rebuilt here to stay byte-identical to the
	// first one, retry header included.
	//
	// TWO facts, both required, neither inferred from the other: the CALLER
	// says the verdict could have come from the memo (memoed), and the MEMO
	// says it did for this host:port. policy_test.go pins both negatives and
	// the positive.
	if memoed && reason == "" && p.privateIPMemoed(host, port) {
		reason = "builtin:private-ip"
	}
	switch reason {
	case "builtin:private-ip":
		w.Header().Set(egressHeaderRetry, egressRetryNever)
		kind, hint := p.privateIPBlockKind(host, port)
		if d := literalIPDenialDetail(host, port, p.policy, kind, hint); d != "" {
			w.Header().Set(egressHeaderDetail, d)
			detail = "egress denied: " + d
		}
	case "builtin:resolve-failed":
		w.Header().Set(egressHeaderDetail, resolveFailedDetail)
		detail = "egress denied: " + resolveFailedDetail
	case ruleSourceIPSpelling:
		w.Header().Set(egressHeaderDetail, ipSpellingDetail)
		detail = "egress denied: " + ipSpellingDetail
	case ruleSourceNonASCIIHost:
		w.Header().Set(egressHeaderDetail, nonASCIIHostDetail)
		detail = "egress denied: " + nonASCIIHostDetail
	case ruleSourcePort:
		w.Header().Set(egressHeaderDetail, portRuleDetail)
		detail = "egress denied: " + portRuleDetail
	case ruleSourceGitPushDenyAllForge:
		detail = pushDenyAllForgeRefusal(host)
		w.Header().Set(egressHeaderDetail, detail)
	}
	headerHost := host
	if shape != shapePlain {
		headerHost, _ = p.gatewayFront(shape, host) // a gateway's own address never reaches the sandbox
	}
	setEgressRefusalHeadersWithReason(w, egressRefusalDenied, headerHost, reason)
	attribution := ""
	if policyDecidedReason(reason) {
		attribution = p.attributeRefusal(w)
	}
	if shape != shapePlain {
		// The SDK shows the message, never the headers, so the attribution rides in it.
		msg := brokeredMessage(reasonEgressDenied, p.hostPortLabel(shape, host, port), reason, faultHint(host, reasonEgressDenied, reason, nil), "")
		if attribution != "" {
			msg += ". " + attribution
		}
		writeProviderError(w, shape, http.StatusForbidden, reasonEgressDenied, msg, nil)
		return
	}
	body := map[string]string{"reason": reasonEgressDenied, "host": host, "rule_source": reason, "detail": detail}
	if attribution != "" {
		body["attribution"] = attribution
	}
	w.Header().Set(egressHeaderWardynReason, reasonEgressDenied)
	writeJSONStatus(w, http.StatusForbidden, "application/json", map[string]any{"wardyn": body})
}
