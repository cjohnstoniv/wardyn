// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The Wardyn Git Broker: an authenticating git-smart-HTTP reverse-proxy local
// route so a sandbox NEVER dials github.com directly. The sandbox's git is
// pointed (url.<broker>.insteadOf) at http://wardyn-proxy:3128/wardyn/gh/<org>/<repo>,
// which lands here as an origin-form local route. This handler enforces the
// per-repo allowlist (p.gitGrants) — the sandbox->proxy hop is plain HTTP on
// the proxy's own endpoint, so no third-party TLS-MITM is needed to see which
// repo is requested — mints the repo-scoped GitHub App installation token
// server-side, and re-originates to github.com with the token as Basic auth
// on the OUTBOUND request only; the token is never returned to the sandbox.
//
// Repo is the unit of trust: a repo not in p.gitGrants is 403, so the sandbox
// can reach ONLY the repos its run was granted, never all of github.com.
const (
	routeGitBroker = "/wardyn/gh/"
	ruleSourceGit  = "brokered:git"
	githubHost     = "github.com"
	// gitBrokerUsername is the git username a GitHub App INSTALLATION token
	// authenticates as — fixed by GitHub. Named so the mask registration and
	// the outbound header (SetBasicAuth) derive it from the same place.
	gitBrokerUsername = "x-access-token"
	// ruleSourceGitRef marks a decision-log row denied by push branch-namespace
	// confinement, so audit says WHICH gate closed. The offending ref goes to
	// slog only, since the decision log has no free-text field and is SIEM-fanned.
	ruleSourceGitRef = "brokered:git:branch-ns"
	// ruleSourceGitEnc marks refusal of a push body this proxy could not INSPECT
	// (non-identity Content-Encoding) — distinct so audit never reads it as a ref violation.
	ruleSourceGitEnc = "brokered:git:branch-ns-encoding"
	// envEnforceBranchNS is the ONE var covering both brokered git lanes'
	// push branch-namespace confinement (#203: folds the former
	// WARDYN_GIT_PAT_BROKER_ENFORCE_BRANCH_NS into this name with an
	// {app,pat} scope — see branchNSScopes). See BranchNSEnforced (App lane,
	// ON by default) and PATBranchNSEnforced (PAT lane, OFF by default).
	envEnforceBranchNS = "WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS"
	// ruleSourceGitNSOff marks a push this proxy FORWARDED WITHOUT PARSING while
	// the lane's confinement was otherwise in force (an opt-out via env or
	// git_push_any_branch). It is the per-push proof of which posture actually
	// applied, alongside the boot log's one-time statement of the posture.
	ruleSourceGitNSOff = "brokered:git:branch-ns-off"
	// maxReceivePackCmds caps the pkt-line COMMAND SECTION buffered ahead of the
	// streamed packfile; anything larger is pathological and refused.
	maxReceivePackCmds = 64 << 10
	// envGitApprovalTimeout overrides gitApprovalTimeout's default (operator
	// escape hatch for a slower approval workflow).
	envGitApprovalTimeout = "WARDYN_GIT_APPROVAL_TIMEOUT"
	// defaultGitApprovalTimeout mirrors wardyn-git-helper's own default: how
	// long the FIRST clone/fetch/push on an approval-gated grant blocks waiting for a human approval.
	defaultGitApprovalTimeout = 120 * time.Second
)

// brokerRefreshMargin is how close to a STATED expiry brokeredToken re-mints.
// Deliberately NOT injectRefreshMargin (inject.go, 5m): this cache serves ONE
// clone's two sub-requests from a single mint, and a 5m margin would treat a
// grant with ttl_seconds<=300 as stale on the very next sub-request. At
// sub-request scale the margin only needs to cover the round trip.
const brokerRefreshMargin = 30 * time.Second

// gitApprovalBudget is the operator's ceiling for ONE brokered-credential
// acquisition: the first mint call, the approval wait, and every poll inside
// it (WARDYN_GIT_APPROVAL_TIMEOUT, default defaultGitApprovalTimeout).
//
// Trust boundary: nothing else bounds the underlying HTTP calls (server.go's
// timeouts are 0, p.localClient has none), so against a control plane that
// never answers a clone would otherwise block FOREVER. The budget is derived
// once at the top of brokeredToken and passed down so the documented timeout
// is a real bound; the human's approval window shrinks by however long the
// first mint round trip took.
func gitApprovalBudget() time.Duration {
	if v := os.Getenv(envGitApprovalTimeout); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultGitApprovalTimeout
}

