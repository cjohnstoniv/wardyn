// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// The Wardyn Git Broker: an authenticating git-smart-HTTP reverse-proxy local
// route so a sandbox NEVER dials github.com directly. The sandbox's git is
// pointed (url.<broker>.insteadOf, set by agent-run) at
// http://wardyn-proxy:3128/wardyn/gh/<org>/<repo>, which lands here as an
// origin-form local route (wardyn-proxy is in the sandbox NO_PROXY). This
// handler enforces the per-repo allowlist (p.gitGrants) in CLEARTEXT — the
// sandbox->proxy hop is plain HTTP on the proxy's own endpoint, so no third-party
// TLS-MITM is needed to see which repo is requested — mints the repo-scoped
// GitHub App installation token server-side, and re-originates to github.com with
// the token as Basic auth on the OUTBOUND request only. The token is never
// returned to the sandbox (git responses carry no Authorization).
//
// Repo is the unit of trust: a repo not in p.gitGrants is 403, so the sandbox can
// reach ONLY the repos its run was granted, never all of github.com.
const (
	routeGitBroker = "/wardyn/gh/"
	ruleSourceGit  = "brokered:git"
	githubHost     = "github.com"
)

// gitServices is the closed set of valid ?service= values / smart-HTTP verbs.
var gitServices = map[string]bool{"git-upload-pack": true, "git-receive-pack": true}

// gitTokEntry caches one grant's minted GitHub App installation token. token +
// expiresAt are guarded by reMu, which ALSO single-flights the mint so info/refs
// and the following git-upload-pack don't stampede a double-mint — fatal for a
// single-use (approval-gated) grant, whose second mint 409s (broker ErrAlreadyMinted).
type gitTokEntry struct {
	reMu      sync.Mutex
	token     string
	expiresAt int64 // unix ms; 0 = unset
}

