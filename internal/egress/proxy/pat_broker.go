// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The git_pat broker: git-over-HTTPS to a NON-GitHub forge with the PAT held
// proxy-side and never resident in the sandbox. agent-run rewrites granted
// hosts to a plain-HTTP broker path (.insteadOf https://<host>/), so the
// proxy terminates the request, mints server-side, and sets Basic auth on
// the outbound leg — the sandbox never holds the credential.
//
// This makes the credential NON-RESIDENT, not least-privilege: a PAT carries
// whatever scope the operator issued it with and Wardyn cannot narrow it, so
// the allowlist here is per-HOST (a per-repo key would imply a confinement
// the credential does not have).

import (
	"cmp"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
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
// and the remaining upstream path. The allowlist lookup IS the validation:
// anything that is not a plain host (traversal, userinfo, smuggled port)
// simply fails to match a grant.
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

// emitPATDecision records a git_pat broker decision against the FORGE, the way
// the GitHub lane's emitGitDecision records github.com:443 — NOT through
// emitLocalDecision, whose "host:port is the control plane's" premise is false
// here (this route re-originates to the granted forge). host is "" only when
// the request named no forge this proxy could parse.
func (p *Proxy) emitPATDecision(r *http.Request, host string, decision egress.Decision, ruleSource string) {
	if p.sink == nil {
		return
	}
	p.sink.emit(decisionLog(p.reqOf(r, host, 443), decision, ruleSource))
}

// patForwardSafe reports whether a sandbox-supplied upstream path and query
// can be forwarded as the SAME value the smart-HTTP verb check ran on. The
// verb check reads the DECODED path, so '#'/'?' (start a fragment/query and
// let "/api/v4/user#/info/refs" pass the suffix check while forwarding a
// bare "/api/v4/user"), backslash, control characters, and "."/".." segments
// are all refused — the widest predicate under which the checked and
// forwarded paths stay one string.
func patForwardSafe(rest, rawQuery string) bool {
	for _, s := range []string{rest, rawQuery} {
		for _, c := range s {
			if c < 0x20 || c == 0x7f || c == '#' || c == '?' || c == '\\' {
				return false
			}
		}
	}
	for _, seg := range strings.Split(rest, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// handlePATBroker serves /wardyn/git/<host>/<rest> for a git_pat-granted host.
func (p *Proxy) handlePATBroker(w http.ResponseWriter, r *http.Request) {
	host, rest, ok := parsePATBrokerPath(r.URL.Path)
	if !ok {
		p.emitPATDecision(r, "", egress.Deny, ruleSourcePATDenied)
		http.Error(w, "invalid git broker path", http.StatusNotFound)
		return
	}
	// The allowlist lookup is the authorization gate: an ungranted host 403s
	// before any upstream URL is formed.
	grant, granted := p.patGrants[host]
	ado, adoLane := p.adoGitGrant(host)
	if !granted && !adoLane {
		p.emitPATDecision(r, host, egress.Deny, ruleSourcePATDenied)
		http.Error(w, "host not granted to this run", http.StatusForbidden)
		return
	}
	// Same smart-HTTP surface the GitHub lane admits: refs discovery and the two
	// pack endpoints, nothing else, or this broker becomes a credentialed proxy
	// to the whole forge. The forge path is arbitrarily deep (gitlab subgroups,
	// ADO's "o/p/_git/r"), so this lane takes the last segment as verb rather
	// than the GitHub lane's closed enum.
	verb := rest[strings.LastIndex(rest, "/")+1:]
	if strings.HasSuffix(rest, "/info/refs") {
		verb = "info/refs"
	}
	// patForwardSafe first: it must run before the verb check can be bypassed
	// by a rest that re-splits the rebuilt URL differently.
	if !patForwardSafe(rest, r.URL.RawQuery) || !validGitRest(r.Method, verb, r.URL.Query().Get("service")) {
		p.emitPATDecision(r, host, egress.Deny, ruleSourcePATDenied)
		http.Error(w, "unsupported git request", http.StatusForbidden)
		return
	}
	// A host the run's Azure DevOps Entra grant covers takes that lane instead
	// (pat_broker_entra.go).
	if adoLane {
		p.serveADOGit(w, r, host, rest, verb, ado)
		return
	}

	// Push branch-namespace confinement, OFF unless the operator opted this proxy
	// in with the pat scope of WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS (PATBranchNSEnforced;
	// #203 folds the standalone WARDYN_GIT_PAT_BROKER_ENFORCE_BRANCH_NS into it).
	// Opted in, it is the SAME confinement the App lane applies (confinePush,
	// same rule sources), so a run's git_push_any_branch still opts out. A
	// refusal happens BEFORE patToken, so the refused request mints nothing.
	var reqBody io.Reader = r.Body
	allowSrc := ruleSourcePAT
	if verb == "git-receive-pack" && PATBranchNSEnforced() {
		if p.policy.GitPushAnyBranch() {
			allowSrc = ruleSourceGitNSOff
		} else {
			body, ok := p.confinePush(w, r, slog.String("host", host),
				func(ruleSource string) { p.emitPATDecision(r, host, egress.Deny, ruleSource) })
			if !ok {
				return
			}
			reqBody = body
		}
	}
	// CONTENT rules, on the SAME terms as the App lane, entered independently
	// of the branch-namespace block above — gating content on a switch the
	// operator may never have enabled would make the policy enforce nothing.
	// A refused push is never forwarded.
	if verb == "git-receive-pack" {
		body, release, ok := p.applyPushRules(w, r, reqBody, slog.String("host", host),
			func(ruleSource string) { p.emitPATDecision(r, host, egress.Deny, ruleSource) },
			p.patForge(host, rest, grant), patPushTarget(host, rest, grant))
		defer release()
		if !ok {
			return
		}
		reqBody = body
	}

	token, username, err := p.patToken(r.Context(), grant)
	if err != nil {
		p.emitPATDecision(r, host, egress.Deny, ruleSourcePATDenied)
		p.httpError(w, "mint git_pat", err, http.StatusBadGateway)
		return
	}

	// The advertisement this lane relays gets the same no-thin rewrite the App
	// lane's does, under the same condition (push_advert.go): the advertisement
	// must be PARSEABLE to be rewritten, so identity is requested explicitly.
	noThin := p.noThinAdvert(r, verb)
	resp, ok := p.forwardBrokeredGit(w, r, host, rest, reqBody, allowSrc, ruleSourcePATDenied,
		func(out *http.Request) {
			if noThin {
				out.Header.Set("Accept-Encoding", "identity")
			}
			out.SetBasicAuth(username, token)
		})
	if !ok {
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if noThin {
		relayNoThinAdvert(w, resp) // relay(), with no-thin added to the advertisement
		return
	}
	relay(w, resp)
}

// forwardBrokeredGit is the outbound leg both /wardyn/git/ lanes share: it
// re-originates the validated request to the granted forge over HTTPS with
// the sandbox's own credential headers stripped and the lane's credential
// set by authorize. On ok=false it has already answered the sandbox.
func (p *Proxy) forwardBrokeredGit(w http.ResponseWriter, r *http.Request, host, rest string, reqBody io.Reader,
	allowSrc, denySrc string, authorize func(*http.Request),
) (*http.Response, bool) {
	// Build the upstream URL from the MATCHED allowlist key + the validated
	// rest and let net/url do the escaping — never a raw concatenation of the
	// decoded path (git_broker.go: "never the raw request path").
	upstream := (&url.URL{Scheme: "https", Host: host, Path: rest, RawQuery: r.URL.RawQuery}).String()
	// Vet the destination through the SAME guard every other egress takes: the
	// broker is a credential path, not a bypass of the IP guard.
	target, _, err := p.egressTarget(host, 443)
	if err != nil {
		p.emitPATDecision(r, host, egress.Deny, denySrc)
		p.httpError(w, "vet git host", err, http.StatusForbidden)
		return nil, false
	}

	outReq, err := http.NewRequestWithContext(
		context.WithValue(r.Context(), vettedIPKey{}, target),
		r.Method, upstream, reqBody)
	if err != nil {
		p.emitPATDecision(r, host, egress.Deny, denySrc)
		p.httpError(w, "build git request", err, http.StatusBadGateway)
		return nil, false
	}
	outReq.ContentLength = r.ContentLength
	copyHeader(outReq.Header, r.Header)
	removeHopByHop(outReq.Header)
	// Strip any sandbox-supplied credential BEFORE injecting ours, through
	// stripSandboxCredentials (inject.go, the ONE definition) — a local
	// Header.Del("Authorization") would leave Private-Token, X-Api-Key,
	// Api-Key, X-Auth-Token and Cookie for the forge to choose between.
	stripSandboxCredentials(outReq.Header, "")
	authorize(outReq)
	outReq.Host = host
	outReq.Header.Del("Host")

	// roundTripUpstream, not the transport directly: the same HTTP/2 fallback
	// as the GitHub lane (git_broker.go) applies to a forge.
	resp, err := p.roundTripUpstream(outReq)
	if err != nil {
		p.failUpstream(w, err, &egress.DecisionLog{Request: p.reqOf(r, host, 443)}, host, "git upstream error")
		return nil, false
	}
	p.emitPATDecision(r, host, egress.Allow, allowSrc)
	return resp, true
}

// patToken returns the brokered PAT with the git username the host expects.
// It goes through brokeredToken — the SAME per-grant cache, single-flight and
// 409-pending wait the GitHub lane uses — since one clone is TWO sub-requests
// (GET info/refs, then POST git-upload-pack) against a single-use grant, and
// a first mint's 409-pending must be waited out rather than returned as a 502.
//
// The username falls back to the grant's configured value and then to "pat"
// (ADO ignores it, GitLab wants "oauth2"); an EMPTY username fails on every
// forge, so it is never sent.
func (p *Proxy) patToken(ctx context.Context, g PATGrant) (token, username string, err error) {
	// The mask must be registered under the username THIS lane sends, so the
	// fallback chain is handed to brokeredToken rather than re-derived after it.
	wireUser := func(mintUser string) string { return cmp.Or(mintUser, g.Username, "pat") }
	tok, user, err := p.brokeredToken(ctx, g.GrantID, wireUser)
	if err != nil {
		return "", "", err
	}
	username = wireUser(user)
	// Register the brokered PAT with the process-global mask registry (the
	// raw token AND the base64(username+":"+tok) SetBasicAuth puts on the
	// wire) before it can reach any output stream. registerBasicAuthCredential
	// (inject.go) is the one definition.
	registerBasicAuthCredential(username, tok)
	return tok, username, nil
}