// gitApprovalPollInterval is how often mintGitToken re-polls a pending
// mint-approval. A var so tests can shrink it.
var gitApprovalPollInterval = 2 * time.Second

// gitServices is the closed set of valid ?service= values / smart-HTTP verbs.
var gitServices = map[string]bool{"git-upload-pack": true, "git-receive-pack": true}

// gitTokEntry caches one grant's minted broker credential. token/username/
// expiresAt are guarded by reMu, which ALSO single-flights the mint so
// concurrent sub-requests don't stampede a double-mint (fatal for a
// single-use approval-gated grant). Backs both broker lanes (see brokeredToken).
type gitTokEntry struct {
	reMu      sync.Mutex
	token     string
	username  string
	expiresAt int64 // unix ms; 0 = no expiry stated by the mint
}

// handleGitBroker serves /wardyn/gh/<org>/<repo>[.git]/<rest>. It is
// method-agnostic at the switch; it validates the method against the smart-HTTP
// subpath itself.
func (p *Proxy) handleGitBroker(w http.ResponseWriter, r *http.Request) {
	orgRepo, rest, ok := parseGitBrokerPath(r.URL.Path)
	if !ok {
		p.emitGitDecision(r, egress.Deny, ruleSourceGit)
		http.Error(w, "invalid git broker path", http.StatusNotFound)
		return
	}
	// The allowlist lookup is the STRUCTURAL traversal + authorization gate:
	// anything not an exact granted key 403s before any github URL is formed.
	grantID, granted := p.gitGrants[orgRepo]
	if !granted {
		p.emitGitDecision(r, egress.Deny, ruleSourceGit)
		p.denyAttributed(w, "repository not granted to this run", http.StatusForbidden)
		return
	}
	if !validGitRest(r.Method, rest, r.URL.Query().Get("service")) {
		p.emitGitDecision(r, egress.Deny, ruleSourceGit)
		http.Error(w, "unsupported git request", http.StatusForbidden)
		return
	}

	// Push confinement (ON by default; =false opts out). A receive-pack POST
	// carries its ref updates in a small pkt-line command section AHEAD of the
	// packfile, so buffer only that section, validate every ref against this
	// run's branch namespace, then forward it followed by the still-streaming
	// pack. Fetch/clone never enter this branch. allowSrc records which posture
	// applied (ruleSourceGitNSOff when either switch opted out).
	var reqBody io.Reader = r.Body
	allowSrc := ruleSourceGit
	isPush := rest == "git-receive-pack"
	if isPush && (!BranchNSEnforced() || p.policy.GitPushAnyBranch()) {
		allowSrc = ruleSourceGitNSOff
	} else if isPush {
		body, ok := p.confinePush(w, r, slog.String("repo", orgRepo),
			func(ruleSource string) { p.emitGitDecision(r, egress.Deny, ruleSource) })
		if !ok {
			return
		}
		reqBody = body
	}
	// CONTENT rules, entered independently of the block above: git_push_any_branch
	// opts out of WHERE a push may land, and wiring this inside that block
	// would let a WHERE opt-out silently switch off a WHAT control. A refused
	// push is never forwarded.
	if isPush {
		forge := &forgeRepo{p: p, repo: orgRepo,
			token: func(ctx context.Context) (string, error) { return p.gitToken(ctx, grantID) }}
		body, release, ok := p.applyPushRules(w, r, reqBody, slog.String("repo", orgRepo),
			func(ruleSource string) { p.emitGitDecision(r, egress.Deny, ruleSource) }, forge,
			appPushTarget(orgRepo, grantID))
		defer release()
		if !ok {
			return
		}
		reqBody = body
	}

	token, err := p.gitToken(r.Context(), grantID)
	if err != nil {
		p.emitGitDecision(r, egress.Deny, ruleSourceGit)
		p.httpError(w, "git credential unavailable", err, http.StatusBadGateway)
		return
	}

	// egressTarget hides the corp-upstream branch: under an operator upstream
	// proxy this sends the CONNECT to github.com BY NAME rather than a
	// resolved IP literal the corp proxy would refuse.
	target, _, err := p.egressTarget(githubHost, 443)
	if err != nil {
		p.emitGitDecision(r, egress.Deny, ruleSourceGit)
		p.httpError(w, "git upstream vet failed", err, http.StatusBadGateway)
		return
	}

	// Build the upstream URL from the MATCHED key + validated rest, never the
	// raw request path, so nothing attacker-controlled beyond it flows outbound.
	upstreamURL := "https://" + githubHost + "/" + orgRepo + ".git/" + rest
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}
	outReq, err := http.NewRequestWithContext(
		context.WithValue(r.Context(), vettedIPKey{}, target),
		r.Method, upstreamURL, reqBody)
	if err != nil {
		p.emitGitDecision(r, egress.Deny, ruleSourceGit)
		p.httpError(w, "build git request", err, http.StatusBadGateway)
		return
	}
	// Stream the body straight through (only the validated receive-pack command
	// section was buffered above). Preserve declared length; unknown => chunked.
	outReq.ContentLength = r.ContentLength
	copyHeader(outReq.Header, r.Header)
	removeHopByHop(outReq.Header)
	// Defensive: strip any sandbox-supplied credential before injecting ours
	// (stripSandboxCredentials, inject.go) so a rogue client can't smuggle its own.
	stripSandboxCredentials(outReq.Header, "")
	// Content rules need a readable pack, sent only when asked (push_advert.go);
	// identity is asked for explicitly so the transport can't negotiate its own coding.
	noThin := p.noThinAdvert(r, rest)
	if noThin {
		outReq.Header.Set("Accept-Encoding", "identity")
	}
	outReq.SetBasicAuth(gitBrokerUsername, token) // GitHub App installation-token auth
	outReq.Host = githubHost
	outReq.Header.Del("Host")

	// roundTripUpstream, not the transport directly: this forge speaks HTTP/2,
	// same fallback the MITM/plain lanes get. The allow row follows success only.
	resp, err := p.roundTripUpstream(outReq)
	if err != nil {
		p.failUpstream(w, err, &egress.DecisionLog{Request: p.reqOf(r, githubHost, 443)}, githubHost, "git upstream error")
		return
	}
	p.emitDialledAllow(decisionLog(p.reqOf(r, githubHost, 443), egress.Allow, allowSrc), githubHost)
	defer func() { _ = resp.Body.Close() }()

	if noThin {
		relayNoThinAdvert(w, resp) // relay(), with no-thin added to the advertisement
		return
	}
	relay(w, resp) // stream the pack back
}

