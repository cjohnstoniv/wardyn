// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The git_pat broker: git-over-HTTPS to a NON-GitHub forge with the PAT held
// proxy-side and never resident in the sandbox.
//
// WHY THIS EXISTS. The GitHub lane already keeps its credential out of the
// sandbox — /wardyn/gh/<org>/<repo> mints an installation token server-side and
// injects it. Every other forge fell to the git_pat grant, whose own type doc
// calls it "the OPPOSITE of api_key": git-over-HTTPS is an opaque CONNECT tunnel
// the proxy cannot inject Basic-auth into, so the PAT was handed to the
// in-sandbox credential helper and was resident for the life of the run.
//
// That asymmetry was the whole gap: GitHub users got a short-lived, per-repo,
// ref-confined token that never touched the sandbox; GitLab and Azure DevOps
// users got a long-lived operator PAT sitting in the agent's process.
//
// THE MECHANISM removes the tunnel rather than trying to inject into it.
// agent-run rewrites the granted hosts to a PLAIN-HTTP broker path
// (url.<broker>/git/<host>/.insteadOf https://<host>/), so the proxy terminates
// the request itself, mints server-side, and sets Basic auth on the outbound
// leg. The sandbox speaks cleartext HTTP to its own sidecar over a loopback-
// equivalent hop and never holds the credential.
//
// WHAT THIS DOES NOT DO. A PAT carries whatever scope the operator issued it
// with, and Wardyn cannot narrow it — there is no ADO/GitLab equivalent of a
// scoped installation token. So this makes the credential NON-RESIDENT; it does
// not make it least-privilege. The allowlist here is per-HOST for exactly that
// reason: a per-repo key would imply a confinement the credential does not have.

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// routePATBroker is the sandbox-facing prefix: /wardyn/git/<host>/<rest>.
const routePATBroker = "/wardyn/git/"

const (
	ruleSourcePAT       = "brokered:git-pat"
	ruleSourcePATDenied = "brokered:git-pat:denied"
)

// parsePATBrokerPath splits /wardyn/git/<host>/<rest> into a lower-cased host
// and the remaining upstream path.
//
// The host is returned EXACTLY as the allowlist key it will be looked up
// against, and anything that is not a plain host — an empty segment, a path
// traversal, a userinfo or port smuggled into the segment — simply fails that
// lookup. The allowlist IS the validation, the same way the GitHub lane's
// per-repo map is.
func parsePATBrokerPath(p string) (host, rest string, ok bool) {
	trimmed := strings.TrimPrefix(p, routePATBroker)
	if trimmed == p {
		return "", "", false
	}
	host, rest, found := strings.Cut(trimmed, "/")
	if !found || host == "" {
		return "", "", false
	}
	return strings.ToLower(host), "/" + rest, true
}

