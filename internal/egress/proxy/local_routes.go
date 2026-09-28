// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Brokered LOCAL routes served by the proxy listener itself (origin-form
// only; see ServeHTTP for the security gating). SECURITY: they let the
// sandbox obtain broker-minted credentials WITHOUT ever holding the run
// token; the sandbox-supplied Authorization header is always stripped before
// injection so the sandbox can't smuggle or replace brokered credentials.
const (
	localRoutePrefix = "/wardyn/"

	routeMint = "/wardyn/v1/credentials/mint"
	// routeApprovalsCreate (POST, exact path) raises a tool_call hold from the
	// sandbox; routeApprovals (GET, {id} suffix) polls one.
	routeApprovalsCreate = "/wardyn/v1/approvals"
	routeApprovals       = "/wardyn/v1/approvals/"
	// routeApprovalsExpireSuffix (POST, {id}+suffix) is wardyn-toolgate's own
	// give-up signal: closes the approval it raised at its wait deadline
	// instead of leaving the row PENDING for the periodic sweep.
	routeApprovalsExpireSuffix = "/expire"
	routeRecordings            = "/wardyn/v1/recordings/"
	routeScanResults           = "/wardyn/v1/scan-results/"
	// routeSSOToken carries the AWS SSO session captured by an `aws sso login`
	// container-login run. Same brokered shape as the scan/verify result uploads.
	routeSSOToken = "/wardyn/v1/sso-token/"

	// rule_source values emitted for the brokered routes (audit pipeline).
	ruleSourceMint        = "brokered:mint"
	ruleSourceApprovals   = "brokered:approvals"
	ruleSourceRecordings  = "brokered:recording"
	ruleSourceScanResults = "brokered:scan-result"
	// A tool call decided by the run's own tool_rules, with no human asked.
	ruleSourceToolAllow = "policy:tool-allow"
	ruleSourceToolDeny  = "policy:tool-deny"
	ruleSourceSSOToken  = "brokered:sso-token"
	// ruleSourceArtifactMITM marks a corp artifact-registry request TLS-MITM'd
	// only to inject the operator's registry token — not an inspection path.
	ruleSourceArtifactMITM = "artifact:mitm"

	// ruleSourceCredentialReauthTimeout: the proxy parked a sandbox's AWS SSO
	// credential exchange while its owner was asked to sign in again, and
	// nobody did before the budget ended. Its own rule_source since "nobody
	// signed in" and "the credential couldn't be refreshed" have different fixes.
	//
	// Appended, not slotted beside its siblings: docs/AUDIT-ACTIONS.md cites
	// each by LINE, so inserting above rots citations.
	ruleSourceCredentialReauthTimeout = "credential:reauth-timeout"

	// maxBrokeredBody caps mint/approvals forward bodies. LLM bodies are
	// unbounded (streamed) — Anthropic is the size authority there.
	maxBrokeredBody = 10 << 20 // 10 MiB
	// maxRecordingBody caps recording uploads (100 MiB is generous for a session).
	maxRecordingBody = 100 << 20
	// maxScanResultBody caps scan-result uploads, a generous DoS ceiling.
	maxScanResultBody = 8 << 20
	// Bounds for a sandbox-raised tool_call approval: the body cap is a DoS
	// ceiling, the field caps bound what is shown to a human.
	maxToolApprovalBody = 64 << 10
	maxToolCmd          = 4 << 10
	maxToolName         = 128
	maxToolEnvNames     = 32
)

// handleLocalRoute dispatches an origin-form /wardyn/... request to its
// brokered handler. Unknown /wardyn/... paths are 404.
func (p *Proxy) handleLocalRoute(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == routeMint:
		p.handleBrokerMint(w, r)
	case r.Method == http.MethodPost && path == routeApprovalsCreate:
		p.handleBrokerCreateApproval(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, routeApprovals):
		p.handleBrokerApproval(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(path, routeApprovals) && strings.HasSuffix(path, routeApprovalsExpireSuffix):
		p.handleBrokerExpireApproval(w, r)
	case r.Method == http.MethodPut && strings.HasPrefix(path, routeRecordings):
		p.handleBrokerRecording(w, r)
	case r.Method == http.MethodPut && strings.HasPrefix(path, routeScanResults):
		p.handleBrokerScanResult(w, r)
	case r.Method == http.MethodPut && strings.HasPrefix(path, routeSSOToken):
		p.handleBrokerSSOToken(w, r)
	case strings.HasPrefix(path, llmAnthropicPrefix):
		p.handleLLMAnthropic(w, r)
	case strings.HasPrefix(path, llmOpenAIPrefix):
		p.handleLLMOpenAI(w, r)
	case strings.HasPrefix(path, routeGitBroker):
		p.handleGitBroker(w, r)
	case strings.HasPrefix(path, routePATBroker):
		p.handlePATBroker(w, r)
	default:
		http.Error(w, "unknown brokered route", http.StatusNotFound)
	}
}