// isBrokeredGitGrant reports whether a sandbox-supplied mint body names one of
// THIS run's git-broker grants — the guard that keeps the App installation
// token out of the sandbox (handleBrokerMint). handleGitBroker's own mint
// bypasses this (mintGitToken -> forwardToControlPlane directly).
//
// Decoding mirrors the control plane's handleInternalMint EXACTLY (same
// Decoder, same struct, same tag), so a body that mints there decodes to the
// same grant id here.
//
// ponytail: fail OPEN on an undecodable body. A body we cannot decode cannot name
// a brokered grant the control plane would accept either (it 400s), so forwarding
// it preserves today's error behavior for legitimate callers without opening a
// hole. Fail-closed here would only convert a control-plane 400 into a proxy 403.
func (p *Proxy) isBrokeredGitGrant(body []byte) bool {
	if len(p.gitGrants) == 0 {
		return false
	}
	var req struct {
		GrantID uuid.UUID `json:"grant_id"`
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil || req.GrantID == uuid.Nil {
		return false
	}
	for _, id := range p.gitGrants {
		if id == req.GrantID {
			return true
		}
	}
	return false
}

// isBrokeredPATGrant reports whether a sandbox-supplied mint body names one of
// THIS run's git_pat grants while the PAT broker is on: the guard that keeps a
// stored PAT out of the sandbox through the raw mint relay. It decodes exactly
// as isBrokeredGitGrant does and fails open on an undecodable body for the same
// reason. handlePATBroker's own mint bypasses this (brokeredToken ->
// forwardToControlPlane directly).
func (p *Proxy) isBrokeredPATGrant(body []byte) bool {
	if len(p.brokeredPATGrantIDs) == 0 {
		return false
	}
	var req struct {
		GrantID uuid.UUID `json:"grant_id"`
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil || req.GrantID == uuid.Nil {
		return false
	}
	return slices.Contains(p.brokeredPATGrantIDs, req.GrantID)
}

// gitToken returns a cached (or freshly minted) installation token for grantID.
// The GitHub lane always authenticates as x-access-token, so it discards the
// username brokeredToken carries for its git_pat sibling.
func (p *Proxy) gitToken(ctx context.Context, grantID uuid.UUID) (string, error) {
	// The wire username is the CONSTANT this lane authenticates as, never what
	// the mint returned (a github_token mint returns none).
	tok, _, err := p.brokeredToken(ctx, grantID, func(string) string { return gitBrokerUsername })
	return tok, err
}

// brokeredToken returns a cached (or freshly minted) broker credential for
// grantID, re-minting only when unset or within brokerRefreshMargin of a
// STATED expiry. reMu single-flights per grant so concurrent sub-requests
// don't double-mint (fatal for a single-use approval-gated grant, whose
// second mint 409s ErrAlreadyMinted; a PENDING 409 is waited out here).
//
// A mint stating NO expiry (expiresAt == 0, or unparseable) is cached for the
// process rather than re-minted per request, since re-minting is the fatal
// act for a single-use grant.
//
// wireUser maps the mint's username to what the CALLING LANE actually puts on
// the wire, so the mask registered below covers the rendering that leaves the process.
func (p *Proxy) brokeredToken(ctx context.Context, grantID uuid.UUID, wireUser func(mintUsername string) string) (token, username string, err error) {
	p.gitTokMu.Lock()
	e, ok := p.gitTokens[grantID]
	if !ok {
		e = &gitTokEntry{}
		p.gitTokens[grantID] = e
	}
	p.gitTokMu.Unlock()

	e.reMu.Lock()
	defer e.reMu.Unlock()
	if e.token != "" && (e.expiresAt == 0 ||
		time.Now().Before(time.UnixMilli(e.expiresAt).Add(-brokerRefreshMargin))) {
		return e.token, e.username, nil
	}
	// One budget for the whole acquisition below (see gitApprovalBudget), armed
	// after the cache check so a cache hit costs nothing.
	ctx, cancel := context.WithTimeout(ctx, gitApprovalBudget())
	defer cancel()
	// Auto-mintable grants re-mint past TTL fine; approval-gated grants are
	// single-use, so a run outliving the ~1h token TTL fails here (a pre-existing ceiling).
	tok, user, expMs, err := p.mintGitToken(ctx, grantID)
	if err != nil {
		return "", "", err
	}
	// Register the installation token in the process-global mask registry
	// (inject.go) so it can never ride out on a sandbox-visible error string or
	// decision-log line. Both renderings are registered: the raw token AND the
	// base64(username+":"+tok) SetBasicAuth puts on the wire — registering only
	// the raw token would leave the wire form unmasked.
	//
	// wireUser(user), NOT the mint's username: the two lanes send different
	// usernames, and masking the wrong rendering (the mint's, rather than what
	// actually goes on the wire) would leave the real one in cleartext.
	// Residual: only verbatim bytes of those renderings are caught.
	registerBasicAuthCredential(wireUser(user), tok)
	e.token, e.username, e.expiresAt = tok, user, expMs
	return tok, user, nil
}

// mintGitToken calls the control-plane mint route server-side (the exact
// route wardyn-git-helper uses). Returns the token and its expiry (unix ms; 0 if unparseable).
//
// On the FIRST clone/fetch/push against an approval-gated grant, the control
// plane 409s with an approval_id instead of minting. Unlike the sandbox-facing
// mint route (which passes the 409 through for the caller to poll), the
// broker mints server-side with no caller able to retry, so it polls the SAME
// approval itself here rather than 502ing the clone.
func (p *Proxy) mintGitToken(ctx context.Context, grantID uuid.UUID) (token, username string, expMs int64, err error) {
	tok, user, exp, status, body, err := p.callMintGit(ctx, grantID)
	if err != nil {
		return "", "", 0, err
	}
	if status == http.StatusOK {
		return tok, user, exp, nil
	}
	if status != http.StatusConflict {
		return "", "", 0, fmt.Errorf("mint status %d: %s", status, strings.TrimSpace(string(body)))
	}
	approvalID := extractApprovalID(body)
	if approvalID == nil {
		return "", "", 0, fmt.Errorf("mint status 409 without approval_id")
	}
	return p.waitForGitApproval(ctx, grantID, *approvalID)
}

// callMintGit issues ONE POST to the control-plane mint route: (token,
// username, expiry, 200) on success, (409, body-with-approval_id) when
// pending, or an error the caller cannot retry. username is discarded by the GitHub lane.
func (p *Proxy) callMintGit(ctx context.Context, grantID uuid.UUID) (token, username string, expMs int64, status int, body []byte, err error) {
	reqBody, err := json.Marshal(map[string]string{"grant_id": grantID.String()})
	if err != nil {
		return "", "", 0, 0, nil, err
	}
	resp, err := p.forwardToControlPlane(ctx, http.MethodPost,
		"/api/v1/internal/credentials/mint", reqBody, "application/json")
	if err != nil {
		return "", "", 0, 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxBrokeredBody))
	if resp.StatusCode != http.StatusOK {
		return "", "", 0, resp.StatusCode, respBody, nil
	}
	var mr struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
		// Username is set for a git_pat mint (the host's expected git username);
		// empty for a github_token mint (always x-access-token).
		Username string `json:"username"`
	}
	if err := json.Unmarshal(respBody, &mr); err != nil {
		return "", "", 0, 0, nil, fmt.Errorf("decode mint response: %w", err)
	}
	if mr.Token == "" {
		return "", "", 0, 0, nil, fmt.Errorf("mint response missing token")
	}
	if t, perr := time.Parse(time.RFC3339, mr.ExpiresAt); perr == nil {
		expMs = t.UnixMilli()
	}
	return mr.Token, mr.Username, expMs, http.StatusOK, nil, nil
}

