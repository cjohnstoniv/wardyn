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

// Brokered LOCAL routes served by the proxy listener itself (origin-form only;
// see ServeHTTP for the security gating). They let the sandbox obtain its
// broker-minted credentials WITHOUT ever holding the run token: the proxy holds
// the run token in its config and injects it when forwarding internal API calls
// to the control plane. The sandbox-supplied Authorization header is always
// stripped before injection so the sandbox cannot smuggle or replace brokered
// credentials.
const (
	localRoutePrefix = "/wardyn/"

	routeMint = "/wardyn/v1/credentials/mint"
	// routeApprovalsCreate (POST, exact path) raises a tool_call hold from the
	// sandbox; routeApprovals (GET, {id} suffix) polls one. Same control-plane
	// endpoint pair, same token injection.
	routeApprovalsCreate = "/wardyn/v1/approvals"
	routeApprovals       = "/wardyn/v1/approvals/"
	routeRecordings      = "/wardyn/v1/recordings/"
	routeScanResults     = "/wardyn/v1/scan-results/"
	// routeSSOToken carries the AWS SSO session captured by an `aws sso login`
	// container-login run (uploaded by wardyn-aws-sso). Same brokered shape as the
	// scan/verify result uploads.
	routeSSOToken = "/wardyn/v1/sso-token/"

	// rule_source values emitted for the brokered routes (audit pipeline).
	ruleSourceMint        = "brokered:mint"
	ruleSourceApprovals   = "brokered:approvals"
	ruleSourceRecordings  = "brokered:recording"
	ruleSourceScanResults = "brokered:scan-result"
	// A tool call decided by the run's own tool_rules, with no human asked.
	// Distinct source strings per effect so the decision log answers "how many
	// calls did policy wave through" without parsing anything.
	ruleSourceToolAllow = "policy:tool-allow"
	ruleSourceToolDeny  = "policy:tool-deny"
	ruleSourceSSOToken  = "brokered:sso-token"
	// ruleSourceArtifactMITM marks a corp artifact-registry request TLS-MITM'd only
	// to inject the operator's registry token on the wire — NOT an LLM/inspection
	// path, so the decision log reads honestly (no scan coverage implied).
	ruleSourceArtifactMITM = "artifact:mitm"

	// maxBrokeredBody caps mint/approvals forward bodies. LLM bodies are
	// unbounded (streamed) — Anthropic is the size authority there.
	maxBrokeredBody = 10 << 20 // 10 MiB
	// maxRecordingBody caps recording uploads (PTY casts compress poorly but
	// are text; 100 MiB is generous for a session).
	maxRecordingBody = 100 << 20
	// maxScanResultBody caps scan-result uploads. ScanFacts is bounded by the
	// scanner's manifest-count + per-file caps, so this is a generous DoS ceiling.
	maxScanResultBody = 8 << 20
	// Bounds for a sandbox-raised tool_call approval. The body cap is a DoS
	// ceiling; the field caps bound what is PERSISTED and shown to a human — a
	// megabyte of agent-supplied text in an approval card is a decision nobody
	// can actually read.
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
// A grant this proxy brokers on /wardyn/gh/ is REFUSED here (see
// isBrokeredGitGrant): that route mints the GitHub App installation token
// server-side and re-originates with it, so handing the same token to the
// sandbox would defeat the per-repo allowlist and the push branch-namespace
// parser — and, because an approval-gated grant is single-use, would also burn
// the broker's one mint out from under the run's own clone/push.
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
	// The 409 body carries the approval id (decision-log enrichment); a 200 mint
	// body is not parsed for one.
	p.relayControlPlane(w, r, http.MethodPost, "/api/v1/internal/credentials/mint",
		body, r.Header.Get("Content-Type"), ruleSourceMint, approvalIDOn409)
}

// handleBrokerApproval forwards GET /wardyn/v1/approvals/{id} to the control
// plane's internal approval endpoint with the run token injected.
func (p *Proxy) handleBrokerApproval(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, routeApprovals)
	// Only a bare UUID segment is a valid approval id. Parsing (not just a
	// no-slash check) makes the token-injected forward path structurally
	// traversal-proof regardless of URL re-parsing behavior.
	if _, err := uuid.Parse(id); err != nil {
		http.Error(w, "invalid approval id", http.StatusNotFound)
		return
	}
	p.relayControlPlane(w, r, http.MethodGet, "/api/v1/internal/approvals/"+id,
		nil, "", ruleSourceApprovals, nil)
}