// handleBrokerMint forwards POST /wardyn/v1/credentials/mint to the control
// plane's internal mint endpoint with the run token injected. The response
// (status + body) is passed through verbatim.
//
// TRUST BOUNDARY: a grant this proxy brokers on /wardyn/gh/ is REFUSED here
// (see isBrokeredGitGrant) — that route mints the GitHub App installation
// token server-side, so handing the same token to the sandbox would defeat
// the per-repo allowlist and burn the broker's one (single-use) mint.
func (p *Proxy) handleBrokerMint(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBrokeredBody))
	if err != nil {
		http.Error(w, "read request body", http.StatusBadRequest)
		return
	}
	if p.isBrokeredGitGrant(body) {
		p.emitLocalDecision(r, egress.Deny, ruleSourceMint, nil)
		http.Error(w, "wardyn: this grant is brokered on "+routeGitBroker+
			"; the GitHub App installation token is minted proxy-side and never enters the sandbox",
			http.StatusForbidden)
		return
	}
	// The 409 body carries the approval id; a 200 mint body is not parsed for one.
	p.relayControlPlane(w, r, http.MethodPost, "/api/v1/internal/credentials/mint",
		body, r.Header.Get("Content-Type"), ruleSourceMint, approvalIDOn409)
}

// handleBrokerApproval forwards GET /wardyn/v1/approvals/{id} to the control
// plane's internal approval endpoint with the run token injected.
func (p *Proxy) handleBrokerApproval(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, routeApprovals)
	// Only a bare UUID segment is valid: parsing makes the forward path
	// structurally traversal-proof regardless of URL re-parsing behavior.
	if _, err := uuid.Parse(id); err != nil {
		http.Error(w, "invalid approval id", http.StatusNotFound)
		return
	}
	p.relayControlPlane(w, r, http.MethodGet, "/api/v1/internal/approvals/"+id,
		nil, "", ruleSourceApprovals, nil)
}

// handleBrokerExpireApproval forwards POST /wardyn/v1/approvals/{id}/expire
// to the control plane's internal expire endpoint with the run token
// injected — wardyn-toolgate's own give-up signal.
func (p *Proxy) handleBrokerExpireApproval(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, routeApprovals), routeApprovalsExpireSuffix)
	if _, err := uuid.Parse(id); err != nil {
		http.Error(w, "invalid approval id", http.StatusNotFound)
		return
	}
	p.relayControlPlane(w, r, http.MethodPost, "/api/v1/internal/approvals/"+id+"/expire",
		nil, "", ruleSourceApprovals, nil)
}

// toolApprovalRequest is the SANDBOX-facing body for POST /wardyn/v1/approvals.
//
// SECURITY: Env is a list of variable NAMES by type — the wire can't carry a
// value, so no code path could persist one; {"env":{"AWS_SECRET":"…"}} gets a
// 400, not a stored secret.
type toolApprovalRequest struct {
	Kind    string `json:"kind"`
	Payload struct {
		Tool string   `json:"tool"`
		Cmd  string   `json:"cmd"`
		Env  []string `json:"env,omitempty"`
	} `json:"payload"`
}

// toolCallScope is the requested_scope persisted for a tool_call approval,
// what the approvals screen reads. env is the joined name list, a display
// string, never values.
type toolCallScope struct {
	Tool string `json:"tool"`
	Cmd  string `json:"cmd"`
	Env  string `json:"env,omitempty"`
}