// waitForGitApproval polls approvalID (the SAME control-plane route
// handleBrokerApproval forwards) until it reaches a terminal state or
// gitApprovalTimeout elapses, re-minting exactly once on APPROVED. A
// transient poll error is retried, not fatal.
func (p *Proxy) waitForGitApproval(ctx context.Context, grantID, approvalID uuid.UUID) (token, username string, expMs int64, err error) {
	timeout := gitApprovalBudget()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(gitApprovalPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// Say what the deadline arm says rather than a bare "context deadline
			// exceeded"; a CANCELLED ctx (sandbox hanging up) is a different thing.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return "", "", 0, fmt.Errorf("timed out after %s waiting for credential approval %s — "+
					"approve it in the Wardyn UI and re-run", timeout, approvalID)
			}
			return "", "", 0, ctx.Err()
		case <-deadline.C:
			return "", "", 0, fmt.Errorf("timed out after %s waiting for credential approval %s — "+
				"approve it in the Wardyn UI and re-run", timeout, approvalID)
		case <-ticker.C:
		}
		state, perr := p.pollGitApproval(ctx, approvalID)
		if perr != nil {
			continue // transient: keep waiting until the deadline
		}
		switch state {
		case types.ApprovalApproved:
			tok, user, exp, status, body, mErr := p.callMintGit(ctx, grantID)
			if mErr != nil {
				return "", "", 0, mErr
			}
			if status != http.StatusOK {
				return "", "", 0, fmt.Errorf("re-mint after approval status %d: %s", status, strings.TrimSpace(string(body)))
			}
			return tok, user, exp, nil
		case types.ApprovalDenied:
			return "", "", 0, fmt.Errorf("credential approval %s was denied by the operator", approvalID)
		case types.ApprovalExpired:
			return "", "", 0, fmt.Errorf("credential approval %s expired before a decision was made", approvalID)
		case types.ApprovalCancelled:
			// Terminal, like DENIED/EXPIRED — else a killed run would block the
			// git operation for the whole timeout for no reason.
			return "", "", 0, fmt.Errorf("credential approval %s was cancelled: the run ended before anyone decided it", approvalID)
		default: // still PENDING: keep polling
		}
	}
}