// handleGitBroker serves /wardyn/gh/<org>/<repo>[.git]/<rest>. It is
// method-agnostic at the switch; it validates the method against the smart-HTTP
// subpath itself.
func (p *Proxy) handleGitBroker(w http.ResponseWriter, r *http.Request) {
	orgRepo, rest, ok := parseGitBrokerPath(r.URL.Path)
	if !ok {
		http.Error(w, "invalid git broker path", http.StatusNotFound)
		return
	}
	// The allowlist lookup is the STRUCTURAL traversal + authorization gate
	// (mirrors handleBrokerApproval's uuid.Parse): anything not an exact granted
	// key — including ".."-bearing or extra-segment paths — 403s before any
	// github URL is formed.
	grantID, granted := p.gitGrants[orgRepo]
	if !granted {
		p.emitGitDecision(r, egress.Deny, ruleSourceGit)
		http.Error(w, "repository not granted to this run", http.StatusForbidden)
		return
	}
	if !validGitRest(r.Method, rest, r.URL.Query().Get("service")) {
		http.Error(w, "unsupported git request", http.StatusForbidden)
		return
	}

	token, err := p.gitToken(r.Context(), grantID)
	if err != nil {
		p.emitGitDecision(r, egress.Deny, ruleSourceGit)
		http.Error(w, "git credential unavailable: "+err.Error(), http.StatusBadGateway)
		return
	}

	target, err := p.vetURL("https://" + githubHost)
	if err != nil {
		p.emitGitDecision(r, egress.Deny, ruleSourceGit)
		http.Error(w, "git upstream vet failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Build the upstream URL from the MATCHED key + validated rest — never the raw
	// request path — so nothing attacker-controlled beyond a granted map key and a
	// closed-enum verb flows outbound.
	upstreamURL := "https://" + githubHost + "/" + orgRepo + ".git/" + rest
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}
	outReq, err := http.NewRequestWithContext(
		context.WithValue(r.Context(), vettedIPKey{}, target),
		r.Method, upstreamURL, r.Body)
	if err != nil {
		p.emitGitDecision(r, egress.Deny, ruleSourceGit)
		http.Error(w, "build git request: "+err.Error(), http.StatusBadGateway)
		return
	}
	// Stream the body straight through (no buffering) — upload-pack/receive-pack
	// packs can be large. Preserve the client's declared length; unknown => chunked.
	outReq.ContentLength = r.ContentLength
	copyHeader(outReq.Header, r.Header)
	removeHopByHop(outReq.Header)
	// Defensive: strip any sandbox-supplied credential before injecting ours, so a
	// rogue in-sandbox client can't smuggle its own onto the outbound request.
	outReq.Header.Del("Authorization")
	outReq.SetBasicAuth("x-access-token", token) // GitHub App installation-token auth
	outReq.Host = githubHost
	outReq.Header.Del("Host")

	p.emitGitDecision(r, egress.Allow, ruleSourceGit)

	resp, err := p.transport.RoundTrip(outReq)
	if err != nil {
		http.Error(w, "git upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	dst := w.Header()
	copyHeader(dst, resp.Header)
	removeHopByHop(dst)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body) // stream the pack back
}

// gitToken returns a cached (or freshly minted) installation token for grantID,
// re-minting only when unset or within injectRefreshMargin of expiry. reMu
// single-flights per grant so concurrent git sub-requests don't double-mint.
func (p *Proxy) gitToken(ctx context.Context, grantID uuid.UUID) (string, error) {
	p.gitTokMu.Lock()
	e, ok := p.gitTokens[grantID]
	if !ok {
		e = &gitTokEntry{}
		if p.gitTokens == nil {
			p.gitTokens = make(map[uuid.UUID]*gitTokEntry)
		}
		p.gitTokens[grantID] = e
	}
	p.gitTokMu.Unlock()

	e.reMu.Lock()
	defer e.reMu.Unlock()
	if e.token != "" && time.Now().Before(time.UnixMilli(e.expiresAt).Add(-injectRefreshMargin)) {
		return e.token, nil
	}
	// ponytail: one token per grant for the run. Auto-mintable grants re-mint past
	// TTL fine; approval-gated grants are single-use, so a run outliving the ~1h
	// installation-token TTL fails here on re-mint — a pre-existing ceiling.
	tok, expMs, err := p.mintGitToken(ctx, grantID)
	if err != nil {
		return "", err
	}
	e.token, e.expiresAt = tok, expMs
	return tok, nil
}

// mintGitToken calls the control-plane mint route server-side (run token injected
// by forwardToControlPlane) — the exact route wardyn-git-helper uses, so no broker
// change is needed. Returns the token and its expiry (unix ms; 0 if unparseable).
func (p *Proxy) mintGitToken(ctx context.Context, grantID uuid.UUID) (string, int64, error) {
	body, err := json.Marshal(map[string]string{"grant_id": grantID.String()})
	if err != nil {
		return "", 0, err
	}
	resp, err := p.forwardToControlPlane(ctx, http.MethodPost,
		"/api/v1/internal/credentials/mint", body, "application/json")
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxBrokeredBody))
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("mint status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var mr struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(respBody, &mr); err != nil {
		return "", 0, fmt.Errorf("decode mint response: %w", err)
	}
	if mr.Token == "" {
		return "", 0, fmt.Errorf("mint response missing token")
	}
	var expMs int64
	if t, perr := time.Parse(time.RFC3339, mr.ExpiresAt); perr == nil {
		expMs = t.UnixMilli()
	}
	return mr.Token, expMs, nil
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

// emitGitDecision records a brokered:git decision-log row (host github.com, port
// 443) so clone/fetch/push land in audit as allow/deny rows.
func (p *Proxy) emitGitDecision(r *http.Request, decision egress.Decision, ruleSource string) {
	if p.sink == nil {
		return
	}
	p.sink.emit(egress.DecisionLog{
		Request: egress.Request{
			RunID:  p.runID,
			Host:   githubHost,
			Port:   443,
			Method: strings.ToUpper(r.Method),
			Path:   r.URL.Path,
			Time:   p.now(),
		},
		Decision:   decision,
		RuleSource: ruleSource,
	})
}