// handlePATBroker serves /wardyn/git/<host>/<rest> for a git_pat-granted host.
func (p *Proxy) handlePATBroker(w http.ResponseWriter, r *http.Request) {
	host, rest, ok := parsePATBrokerPath(r.URL.Path)
	if !ok {
		p.emitLocalDecision(r, egress.Deny, ruleSourcePATDenied, nil)
		http.Error(w, "invalid git broker path", http.StatusNotFound)
		return
	}
	// The allowlist lookup is the authorization gate, exactly as it is on the
	// GitHub lane: a host this run was not granted 403s before any upstream URL
	// is formed, so a traversal or an extra segment cannot reach the network.
	grant, granted := p.patGrants[host]
	if !granted {
		p.emitLocalDecision(r, egress.Deny, ruleSourcePATDenied, nil)
		http.Error(w, "host not granted to this run", http.StatusForbidden)
		return
	}
	// Same smart-HTTP surface the GitHub lane admits: refs discovery and the two
	// pack endpoints, nothing else. A broker that forwarded arbitrary paths would
	// be a credentialed proxy to the whole forge — the REST API included.
	//
	// The GitHub lane hands validGitRest a bare tail because parseGitBrokerPath
	// strips <org>/<repo> for it. A forge path is arbitrarily deep, so this lane
	// takes the last segment instead — the ONE exception being refs discovery,
	// whose two segments are the whole surface it must match.
	verb := rest[strings.LastIndex(rest, "/")+1:]
	if strings.HasSuffix(rest, "/info/refs") {
		verb = "info/refs"
	}
	if !validGitRest(r.Method, verb, r.URL.Query().Get("service")) {
		p.emitLocalDecision(r, egress.Deny, ruleSourcePATDenied, nil)
		http.Error(w, "unsupported git request", http.StatusForbidden)
		return
	}

	token, username, err := p.patToken(r.Context(), grant)
	if err != nil {
		p.emitLocalDecision(r, egress.Deny, ruleSourcePATDenied, nil)
		p.httpError(w, "mint git_pat", err, http.StatusBadGateway)
		return
	}

	upstream := "https://" + host + rest
	if q := r.URL.RawQuery; q != "" {
		upstream += "?" + q
	}
	// Vet the destination through the SAME guard every other egress takes: a
	// granted host that resolves into private space is still denied. The broker
	// is a credential path, not a bypass of the IP guard.
	target, _, err := p.egressTarget(host, 443)
	if err != nil {
		p.emitLocalDecision(r, egress.Deny, ruleSourcePATDenied, nil)
		p.httpError(w, "vet git host", err, http.StatusForbidden)
		return
	}

	outReq, err := http.NewRequestWithContext(
		context.WithValue(r.Context(), vettedIPKey{}, target),
		r.Method, upstream, r.Body)
	if err != nil {
		p.emitLocalDecision(r, egress.Deny, ruleSourcePATDenied, nil)
		p.httpError(w, "build git request", err, http.StatusBadGateway)
		return
	}
	outReq.ContentLength = r.ContentLength
	copyHeader(outReq.Header, r.Header)
	removeHopByHop(outReq.Header)
	// Strip any sandbox-supplied credential BEFORE injecting ours, so a rogue
	// in-sandbox client cannot smuggle its own onto the outbound request. Same
	// order, and the same reason, as the GitHub lane.
	outReq.Header.Del("Authorization")
	outReq.SetBasicAuth(username, token)
	outReq.Host = host
	outReq.Header.Del("Host")

	p.emitLocalDecision(r, egress.Allow, ruleSourcePAT, nil)

	resp, err := p.transport.RoundTrip(outReq)
	if err != nil {
		p.httpError(w, "git upstream error", err, http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	relay(w, resp)
}

// patToken mints the PAT server-side, returning it with the git username the
// host expects.
//
// The username falls back to the grant's configured value and then to "pat":
// Azure DevOps ignores the username entirely as long as one is present, GitLab
// wants "oauth2", and an operator override rides the grant. An EMPTY username
// is the one value that fails on every forge, so it is never sent.
func (p *Proxy) patToken(ctx context.Context, g PATGrant) (token, username string, err error) {
	tok, user, _, status, body, err := p.callMintGit(ctx, g.GrantID)
	if err != nil {
		return "", "", err
	}
	if status != http.StatusOK {
		return "", "", fmt.Errorf("mint status %d: %s", status, strings.TrimSpace(string(body)))
	}
	username = cmp.Or(user, g.Username, "pat")
	// Register the minted PAT with the process-global mask registry before it
	// can reach any output stream — the raw token AND the base64(username +
	// ":" + tok) that SetBasicAuth (:142) puts on the wire.
	//
	// TRUST BOUNDARY (F155, same root cause as the git lane's): procRegistry is
	// what maskDecisionBytes consults for every sandbox-facing error body
	// (Proxy.httpError) and every decision-log line, and the mask is exact-bytes
	// per RENDERING. This lane registered NOTHING at all, so a transport error
	// quoting the outbound request would have carried the operator's PAT out
	// verbatim. registerBasicAuthCredential (inject.go) is the one definition.
	registerBasicAuthCredential(username, tok)
	return tok, username, nil
}