// pollGitApproval fetches one approval's current state via the same
// control-plane route the sandbox-facing /wardyn/v1/approvals/{id} local
// route forwards (handleBrokerApproval), run-token-authenticated.
func (p *Proxy) pollGitApproval(ctx context.Context, id uuid.UUID) (types.ApprovalState, error) {
	resp, err := p.forwardToControlPlane(ctx, http.MethodGet, "/api/v1/internal/approvals/"+id.String(), nil, "")
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBrokeredBody))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("poll approval status %d", resp.StatusCode)
	}
	var ar types.ApprovalRequest
	if err := json.Unmarshal(body, &ar); err != nil {
		return "", err
	}
	return ar.State, nil
}

// parseGitBrokerPath splits /wardyn/gh/<org>/<repo>[.git]/<rest...> into the
// canonical lowercased "<org>/<repo>" key (.git stripped) and the smart-HTTP
// subpath. ok=false on a malformed shape; the allowlist lookup is the real gate.
func parseGitBrokerPath(path string) (orgRepo, rest string, ok bool) {
	segs := strings.Split(strings.TrimPrefix(path, routeGitBroker), "/")
	if len(segs) < 3 {
		return "", "", false
	}
	org, repo := segs[0], strings.TrimSuffix(segs[1], ".git")
	if !gitSegSafe(org) || !gitSegSafe(repo) {
		return "", "", false
	}
	return strings.ToLower(org + "/" + repo), strings.Join(segs[2:], "/"), true
}

