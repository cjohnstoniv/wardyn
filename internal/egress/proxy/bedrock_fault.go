// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// Bedrock data-plane refusals AFTER dispatch: AWS answered a model call the
// proxy allowed and relayed, but refused it — an SCP/IAM deny (403
// AccessDeniedException) or a quota throttle (429 ThrottlingException). Any
// other non-2xx is named "<status> <class>" (such as "400 ValidationException"),
// the class read off the header and omitted when AWS sent none. The proxy never
// alters or retries these; it only names the refusal on the decision row so
// the control plane can say why a run died of it.
const (
	bedrockFaultAccessDenied = "AccessDeniedException"
	bedrockFaultThrottling   = "ThrottlingException"
	// bedrockFaultRecovered marks the first clean model call on a host after a
	// refusal, so a transient throttle does not outlive the retry that cleared it.
	bedrockFaultRecovered = "recovered"
)

// bedrockUpstreamFault returns the value for the decision row's UpstreamFault.
// path is the UPSTREAM request path: every bedrock-runtime operation
// addresses /model/<id>/<op>. The class is read off the status and
// x-amzn-ErrorType header only, so the body is never touched.
func (p *Proxy) bedrockUpstreamFault(host, path string, resp *http.Response) string {
	if !strings.HasPrefix(path, "/model/") {
		return ""
	}
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	class, _, _ := strings.Cut(resp.Header.Get("X-Amzn-Errortype"), ":")
	switch {
	case resp.StatusCode == http.StatusForbidden && class == bedrockFaultAccessDenied,
		resp.StatusCode == http.StatusTooManyRequests && class == bedrockFaultThrottling:
		p.bedrockFaulted.Store(h, true)
		return class
	case resp.StatusCode >= http.StatusBadRequest:
		p.bedrockFaulted.Store(h, true)
		return bedrockRefusal(resp.StatusCode, class)
	case resp.StatusCode < http.StatusBadRequest:
		if _, was := p.bedrockFaulted.LoadAndDelete(h); was {
			return bedrockFaultRecovered
		}
	}
	return ""
}

// bedrockRefusal is "<status> <class>", or the bare status when AWS sent no
// class or one that is not a plain identifier (the value is relayed upstream
// text and lands in a run's failure hint).
func bedrockRefusal(status int, class string) string {
	if len(class) == 0 || len(class) > 64 || strings.ContainsFunc(class, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.')
	}) {
		return strconv.Itoa(status)
	}
	return strconv.Itoa(status) + " " + class
}

// emitLLMAllowWithFault is emitLLMDecision's Allow for a relayed LLM call,
// plus the Bedrock data-plane fault class read off resp (bedrockUpstreamFault)
// — kept here, beside that classifier, rather than in llm_routes.go. callID is the
// attempt's X-Wardyn-Call-Id on a gateway route, "" elsewhere.
func (p *Proxy) emitLLMAllowWithFault(r *http.Request, host string, port int, ruleSource string, scan *egress.ScanSummary, path, callID string, resp *http.Response) {
	if p.sink == nil {
		return
	}
	log := decisionLog(p.reqOf(r, host, port), egress.Allow, ruleSource)
	log.Scan = scan
	log.UpstreamFault = p.bedrockUpstreamFault(host, path, resp)
	log.GatewayCallID = callID
	p.emitDialledAllow(log, host)
}