// handleBrokerCreateApproval forwards POST /wardyn/v1/approvals to the
// control plane's internal approval endpoint with the run token injected —
// the sandbox-facing alias the tool-approval gate uses to park a tool call
// for a human decision.
//
// TRUST BOUNDARY: the run identity is never sandbox input — it rides the run
// token this proxy holds, which the control plane binds from its verified
// claims.
//
// Only kind "tool_call" is accepted: egress holds are raised by the PROXY
// itself (approvals.go raise()), never the sandbox, since a sandbox-minted
// egress_domain approval could open a hold for a host it was never allowed to
// reach.
func (p *Proxy) handleBrokerCreateApproval(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxToolApprovalBody))
	var body toolApprovalRequest
	if err != nil || json.Unmarshal(raw, &body) != nil {
		http.Error(w, "invalid tool approval request", http.StatusBadRequest)
		return
	}
	// `lane` names a control-plane-raised escalation. The typed re-marshal
	// below would drop it anyway, but a sandbox that tries to say it is
	// refused out loud rather than silently cleaned.
	if sandboxNamesLane(raw) {
		p.emitLocalDecision(r, egress.Deny, ruleSourceApprovals, nil)
		http.Error(w, "wardyn: a tool approval may not name a lane", http.StatusBadRequest)
		return
	}
	if body.Kind != string(types.ApprovalToolCall) {
		p.emitLocalDecision(r, egress.Deny, ruleSourceApprovals, nil)
		http.Error(w, `wardyn: this route raises kind "tool_call" only; egress holds are raised proxy-side`,
			http.StatusBadRequest)
		return
	}
	scope := toolCallScope{
		Tool: clampToolField(body.Payload.Tool, maxToolName),
		Cmd:  clampToolField(body.Payload.Cmd, maxToolCmd),
		Env:  envNames(body.Payload.Env),
	}
	// Neither tool nor command asks a human to decide about nothing.
	if scope.Tool == "" && scope.Cmd == "" {
		http.Error(w, "tool approval needs a tool or a cmd", http.StatusBadRequest)
		return
	}
	if handled := p.decideByToolRules(w, r, scope.Tool); handled {
		return
	}
	fwd, err := json.Marshal(struct {
		Kind           string        `json:"kind"`
		RequestedScope toolCallScope `json:"requested_scope"`
	}{Kind: string(types.ApprovalToolCall), RequestedScope: scope})
	if err != nil {
		p.httpError(w, "encode tool approval", err, http.StatusInternalServerError)
		return
	}
	// The created row's id rides the decision log so audit can join it to the approval.
	p.relayControlPlane(w, r, http.MethodPost, "/api/v1/internal/approvals",
		fwd, "application/json", ruleSourceApprovals, approvalIDAlways)
}

// sandboxNamesLane reports whether the body, or its payload, carries a `lane`
// key in any letter case (encoding/json matches keys case-insensitively).
func sandboxNamesLane(raw []byte) bool {
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil {
		return false
	}
	for k, v := range top {
		if strings.EqualFold(k, "lane") {
			return true
		}
		if strings.EqualFold(k, "payload") {
			var inner map[string]json.RawMessage
			if json.Unmarshal(v, &inner) == nil {
				for ik := range inner {
					if strings.EqualFold(ik, "lane") {
						return true
					}
				}
			}
		}
	}
	return false
}

// clampToolField bounds a sandbox-supplied string for storage and marks any
// truncation honestly, since a human decides on what this renders.
// ToValidUTF8 drops the partial rune a byte-slice cut can leave behind.
func clampToolField(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return strings.ToValidUTF8(s[:max], "") + "… (truncated)"
}

// envNames renders the sandbox-supplied env variable NAMES for display, bounded
// in count and length. Values never reach here (see toolApprovalRequest.Env).
func envNames(names []string) string {
	if len(names) > maxToolEnvNames {
		names = names[:maxToolEnvNames]
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n = clampToolField(n, maxToolName); n != "" {
			out = append(out, n)
		}
	}
	return strings.Join(out, ", ")
}

// handleBrokerRecording forwards PUT /wardyn/v1/recordings/{runID} to the
// control plane's internal recording-upload endpoint with the run token
// injected. The control plane rejects cross-run uploads, so the sandbox can
// deliver ONLY its own cast — the multi-node-safe replacement for the shared
// volume (which leaked recordings across same-uid agent containers).
func (p *Proxy) handleBrokerRecording(w http.ResponseWriter, r *http.Request) {
	p.forwardBrokeredUpload(w, r, routeRecordings, "/api/v1/internal/recordings/",
		ruleSourceRecordings, "read recording body", maxRecordingBody)
}

// forwardBrokeredUpload is the shared PUT-upload path for the brokered
// recording/scan-result/verify-result routes. The sandbox query string is
// deliberately NOT forwarded — the run→workspace linkage comes from trusted
// control-plane state, never sandbox input.
func (p *Proxy) forwardBrokeredUpload(w http.ResponseWriter, r *http.Request, prefix, cpPathPrefix, ruleSource, readErrMsg string, maxBody int64) {
	id := strings.TrimPrefix(r.URL.Path, prefix)
	if _, err := uuid.Parse(id); err != nil {
		http.Error(w, "invalid run id", http.StatusNotFound)
		return
	}
	// Read past the cap to distinguish a complete body from a truncated prefix.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		http.Error(w, readErrMsg, http.StatusBadRequest)
		return
	}
	if int64(len(body)) > maxBody {
		p.emitLocalDecision(r, egress.Deny, ruleSource, nil)
		http.Error(w, "upload exceeds size limit", http.StatusRequestEntityTooLarge)
		return
	}
	p.relayControlPlane(w, r, http.MethodPut, cpPathPrefix+id, body,
		r.Header.Get("Content-Type"), ruleSource, nil)
}