// toolApprovalRequest is the SANDBOX-facing body for POST /wardyn/v1/approvals.
// Payload is the shape the approvals UI already renders for a tool_call
// (screens/approvals.tsx: {tool, cmd, env}).
//
// Env is a list of variable NAMES, and it is a name list BY TYPE: the wire
// cannot carry a value, so no code path exists that could persist one. A client
// that sends {"env":{"AWS_SECRET":"…"}} gets a 400, not a stored secret.
type toolApprovalRequest struct {
	Kind    string `json:"kind"`
	Payload struct {
		Tool string   `json:"tool"`
		Cmd  string   `json:"cmd"`
		Env  []string `json:"env,omitempty"`
	} `json:"payload"`
}

// toolCallScope is the requested_scope persisted for a tool_call approval:
// {tool, cmd, env} exactly, because that is what the approvals screen reads
// (deriveTitle/deriveBanner in screens/approvals.tsx). env is the joined name
// list — a display string, never values.
type toolCallScope struct {
	Tool string `json:"tool"`
	Cmd  string `json:"cmd"`
	Env  string `json:"env,omitempty"`
}

// handleBrokerCreateApproval forwards POST /wardyn/v1/approvals to the control
// plane's internal approval endpoint with the run token injected — the
// sandbox-facing alias the tool-approval gate uses to park a tool call for a
// human decision.
//
// The RUN IDENTITY IS NEVER SANDBOX INPUT: it rides the run token this proxy
// holds (forwardToControlPlane), which the control plane binds from its
// verified claims — the same derivation the approvals GET and the recording PUT
// use, and the reason the sandbox itself stays tokenless.
//
// Only kind "tool_call" is accepted. Egress holds are raised by the PROXY
// (approvals.go raise()), the component that can actually park the connection;
// letting the sandbox mint an egress_domain approval would let it open a hold
// for a host it was never allowed to reach — and, approved, teach the workspace
// an allow-list entry nothing ever asked for.
func (p *Proxy) handleBrokerCreateApproval(w http.ResponseWriter, r *http.Request) {
	var body toolApprovalRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxToolApprovalBody)).Decode(&body); err != nil {
		http.Error(w, "invalid tool approval request", http.StatusBadRequest)
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
	// An approval that names neither a tool nor a command asks a human to decide
	// about nothing. Refuse it here rather than persisting an undecidable card.
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
	// The created row's id rides the decision log so audit can join "the sandbox
	// raised this hold" to the approval it raised, without parsing the response.
	p.relayControlPlane(w, r, http.MethodPost, "/api/v1/internal/approvals",
		fwd, "application/json", ruleSourceApprovals, approvalIDAlways)
}

// clampToolField bounds a sandbox-supplied string for storage and marks any
// truncation honestly — a human decides on what this renders, so a silently
// shortened command would be a decision made on a half-true string.
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
// injected. The control plane rejects cross-run uploads (403: token run id
// must match the path run id), so the sandbox can deliver ONLY its own cast —
// this is the multi-node-safe delivery path that replaces the shared volume
// (which leaked recordings across same-uid agent containers).
func (p *Proxy) handleBrokerRecording(w http.ResponseWriter, r *http.Request) {
	p.forwardBrokeredUpload(w, r, routeRecordings, "/api/v1/internal/recordings/",
		ruleSourceRecordings, "read recording body", maxRecordingBody)
}

// forwardBrokeredUpload is the shared PUT-upload path for the brokered
// recording/scan-result/verify-result routes: parse the {runID} from the path,
// read the capped body, forward to the control plane with the run token injected,
// emit the decision, and pass the response through verbatim. The sandbox query
// string is deliberately NOT forwarded — the run→workspace linkage comes from
// trusted control-plane state, never sandbox input.
func (p *Proxy) forwardBrokeredUpload(w http.ResponseWriter, r *http.Request, prefix, cpPathPrefix, ruleSource, readErrMsg string, maxBody int64) {
	id := strings.TrimPrefix(r.URL.Path, prefix)
	if _, err := uuid.Parse(id); err != nil {
		http.Error(w, "invalid run id", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, readErrMsg, http.StatusBadRequest)
		return
	}
	p.relayControlPlane(w, r, http.MethodPut, cpPathPrefix+id, body,
		r.Header.Get("Content-Type"), ruleSource, nil)
}

// handleBrokerScanResult forwards PUT /wardyn/v1/scan-results/{runID} to the
// control plane's internal scan-result-upload endpoint with the run token
// injected — the exact sibling of handleBrokerRecording. The control plane
// rejects cross-run uploads (403: token run id must match the path run id), so a
// governed scan run can deliver ONLY its own facts. The sandbox's query string
// is deliberately NOT forwarded (like recordings): the run→workspace linkage the
// control plane needs must come from TRUSTED state, never from sandbox input.
func (p *Proxy) handleBrokerScanResult(w http.ResponseWriter, r *http.Request) {
	p.forwardBrokeredUpload(w, r, routeScanResults, "/api/v1/internal/scan-results/",
		ruleSourceScanResults, "read scan result body", maxScanResultBody)
}

