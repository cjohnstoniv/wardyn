// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// Bedrock data-plane refusals AFTER dispatch: AWS answered a model call the
// proxy allowed and relayed, but refused it — an AWS Organizations SCP or IAM
// deny (403 AccessDeniedException) or a quota throttle (429
// ThrottlingException). The proxy never alters or retries these (the agent's
// SDK already retries a throttle); it only names the class on the decision row
// so the control plane can say why a run died of it. See
// internal/api/bedrock_dataplane_fault.go for the reader.
const (
	bedrockFaultAccessDenied = "AccessDeniedException"
	bedrockFaultThrottling   = "ThrottlingException"
	// bedrockFaultRecovered marks the first clean model call on a host after a
	// refusal, so a transient throttle does not outlive the retry that cleared it.
	bedrockFaultRecovered = "recovered"
)

// bedrockUpstreamFault returns the value for the decision row's UpstreamFault.
// path is the UPSTREAM request path: every bedrock-runtime operation addresses
// /model/<id>/<op>, which is also what lets a WARDYN_BEDROCK_BASE_URL endpoint
// (a VPC host isBedrockHost does not recognise) count. The class is read off
// the status and x-amzn-ErrorType header only — REST-JSON puts it there, with
// an optional ":<namespace>" suffix — so the body is never touched.
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
	case resp.StatusCode < http.StatusBadRequest:
		if _, was := p.bedrockFaulted.LoadAndDelete(h); was {
			return bedrockFaultRecovered
		}
	}
	return ""
}

// emitLLMAllowWithFault is emitLLMDecision's Allow for a relayed LLM call,
// plus the Bedrock data-plane fault class read off resp (bedrockUpstreamFault)
// — kept here, beside that classifier, rather than in llm_routes.go.
func (p *Proxy) emitLLMAllowWithFault(r *http.Request, host string, port int, ruleSource string, scan *egress.ScanSummary, path string, resp *http.Response) {
	if p.sink == nil {
		return
	}
	log := decisionLog(p.reqOf(r, host, port), egress.Allow, ruleSource)
	log.Scan = scan
	log.UpstreamFault = p.bedrockUpstreamFault(host, path, resp)
	p.sink.emit(log)
}