// handleBrokerScanResult forwards PUT /wardyn/v1/scan-results/{runID} to the
// control plane's internal scan-result-upload endpoint, the exact sibling of
// handleBrokerRecording. TRUST BOUNDARY: a governed scan run can deliver ONLY
// its own facts, cross-run uploads being control-plane rejected.
func (p *Proxy) handleBrokerScanResult(w http.ResponseWriter, r *http.Request) {
	p.forwardBrokeredUpload(w, r, routeScanResults, "/api/v1/internal/scan-results/",
		ruleSourceScanResults, "read scan result body", maxScanResultBody)
}

// handleBrokerSSOToken forwards PUT /wardyn/v1/sso-token/{runID} to the
// control plane's internal sso-token endpoint, the exact sibling of
// handleBrokerScanResult. Carries the AWS SSO session wardyn-aws-sso captured
// in the sandbox back to the control plane.
func (p *Proxy) handleBrokerSSOToken(w http.ResponseWriter, r *http.Request) {
	p.forwardBrokeredUpload(w, r, routeSSOToken, "/api/v1/internal/sso-token/",
		ruleSourceSSOToken, "read sso token body", maxScanResultBody)
}

// forwardToControlPlane builds and sends a request to the control plane,
// injecting the run token as Authorization. The control-plane host is
// resolved and IP-vetted once, pinned on the request context so the shared
// transport dials it directly. SECURITY: the inbound sandbox Authorization is
// never carried here — constructed fresh with only the run token set.
func (p *Proxy) forwardToControlPlane(ctx context.Context, method, path string, body []byte, contentType string) (*http.Response, error) {
	if p.controlPlaneURL == "" {
		return nil, fmt.Errorf("control plane url not configured")
	}
	// TRUST BOUNDARY: the control-plane URL is TRUSTED operator configuration,
	// not an agent-chosen target, and legitimately resolves to a
	// private-network address — the agent-SSRF private-IP guard must NOT
	// apply here. Still resolve+pin the IP so the dial can't be re-pointed
	// mid-request.
	target, err := p.resolveTrustedURL(p.controlPlaneURL)
	if err != nil {
		return nil, err
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(
		context.WithValue(ctx, vettedIPKey{}, target),
		method, p.controlPlaneURL+path, rdr)
	if err != nil {
		return nil, err
	}
	// Inject the run token ONLY toward the control plane.
	req.Header.Set("Authorization", "Bearer "+p.runToken.Get())
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return p.localClient.Do(req)
}

// reqOf builds the egress.Request every handler-side emitter records.
// Paired with decisionLog (policy.go), which wraps it into a DecisionLog.
func (p *Proxy) reqOf(r *http.Request, host string, port int) egress.Request {
	return egress.Request{
		RunID:  p.runID,
		Host:   host,
		Port:   port,
		Method: strings.ToUpper(r.Method),
		Path:   r.URL.Path,
		Time:   p.now(),
	}
}

// resolveTrustedURL resolves the host of a TRUSTED rawURL (the
// operator-configured control-plane endpoint) to a pinned "ip:port" dial
// target WITHOUT applying the private/reserved-IP denial, unlike vetURL.
// Still pins a single IP (TOCTOU/DNS-rebinding guard). Fails closed on any
// unparseable URL or unresolvable host.
func (p *Proxy) resolveTrustedURL(rawURL string) (string, error) {
	host, port, err := hostPortFromURL(rawURL)
	if err != nil {
		return "", err
	}
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if h == "" {
		return "", fmt.Errorf("trusted url %q has empty host", rawURL)
	}
	// Literal IP: pin directly (no denial — trusted destination).
	if ip := net.ParseIP(h); ip != nil {
		return net.JoinHostPort(ip.String(), strconv.Itoa(port)), nil
	}
	res := p.res
	if res == nil {
		res = netResolver{}
	}
	ips, err := res.LookupIP(h)
	if err != nil {
		return "", fmt.Errorf("resolve trusted host %q: %w", h, err)
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("trusted host %q resolved to no addresses", h)
	}
	return net.JoinHostPort(ips[0].String(), strconv.Itoa(port)), nil
}

// hostPortFromURL extracts the lowercased host and port from a base URL,
// defaulting the port from the scheme (http=80, https=443).
func hostPortFromURL(rawURL string) (host string, port int, err error) {
	u := rawURL
	scheme := ""
	if i := strings.Index(u, "://"); i >= 0 {
		scheme = strings.ToLower(u[:i])
		u = u[i+3:]
	}
	// Strip any path/query — only the authority matters.
	if i := strings.IndexAny(u, "/?#"); i >= 0 {
		u = u[:i]
	}
	if u == "" {
		return "", 0, fmt.Errorf("url %q has no host", rawURL)
	}
	defaultPort := 80
	if scheme == "https" {
		defaultPort = 443
	}
	host, port = splitHostPort(u, defaultPort)
	if host == "" {
		return "", 0, fmt.Errorf("url %q has empty host", rawURL)
	}
	return host, port, nil
}

// relayControlPlane is the shared tail of every brokered sandbox->control-plane
// route: forward with the run token injected, capture the capped response
// body, emit the brokered decision, and pass the response through verbatim.
// A forward error is a Deny row plus a 502. idOf, when non-nil, derives the
// approval id the decision row carries.
func (p *Proxy) relayControlPlane(w http.ResponseWriter, r *http.Request, method, path string,
	body []byte, contentType, ruleSource string, idOf func(status int, body []byte) *uuid.UUID) {
	resp, err := p.forwardToControlPlane(r.Context(), method, path, body, contentType)
	if err != nil {
		p.emitLocalDecision(r, egress.Deny, ruleSource, nil)
		p.httpError(w, "control plane error", err, http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxBrokeredBody))
	var approvalID *uuid.UUID
	if idOf != nil {
		approvalID = idOf(resp.StatusCode, respBody)
	}
	p.emitLocalDecision(r, decisionForStatus(resp.StatusCode), ruleSource, approvalID)
	passThrough(w, resp, respBody)
}

// approvalIDOn409 reads the approval id only out of a 409 (mint pending/denied).
func approvalIDOn409(status int, body []byte) *uuid.UUID {
	if status != http.StatusConflict {
		return nil
	}
	return extractApprovalID(body)
}

// approvalIDAlways reads the approval id out of any response body.
func approvalIDAlways(_ int, body []byte) *uuid.UUID { return extractApprovalID(body) }

// relay streams an upstream response back to the client verbatim. The
// streaming sibling of passThrough, which writes an already-captured body.
func relay(w http.ResponseWriter, resp *http.Response) {
	dst := w.Header()
	copyHeader(dst, resp.Header)
	removeHopByHop(dst)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// passThrough writes a forwarded control-plane response verbatim: status code
// and body, with hop-by-hop headers stripped.
func passThrough(w http.ResponseWriter, resp *http.Response, body []byte) {
	dst := w.Header()
	copyHeader(dst, resp.Header)
	removeHopByHop(dst)
	// The captured body length is authoritative; drop any upstream
	// Content-Length that may not match after capping.
	dst.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// decisionForStatus maps a forwarded control-plane status to an egress
// decision: 2xx is allow, anything else deny (a 409 pending is deny here;
// the approval_id is carried separately so audit can correlate).
func decisionForStatus(status int) egress.Decision {
	if status >= 200 && status < 300 {
		return egress.Allow
	}
	return egress.Deny
}

// extractApprovalID pulls the approval id out of a control-plane body: the
// "approval_id" a 409 pending carries, or the "id" of a row the approvals POST
// just created. Best-effort: a malformed body yields nil.
func extractApprovalID(body []byte) *uuid.UUID {
	var m struct {
		ApprovalID string `json:"approval_id"`
		ID         string `json:"id"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil
	}
	if m.ApprovalID == "" {
		m.ApprovalID = m.ID
	}
	if m.ApprovalID == "" {
		return nil
	}
	id, err := uuid.Parse(m.ApprovalID)
	if err != nil {
		return nil
	}
	return &id
}

// emitLocalDecision records a DecisionLog for a brokered local route that
// FORWARDS TO THE CONTROL PLANE, whose host/port are the recorded upstream.
//
// The two broker routes that re-originate to a forge must NOT use it: they
// have their own emitters recording the forge they actually dialled
// (emitGitDecision, emitPATDecision); logging those through here would make a
// clone of one forge indistinguishable from a mint or another forge's clone.
func (p *Proxy) emitLocalDecision(r *http.Request, decision egress.Decision, ruleSource string, approvalID *uuid.UUID) {
	if p.sink == nil {
		return
	}
	host, port, _ := hostPortFromURL(p.controlPlaneURL)
	log := decisionLog(p.reqOf(r, host, port), decision, ruleSource)
	log.ApprovalID = approvalID
	p.sink.emit(log)
}