// handleBrokerSSOToken forwards PUT /wardyn/v1/sso-token/{runID} to the control
// plane's internal sso-token endpoint with the run token injected — the exact
// sibling of handleBrokerScanResult. It carries the AWS SSO session
// wardyn-aws-sso captured in the sandbox back to the control plane. Cross-run
// uploads are rejected control-plane-side (token run id must match the path run
// id); the sandbox-supplied Authorization is stripped, the run token injected.
func (p *Proxy) handleBrokerSSOToken(w http.ResponseWriter, r *http.Request) {
	p.forwardBrokeredUpload(w, r, routeSSOToken, "/api/v1/internal/sso-token/",
		ruleSourceSSOToken, "read sso token body", maxScanResultBody)
}

// forwardToControlPlane builds and sends a request to the control plane,
// injecting the run token as Authorization. The control-plane host is resolved
// and IP-vetted once, and the vetted target is pinned on the request context so
// the shared transport dials it directly (no re-resolution). The inbound
// sandbox Authorization is never carried here — this request is constructed
// fresh, and only the run token is set.
func (p *Proxy) forwardToControlPlane(ctx context.Context, method, path string, body []byte, contentType string) (*http.Response, error) {
	if p.controlPlaneURL == "" {
		return nil, fmt.Errorf("control plane url not configured")
	}
	// The control-plane URL is TRUSTED operator configuration (same trust
	// boundary as the run token the proxy already holds), NOT an agent-chosen
	// target. It legitimately resolves to a private-network address (wardynd on
	// a Docker/k8s internal net). The agent-SSRF private/reserved-IP guard
	// (invariant 3) must therefore NOT apply here — it exists to stop the
	// SANDBOX from reaching internal/metadata IPs via the forward-proxy path,
	// not to stop the proxy from reaching its own control plane. We still
	// resolve+pin the IP so the dial cannot be re-pointed mid-request.
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

// reqOf builds the egress.Request every handler-side emitter records: the run,
// the upstream host/port actually contacted, and the sandbox request's method +
// path. Paired with decisionLog (policy.go), which wraps it into a DecisionLog.
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

// resolveTrustedURL resolves the host of a TRUSTED rawURL (the operator-
// configured control-plane endpoint) to a pinned "ip:port" dial target WITHOUT
// applying the private/reserved-IP denial. Unlike vetURL, this is used only for
// the proxy's own control-plane forwarding, where a private-network address is
// expected and legitimate. Resolution still pins a single IP (TOCTOU / DNS-
// rebinding guard); a literal IP is used as-is. Fails closed on any unparseable
// URL or unresolvable host.
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
// route: forward with the run token injected, capture the capped response body,
// emit the brokered decision under ruleSource, and pass the response through
// verbatim. A forward error is a Deny row plus a 502 — the fail-closed shape all
// four callers already had. Each caller keeps its own prologue (the guards).
// idOf, when non-nil, derives the approval id the decision row carries.
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

// relay streams an upstream response back to the client verbatim: headers (less
// hop-by-hop), status code, then the body. The streaming sibling of passThrough,
// which writes an already-captured (capped) body instead.
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
// decision for the brokered decision log: 2xx is allow, anything else deny.
// (A 409 pending is recorded as deny here only for the decision-log decision
// field; the approval_id is carried separately so audit can correlate.)
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

// emitLocalDecision records a DecisionLog for a brokered local route so it
// lands in audit via the existing decisions pipeline. It is for the routes that
// FORWARD TO THE CONTROL PLANE — mint, approval lookup, recording upload — whose
// host AND port are therefore the recorded upstream (an unparseable URL leaves
// both zero rather than fabricating a port).
//
// The two broker routes that re-originate to a forge do NOT use it and must not:
// they have their own emitters that record the forge they actually dialled
// (emitGitDecision -> github.com:443, emitPATDecision -> the granted PAT host).
// Logging those through here recorded the control plane instead, which made a
// clone of one forge indistinguishable from a mint, and from a clone of another.
func (p *Proxy) emitLocalDecision(r *http.Request, decision egress.Decision, ruleSource string, approvalID *uuid.UUID) {
	if p.sink == nil {
		return
	}
	host, port, _ := hostPortFromURL(p.controlPlaneURL)
	log := decisionLog(p.reqOf(r, host, port), decision, ruleSource)
	log.ApprovalID = approvalID
	p.sink.emit(log)
}