// gitSegSafe allows one GitHub owner/repo segment ([A-Za-z0-9._-]) and rejects
// empty / "." / ".." so no path-traversal segment survives parsing.
func gitSegSafe(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

// validGitRest restricts the smart-HTTP subpath + method to the git v2 verbs the
// broker serves: GET info/refs?service=git-{upload,receive}-pack, and POST
// git-upload-pack (fetch) / git-receive-pack (push, scoped to the granted repo).
func validGitRest(method, rest, service string) bool {
	switch rest {
	case "info/refs":
		return method == http.MethodGet && gitServices[service]
	case "git-upload-pack", "git-receive-pack":
		return method == http.MethodPost
	default:
		return false
	}
}

// BranchNSPrefix is the ONLY ref prefix a governed run may push to:
// `wardyn/<run-id>/*` under refs/heads/. MUST stay in lockstep with
// internal/broker.branchNamespaceFormat (TestBranchNamespaceLockstepWithProxy
// fails on drift, the reason this is exported).
func BranchNSPrefix(runID uuid.UUID) string {
	return "refs/heads/wardyn/" + runID.String() + "/"
}

// BranchNSEnforced reports whether push branch-namespace confinement is ON for
// THIS proxy process's App lane. Exported so cmd/wardyn-proxy can state the
// posture ONCE at boot: the per-mint branch_namespace metadata is written
// identically either way, so without that boot line an opted-out proxy reads
// exactly like a confined one.
//
// ON by default: agent-run already checks every cloned repo out onto
// `wardyn/$WARDYN_RUN_ID/work`, so a stock run is already inside its
// namespace. Set WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS=false to opt back out —
// a BARE value binds the App lane only (see branchNSScopes), unchanged from
// before #203 folded the PAT lane's own switch into this same var name.
//
// Scope: THIS switch binds the BROKERED GITHUB path only (handleGitBroker),
// not its git_pat sibling. An SSH push is an opaque tunnel no pkt-line parser
// can read. A PAT push is NOT, and saying so was the stale half of this
// comment. Since 0.7.2 the parser IS wired in there, through the same
// confinePush step, but behind its OWN switch and DEFAULT OFF
// (PATBranchNSEnforced) — a PAT carries whatever scope the operator issued on
// forges whose push-ref conventions aren't GitHub's, and Wardyn cannot narrow
// it by default. Not a hole either way: a brokered repo has no co-granted
// PAT/ssh_key path beside it (validateGrantLaneExclusivity refuses the
// combination at policy-write; dispatch removes the forge's ssh/PAT egress
// and withholds those grants from the sandbox). One standing caveat, shared
// with docs/POLICIES.md's four HTTPS denies: a name-based deny does not bind
// an IP literal, so the brokered route is the only CONVENIENT route, not the
// only conceivable one.
//
// Loud parse: an unrecognized value fails CLOSED (both lanes enforce + error log).
func BranchNSEnforced() bool {
	app, _ := branchNSScopes()
	return app
}

var branchNSWarnOnce sync.Once

// confinePush is the push branch-namespace confinement STEP, shared by both
// brokered git lanes: it reads the receive-pack command section, validates
// every ref against this run's namespace, and returns the body to forward.
//
// On a refusal it writes the 403 itself (through deny, against each lane's
// own host) and returns ok=false — kept here, not at the call sites, so both
// lanes refuse in the same words. Which lane may call it, and under which
// switch, stays the caller's decision.
func (p *Proxy) confinePush(w http.ResponseWriter, r *http.Request, subject slog.Attr, deny func(ruleSource string)) (io.Reader, bool) {
	// git does not gzip receive-pack bodies, but a compressed body must never
	// be waved through unparsed — that would be a silent bypass.
	if enc, bad := nonIdentityEncoding(r.Header); bad {
		deny(ruleSourceGitEnc)
		http.Error(w, "wardyn: cannot enforce branch-namespace confinement on a "+
			enc+"-encoded push body", http.StatusUnsupportedMediaType)
		return nil, false
	}
	prefix := BranchNSPrefix(p.runID)
	head, err := readReceivePackCommands(r.Body, prefix)
	if err != nil {
		deny(ruleSourceGitRef)
		slog.WarnContext(r.Context(), "wardyn-proxy: git push denied by branch-namespace confinement",
			slog.String("run_id", p.runID.String()),
			subject,
			slog.String("reason", err.Error()))
		// ponytail: plain 403 + text/plain body (git surfaces it as "remote:"
		// on the paths that show server messages, and always shows the 403).
		// A sideband report-status would read better but means claiming
		// "unpack ok" for a pack we never forwarded.
		p.denyAttributed(w, "wardyn: "+err.Error()+
			"\nthis run may push only to "+prefix+"*", http.StatusForbidden)
		return nil, false
	}
	return io.MultiReader(bytes.NewReader(head), r.Body), true
}

// PATBranchNSEnforced reports whether push branch-namespace confinement is ON
// for the git_pat lane. OFF unless the operator opts in with the `pat` scope
// of WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS (folded from the standalone
// WARDYN_GIT_PAT_BROKER_ENFORCE_BRANCH_NS by #203).
//
// A SECOND scope with the OPPOSITE default, not a widening of
// BranchNSEnforced: a GitHub App token is Wardyn's own mint, but a PAT is the
// operator's, carrying whatever scope they issued against forges whose
// push-ref conventions aren't GitHub's — Wardyn cannot narrow it, so it isn't
// confined by default. Once opted IN, the parse is the same loud, fail-closed one.
func PATBranchNSEnforced() bool {
	_, pat := branchNSScopes()
	return pat
}

// branchNSWord parses one recognized enforcement word (the shared word list
// every enum switch in this package uses). ok=false means "not a recognized
// word", not a value.
func branchNSWord(v string) (enforce, ok bool) {
	switch v {
	case "0", "false", "no", "off", "disable", "disabled", "none":
		return false, true
	case "1", "true", "yes", "on", "enable", "enabled", "enforce":
		return true, true
	default:
		return false, false
	}
}

// branchNSScopes parses the single WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS var
// into both brokered lanes' enforcement booleans. #203 folds the former,
// standalone WARDYN_GIT_PAT_BROKER_ENFORCE_BRANCH_NS into this one name with
// an {app,pat} scope rather than keeping two var names.
//
// Unset keeps the pre-merge defaults: App on, PAT off.
//
// A BARE recognized word (the shared word list) sets the APP lane ONLY — the
// exact pre-merge meaning of this var, so an operator who merely restates
// today's App-lane default (or opts it out) cannot silently flip the PAT
// lane's posture as a side effect of a value that never named it. The PAT
// lane keeps its own default unless named explicitly.
//
// A SCOPED value, "app:<word>,pat:<word>" (either order, either alone, same
// word list), sets one or both lanes explicitly; a scope not named keeps its
// own default.
//
// Anything else fails CLOSED — mixing a bare word with a scoped part, an
// unrecognized word, an unrecognized scope name, or the same scope named
// twice with disagreeing words — and BOTH lanes enforce, logged once per
// process: a garbage value can never quietly weaken either lane.
func branchNSScopes() (app, pat bool) {
	app, pat = true, false // pre-merge defaults
	raw := strings.TrimSpace(os.Getenv(envEnforceBranchNS))
	if raw == "" {
		return app, pat
	}
	if !strings.Contains(raw, ":") {
		if v, ok := branchNSWord(strings.ToLower(raw)); ok {
			return v, pat
		}
		return branchNSFailClosed(raw)
	}
	set := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		scope, word, cut := strings.Cut(strings.TrimSpace(part), ":")
		scope = strings.ToLower(strings.TrimSpace(scope))
		v, ok := branchNSWord(strings.ToLower(strings.TrimSpace(word)))
		if !cut || !ok || (scope != "app" && scope != "pat") {
			return branchNSFailClosed(raw)
		}
		if prev, dup := set[scope]; dup && prev != v {
			return branchNSFailClosed(raw)
		}
		set[scope] = v
	}
	if v, ok := set["app"]; ok {
		app = v
	}
	if v, ok := set["pat"]; ok {
		pat = v
	}
	return app, pat
}

