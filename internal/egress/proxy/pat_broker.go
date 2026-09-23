// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The git_pat broker: git-over-HTTPS to a NON-GitHub forge with the PAT held
// proxy-side and never resident in the sandbox.
//
// Why this exists: the GitHub lane already keeps its credential out of the
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
// The mechanism removes the tunnel rather than trying to inject into it.
// agent-run rewrites the granted hosts to a PLAIN-HTTP broker path
// (url.<broker>/git/<host>/.insteadOf https://<host>/), so the proxy terminates
// the request itself, mints server-side, and sets Basic auth on the outbound
// leg. The sandbox speaks cleartext HTTP to its own sidecar over a loopback-
// equivalent hop and never holds the credential.
//
// What this does not do: a PAT carries whatever scope the operator issued it
// with, and Wardyn cannot narrow it — there is no ADO/GitLab equivalent of a
// scoped installation token. So this makes the credential NON-RESIDENT; it does
// not make it least-privilege. The allowlist here is per-HOST for exactly that
// reason: a per-repo key would imply a confinement the credential does not have.

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

// emitPATDecision records a git_pat broker decision against the FORGE, the way
// the GitHub lane's emitGitDecision records github.com:443.
//
// handlePATBroker is the ONE brokered local route that does not forward to the
// control plane — it re-originates to the granted forge — so emitLocalDecision's
// premise ("its host AND port are the recorded upstream", i.e. the control
// plane's) is false for it. Logged through that helper, a git-receive-pack to
// gitlab.com landed in the decision stream as the control-plane host:port,
// indistinguishable from a mint or an approval poll, with the forge in no field
// of the row — and a run granted several forges produced rows that could not be
// told apart at all. An egress review reads this stream.
//
// host is "" only on the one path where the request named no forge this proxy
// could parse; the empty host is the truthful record there, and still tells that
// row apart from a real forge's.
func (p *Proxy) emitPATDecision(r *http.Request, host string, decision egress.Decision, ruleSource string) {
	if p.sink == nil {
		return
	}
	p.sink.emit(decisionLog(p.reqOf(r, host, 443), decision, ruleSource))
}