// branchNSFailClosed logs the one-shot garbage-value warning and enforces
// both lanes, shared by every branchNSScopes exit that could not make sense
// of the raw value.
func branchNSFailClosed(raw string) (app, pat bool) {
	branchNSWarnOnce.Do(func() {
		slog.Error("wardyn-proxy: unrecognized "+envEnforceBranchNS+
			" value; ENFORCING push branch-namespace confinement on BOTH lanes (fail closed)",
			slog.String("value", raw))
	})
	return true, true
}

// readReceivePackCommands consumes the pkt-line COMMAND SECTION of a
// git-receive-pack request body (up to and INCLUDING the flush-pkt),
// validating every ref update against prefix, and returns those bytes
// VERBATIM to forward ahead of the still-streaming packfile.
//
// Wire shape: every pkt-line starts with 4 hex length digits that COUNT
// THEMSELVES; "0000" is the flush-pkt; the first command carries
// "\0<capability-list>"; optional "shallow <oid>" lines may precede commands.
// Anything not understood (signed push-cert, bad length, oversized section) is refused.
func readReceivePackCommands(body io.Reader, prefix string) ([]byte, error) {
	var buf bytes.Buffer
	hdr := make([]byte, 4)
	seenCmd := false
	for {
		if _, err := io.ReadFull(body, hdr); err != nil {
			return nil, fmt.Errorf("unreadable pkt-line length: %w", err)
		}
		buf.Write(hdr)
		n, err := strconv.ParseUint(string(hdr), 16, 32)
		if err != nil {
			return nil, fmt.Errorf("malformed pkt-line length %q", hdr)
		}
		if n == 0 { // flush-pkt: command section done, the packfile follows.
			return buf.Bytes(), nil
		}
		// 0001/0002 (delim / response-end) and empty 0004 lines are not part of a
		// receive-pack command section.
		if n < 5 {
			return nil, fmt.Errorf("unexpected pkt-line length %d in receive-pack command section", n)
		}
		if buf.Len()+int(n)-4 > maxReceivePackCmds {
			return nil, fmt.Errorf("receive-pack command section exceeds %d bytes", maxReceivePackCmds)
		}
		payload := make([]byte, n-4)
		if _, err := io.ReadFull(body, payload); err != nil {
			return nil, fmt.Errorf("truncated pkt-line: %w", err)
		}
		buf.Write(payload)
		if err := checkPushCommand(string(payload), prefix, !seenCmd); err != nil {
			return nil, err
		}
		seenCmd = seenCmd || !bytes.HasPrefix(payload, []byte("shallow "))
	}
}