// patForwardSafe reports whether a sandbox-supplied upstream path and query can
// be forwarded as the SAME value the smart-HTTP verb check ran on.
//
// The verb check reads the DECODED path (r.URL.Path), so any character that is
// structurally significant when the URL is rebuilt lets the two diverge: '#' and
// '?' start a fragment/query, so "/api/v4/user#/info/refs" satisfies the
// "/info/refs" suffix and then leaves the proxy as a bare "/api/v4/user" — the
// brokered PAT delivered to the forge's REST API. A backslash and the control
// characters are refused on the same "no second spelling" ground, and a "."/".."
// segment because the forge, not this proxy, would normalize it away.
//
// The forge path is arbitrarily deep here (gitlab subgroups, "o/p/_git/r" on
// Azure DevOps), so this cannot be the GitHub lane's closed enum; it is the
// widest predicate under which the checked path and the forwarded path are one
// string. Ordinary forge segments — including a space in an Azure DevOps project
// name — still pass and are escaped by net/url on the way out.
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
	// The allowlist lookup is the authorization gate, exactly as it is on the
	// GitHub lane: a host this run was not granted 403s before any upstream URL
	// is formed, so a traversal or an extra segment cannot reach the network.
	grant, granted := p.patGrants[host]
	ado, adoLane := p.adoGitGrant(host)
	if !granted && !adoLane {
		p.emitPATDecision(r, host, egress.Deny, ruleSourcePATDenied)
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
	// patForwardSafe first: the verb check runs on the DECODED path, so a rest
	// that can re-split the rebuilt URL would let the checked value and the
	// forwarded value differ — the one way this lane could become the
	// "credentialed proxy to the whole forge" the comment above forbids.
	if !patForwardSafe(rest, r.URL.RawQuery) || !validGitRest(r.Method, verb, r.URL.Query().Get("service")) {
		p.emitPATDecision(r, host, egress.Deny, ruleSourcePATDenied)
		http.Error(w, "unsupported git request", http.StatusForbidden)
		return
	}
	// A host the run's Azure DevOps Entra grant covers takes that lane: the
	// person's bearer, the organisation pin and the capability gate
	// (pat_broker_entra.go). Every other host continues below exactly as before.
	if adoLane {
		p.serveADOGit(w, r, host, rest, verb, ado)
		return
	}

	// Push branch-namespace confinement, OFF unless the operator opted this proxy
	// in with WARDYN_GIT_PAT_BROKER_ENFORCE_BRANCH_NS (PATBranchNSEnforced — its
	// own switch, its own default, and the reasoning lives there). With the
	// switch off this whole block is skipped and the lane behaves exactly as it
	// did: nothing is buffered, no row changes.
	//
	// Opted in, it is the SAME confinement the App lane applies, through the same
	// confinePush step and refused in the same words — one rule, two brokers,
	// which is why it reuses the brokered:git:branch-ns* rule sources instead of
	// minting a second vocabulary an operator would have to learn. The run's own
	// git_push_any_branch still opts out, and then the allow row says so
	// (brokered:git:branch-ns-off) rather than reading like a confined push.
	//
	// A refusal happens BEFORE patToken, so the refused request mints nothing
	// itself — but the push's own discovery (GET info/refs) came first and
	// already did (push_rules.go), and a content-rules verdict that has to read
	// the forge looks the credential up before it is reached.
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
	// CONTENT rules, on the SAME terms as the App lane and entered
	// independently of the branch-namespace block above: this lane terminates
	// and holds the whole request either way, so gating WHAT a push may carry
	// on a WHERE switch the operator may never have turned on would leave a
	// policy that reads as governed enforcing nothing (push_rules.go). A run
	// that sets no push_rules buffers nothing and behaves exactly as it did.
	// A refused push is never forwarded. What the pack does not carry is
	// compared with the forge's own trees on github.com only (patForge).
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

	// Content rules need a pack they can read, and a client only sends one when
	// the server asks. The advertisement this lane relays gets the same no-thin
	// rewrite the App lane's does, under the same condition, because this lane
	// enforces the same rules — advertising on one lane only would make the
	// rules a false-refusal machine on the other (push_advert.go). The header
	// rides the lane's own authorize hook, which is where forwardBrokeredGit
	// lets a lane touch the outbound request; the advertisement must be
	// PARSEABLE to be rewritten, so identity is asked for explicitly rather
	// than merely deleting the header.
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
// re-originates the validated request to the granted forge over HTTPS with the
// sandbox's own credential headers stripped and the lane's credential set by
// authorize, recording denySrc on a pre-flight failure and allowSrc only after a
// successful round trip. On ok=false it has already answered the sandbox.
func (p *Proxy) forwardBrokeredGit(w http.ResponseWriter, r *http.Request, host, rest string, reqBody io.Reader,
	allowSrc, denySrc string, authorize func(*http.Request),
) (*http.Response, bool) {
	// Build the upstream URL from the MATCHED allowlist key + the validated rest
	// and let net/url do the escaping — never a raw concatenation of the decoded
	// path, which re-parses as a NEW url whose Path is whatever a '#'/'?' left in
	// front of it. This is the invariant the GitHub-lane sibling states and keeps
	// (git_broker.go: "never the raw request path"); patForwardSafe above is what
	// makes the two spellings the same string.
	upstream := (&url.URL{Scheme: "https", Host: host, Path: rest, RawQuery: r.URL.RawQuery}).String()
	// Vet the destination through the SAME guard every other egress takes: a
	// granted host that resolves into private space is still denied. The broker
	// is a credential path, not a bypass of the IP guard.
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
	// Strip any sandbox-supplied credential BEFORE injecting ours, so a rogue
	// in-sandbox client cannot smuggle its own onto the outbound request. Same
	// order, and the same reason, as the GitHub lane.
	//
	// F104: through stripSandboxCredentials — the ONE definition (inject.go) —
	// not a local Header.Del("Authorization"). The narrower spelling left
	// Private-Token (GitLab's own access-token header, on the very forge kind
	// this lane exists for), X-Api-Key, Api-Key, X-Auth-Token and Cookie on the
	// request beside the brokered Basic auth, so the FORGE chose which
	// credential won while the decision row still read as brokered egress.
	stripSandboxCredentials(outReq.Header, "")
	authorize(outReq)
	outReq.Host = host
	outReq.Header.Del("Host")

	// roundTripUpstream, not the transport directly: see the GitHub lane
	// (git_broker.go) — the same HTTP/2 fallback applies to a forge.
	resp, err := p.roundTripUpstream(outReq)
	if err != nil {
		p.failUpstream(w, err, &egress.DecisionLog{Request: p.reqOf(r, host, 443)}, host, "git upstream error")
		return nil, false
	}
	p.emitPATDecision(r, host, egress.Allow, allowSrc)
	return resp, true
}

// patToken returns the brokered PAT with the git username the host expects.
//
// It goes through brokeredToken — the SAME per-grant cache, single-flight and
// 409-pending wait the GitHub lane uses — rather than calling the mint route
// itself. Both guards are load-bearing here for the same reason they are there:
// one clone is TWO sub-requests (GET info/refs, then POST git-upload-pack), and
// an approval-gated git_pat grant is single-use, so a per-request mint 409s
// ErrAlreadyMinted on the second and the clone fails outright; and a first mint
// against such a grant 409s PENDING, which must be waited out (the proxy holds
// the run token and no caller can retry for it), not returned as a 502 before
// any human could have approved.
//
// The username falls back to the grant's configured value and then to "pat":
// Azure DevOps ignores the username entirely as long as one is present, GitLab
// wants "oauth2", and an operator override rides the grant. An EMPTY username
// is the one value that fails on every forge, so it is never sent.
func (p *Proxy) patToken(ctx context.Context, g PATGrant) (token, username string, err error) {
	// The mask must be registered under the username THIS lane sends, so the
	// fallback chain is handed to brokeredToken rather than re-derived after it
	// (F120): masking base64(<mint user>:tok) while the wire carries
	// base64("pat":tok) protects a rendering that never leaves the process.
	wireUser := func(mintUser string) string { return cmp.Or(mintUser, g.Username, "pat") }
	tok, user, err := p.brokeredToken(ctx, g.GrantID, wireUser)
	if err != nil {
		return "", "", err
	}
	username = wireUser(user)
	// Register the brokered PAT with the process-global mask registry before it
	// can reach any output stream — the raw token AND the base64(username +
	// ":" + tok) that SetBasicAuth puts on the wire, under the username THIS
	// lane sends (which may differ from the mint's).
	//
	// Trust boundary (F155, same root cause as the git lane's): procRegistry is
	// what maskDecisionBytes consults for every sandbox-facing error body
	// (Proxy.httpError) and every decision-log line, and the mask is exact-bytes
	// per RENDERING. registerBasicAuthCredential (inject.go) is the one definition.
	registerBasicAuthCredential(username, tok)
	return tok, username, nil
}