// checkPushCommand validates ONE command-section pkt-line payload:
// "<old-oid> SP <new-oid> SP <refname>", with "\0<capabilities>" on the
// first, or a "shallow <oid>" line. first reports whether this is the first line.
//
// A NUL anywhere but the first command is refused: a forge whose parser
// differs could read a later NUL-hidden ref as a different ref than checked here.
func checkPushCommand(line, prefix string, first bool) error {
	line, _, caps := strings.Cut(line, "\x00") // capabilities ride the FIRST command only
	line = strings.TrimSuffix(line, "\n")
	shallow := strings.HasPrefix(line, "shallow ")
	if caps && (!first || shallow) {
		return fmt.Errorf("refusing receive-pack command %q: a NUL is allowed only on the first command", line)
	}
	if shallow {
		return nil
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 || parts[2] == "" {
		return fmt.Errorf("unsupported receive-pack command %q (only <old> <new> <ref> updates are allowed)", line)
	}
	ref := parts[2]
	// Defense in depth: a prefix test alone must never be the only thing
	// between "wardyn/<id>/x" and a traversal (adoscope.CheckRefName).
	if err := adoscope.CheckRefName(ref); err != nil {
		return err
	}
	if !strings.HasPrefix(ref, prefix) || len(ref) <= len(prefix) {
		return fmt.Errorf("push to %q is outside this run's branch namespace", ref)
	}
	return nil
}

// emitGitDecision records a brokered:git decision-log row (host github.com, port
// 443) so clone/fetch/push land in audit as allow/deny rows.
func (p *Proxy) emitGitDecision(r *http.Request, decision egress.Decision, ruleSource string) {
	if p.sink == nil {
		return
	}
	p.sink.emit(decisionLog(p.reqOf(r, githubHost, 443), decision, ruleSource))
}
