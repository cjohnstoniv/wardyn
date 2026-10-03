// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// git through the /wardyn/git/ broker for a host the run's per-person Azure DevOps (Entra) grant covers.
// The REST gate refuses git on the intercepted connection, so this route is the one door git has to Azure
// DevOps on this lane; it enforces the same four things the REST gate does, in git's terms: the
// organisation pin (an Entra token carries no org claim), the capability check via adoscope.Permits (a
// push is decided on the pack POST, since the receive-pack advertisement is served to a read-only
// credential too), the content rules (before any capability ask), and the run's Azure DevOps credential
// (an Entra bearer or a PAT sent as Basic, as the control plane resolved it) via the same injector the
// REST lane's MITM uses.
//
// An Azure DevOps Server host (a grant host adoHostedHost does not name) takes the same door with the
// person's own token: the organisation pin is the collection's path, and only the smart-HTTP shapes
// adoServerGitPath names are forwarded. Its REST is never forwarded: adoscope classifies no Server host,
// so the gate refuses every request there, and a CONNECT this proxy won't terminate is refused outright.
//
// SECURITY: a refusal never reaches git as a 401 — git reads a 401 as a credential challenge and prints
// "could not read Username", which hides the reason.

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/egress"
)

const (
	ruleSourceADOGit       = "brokered:ado-git"
	ruleSourceADOGitDenied = "brokered:ado-git:denied"
)

// adoGitDrainLimit bounds how much of a refused push's pack is read and thrown
// away so git sees the refusal instead of a reset connection.
const adoGitDrainLimit = 64 << 20

// adoGitGrant answers the run's Azure DevOps grant for a broker host.
func (p *Proxy) adoGitGrant(host string) (ADOGrant, bool) {
	if p.adoGrants == nil {
		return ADOGrant{}, false
	}
	return p.adoGrants.ADOGrantFor(host)
}

// adoGitPush is a receive-pack command section: the refs it moves and the
// capabilities git asked for, which decide how a refusal is spelled.
type adoGitPush struct {
	refs []string
	caps []string
}

// serveADOGit serves one validated smart-HTTP request (verb is info/refs,
// git-upload-pack or git-receive-pack) for a host the Entra grant covers.
func (p *Proxy) serveADOGit(w http.ResponseWriter, r *http.Request, host, rest, verb string, grant ADOGrant) {
	keys, ok := adoGitKeys(r)
	if !ok {
		p.refuseADOGit(w, r, host, nil, nil,
			"Wardyn refused this git request: its path spells a project or repository name Azure DevOps would read as another.")
		return
	}
	if !adoOrgMatches(host, rest, grant.Organization) {
		p.refuseADOGit(w, r, host, nil, nil, fmt.Sprintf(
			"Wardyn refused this git request: this run is granted the %q Azure DevOps organisation only.", grant.Organization))
		return
	}
	// adoGitKeys drops empty segments but the forward sends rest as written, so an inner "//" is refused here.
	if !adoHostedHost(host) && (strings.Contains(strings.Trim(rest, "/"), "//") || !adoServerGitPath(keys, grant.Organization)) {
		p.refuseADOGit(w, r, host, nil, nil, fmt.Sprintf(
			"Wardyn refused this git request: on Azure DevOps Server this run reaches git only at %s/<project>/_git/<repository>.",
			strings.Trim(grant.Organization, "/")))
		return
	}

	var body io.Reader = r.Body
	need := adoscope.CapCodeRead
	var push *adoGitPush
	if verb == "git-receive-pack" {
		head, pp, msg := readADOGitPush(r)
		if msg != "" {
			p.refuseADOGit(w, r, host, nil, pp, msg)
			return
		}
		push, body = pp, io.MultiReader(bytes.NewReader(head), r.Body)
		// The run's branch rule first (adoRunBranchRule, the REST gate's too): a ref outside the run's own
		// branch is refused outright, never held for a capability.
		if msg := p.adoRunBranchRule(push.refs); msg != "" {
			p.refuseADOGit(w, r, host, nil, push, msg)
			return
		}
		// Content rules run BEFORE the capability check: nobody is asked to approve code_write for a
		// push the rules refuse. A probe moving no ref carries nothing to inspect and stays a read.
		if len(push.refs) > 0 {
			inspected, release, ok := p.applyPushRules(w, r, body, slog.String("host", host),
				func(ruleSource string) { p.emitPATDecision(r, host, egress.Deny, ruleSource) },
				nil, p.adoPushTarget(host, adoGitRepoKeys(r)))
			defer release()
			if !ok {
				return
			}
			body = inspected
		}
		// A command section moving no ref is git's auth probe ahead of a large pack (remote-curl's
		// probe_rpc): it writes nothing, so it's a read and never raises or spends an approval for the push.
		if len(push.refs) > 0 {
			need = adoscope.CapCodeWrite
		}
	}
	if v := adoGitVerdict(need, push); !adoscope.Permits(grant.Capabilities, v) &&
		!p.refuseADOGit(w, r, host, &v, push, fmt.Sprintf(
			"Wardyn refused this git request: it needs %q (%s), and this run was not granted it.",
			adoscope.Label(need), need)) {
		return
	}
	// A push forwarded under git_push_any_branch is marked as the App lane
	// marks one (ruleSourceGitNSOff), so the audit shows its refs went
	// unconfined.
	allowSrc := ruleSourceADOGit
	if push != nil && p.policy.GitPushAnyBranch() {
		allowSrc = ruleSourceGitNSOff
	}

	// refuseCredential answers a failed credential resolve; a client that hung up was never refused.
	refuseCredential := func(err error) {
		if r.Context().Err() != nil {
			return // the client is gone; the hold, if any, carries on without it
		}
		msg, src := adoCredentialRefusalFor(err, ruleSourceADOGitDenied)
		p.emitPATDecision(r, host, egress.Deny, src)
		if push != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, adoGitDrainLimit))
		}
		writeADOGitRefusal(w, push, "Wardyn's git broker: "+msg)
	}
	hdr, ok, err := p.inject.resolveCtx(r.Context(), host)
	if err != nil {
		refuseCredential(err)
		return
	}
	if !ok {
		p.emitPATDecision(r, host, egress.Deny, ruleSourceADOGitDenied)
		p.httpError(w, "resolve Azure DevOps credential", fmt.Errorf("no Azure DevOps credential is configured for %s", host), http.StatusBadGateway)
		return
	}
	registerHeaderCredential(hdr.value)

	// A lane that enforces content rules asks for a pack it can read: the
	// same no-thin rewrite, on the same trigger, the other two lanes make
	// (push_advert.go). Without it a shallow clone's push is thin and every
	// one of them is refused as uninspectable.
	noThin := p.noThinAdvert(r, verb)
	authorize := func(out *http.Request) {
		if noThin {
			out.Header.Set("Accept-Encoding", "identity")
		}
		out.Header.Set(hdr.name, hdr.value)
	}
	body, replay := adoGitReplayBody(r, verb, body)
	resp, ok := p.forwardBrokeredGit(w, r, host, rest, body, allowSrc, ruleSourceADOGitDenied, authorize)
	if !ok {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	// Azure DevOps refused the injected header itself: heal once before answering git, as the REST
	// door does (forwardInspectedLLM). A second refusal takes the ordinary path below.
	if adoCredentialRefused(resp) {
		fresh, retry, err := p.healADOHeader(r.Context(), host, hdr, replay != nil)
		if err != nil {
			drainClose(resp)
			refuseCredential(err)
			return
		}
		if retry {
			drainClose(resp)
			hdr = fresh
			registerHeaderCredential(hdr.value)
			p.emitPATDecision(r, host, egress.Allow, ruleSourceADOGitReresolved)
			again, ok := p.forwardBrokeredGit(w, r, host, rest, replay(), allowSrc, ruleSourceADOGitDenied, authorize)
			if !ok {
				return
			}
			resp = again
		}
	}
	switch {
	case p.refuseADOGitUpstream(w, r, host, rest, push, resp):
	case noThin:
		relayNoThinAdvert(w, resp) // relay(), with no-thin added to the advertisement
	default:
		relay(w, resp)
	}
}

// adoGitReplayBody readies a git request's body to be sent a second time and returns what the first
// attempt sends in body's place, with replay nil when it can't be: info/refs carries no body, an
// upload-pack POST is buffered when its whole body fits adoscope.MaxBodyPeek, and a push
// (receive-pack) is never sent twice.
func adoGitReplayBody(r *http.Request, verb string, body io.Reader) (io.Reader, func() io.Reader) {
	switch {
	case verb == "git-receive-pack":
		return body, nil
	case r.Body == nil || r.Body == http.NoBody:
		return body, func() io.Reader { return http.NoBody }
	case verb != "git-upload-pack":
		return body, nil
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, adoscope.MaxBodyPeek+1))
	if err != nil || len(buf) > adoscope.MaxBodyPeek {
		return io.MultiReader(bytes.NewReader(buf), r.Body), nil
	}
	return bytes.NewReader(buf), func() io.Reader { return bytes.NewReader(buf) }
}

// refuseADOGit is the ONE refusal point for git on the Azure DevOps Entra lane, and its hold point. held
// is non-nil only for a capability the run doesn't hold; that request is escalated (awaitADOCapability)
// and, if approved in time, refuseADOGit reports true and the caller forwards. Every other refusal is
// answered here in git's own terms.
func (p *Proxy) refuseADOGit(w http.ResponseWriter, r *http.Request, host string, held *adoscope.Verdict, push *adoGitPush, msg string) bool {
	if held != nil {
		var ok bool
		if ok, msg = p.awaitADOCapability(r.Context(), host, *held, adoGitAsk(r), msg); ok {
			return true
		}
	}
	p.emitPATDecision(r, host, egress.Deny, ruleSourceADOGitDenied)
	if push != nil {
		// Pack still on the wire: read it so git gets the answer rather than a reset connection.
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, adoGitDrainLimit))
	}
	if line := p.attributeRefusal(w); line != "" {
		msg += " " + line
	}
	writeADOGitRefusal(w, push, msg)
	return false
}

// adoGitVerdict is the verdict a git request is held to: a push carries its refs, as a REST ref move
// does, so a held ask can say whether they lie outside the run's own branch.
func adoGitVerdict(need adoscope.Capability, push *adoGitPush) adoscope.Verdict {
	v := adoscope.Verdict{Capability: need}
	if push != nil {
		v.Refs = push.refs
	}
	return v
}

// adoGitAsk describes a held git request for the approval: its method, the broker-stripped path, and the
// repository — the segment after _git, keyed exactly as the REST gate's adoRepoOf keys it.
func adoGitAsk(r *http.Request) adoAsk {
	_, rest, _ := parsePATBrokerPath(r.URL.Path)
	ask := adoAsk{method: r.Method, path: rest}
	if keys, ok := adoGitKeys(r); ok {
		if i := slices.Index(keys, "_git"); i >= 0 && i+1 < len(keys) {
			ask.repo = keys[i+1]
		}
	}
	return ask
}

// adoGitKeys is every segment of a git request's broker-stripped path as adoscope.NameKey reads it, taken
// from the path as it ARRIVED (EscapedPath), never the decoded Path (decoding first would decode a name
// holding "%" twice). ok=false when any segment is one adoscope.NameKey refuses, so a spelling the REST
// gate refuses is refused here too.
func adoGitKeys(r *http.Request) ([]string, bool) {
	_, rest, _ := parsePATBrokerPath(r.URL.EscapedPath())
	var keys []string
	for _, seg := range strings.Split(strings.Trim(rest, "/"), "/") {
		if seg == "" {
			continue
		}
		k := adoscope.NameKey(seg)
		if k == "" {
			return nil, false
		}
		keys = append(keys, k)
	}
	return keys, true
}

// adoServerGitPath reports whether keys (adoGitKeys) spell an Azure DevOps Server smart-HTTP endpoint
// under collection: <collection>/<project>/_git/<repository>/ or <collection>/_git/<repository>/ (the
// project-named repository's short form), followed by exactly info/refs, git-upload-pack or
// git-receive-pack. Anything else on a Server host — a path outside the collection, a deeper or shorter
// one, one with no _git — is refused rather than forwarded under the person's token.
func adoServerGitPath(keys []string, collection string) bool {
	coll := strings.ToLower(strings.TrimSpace(collection))
	if !adoServerCollection(keys, coll) {
		return false
	}
	tail := keys[len(strings.Split(strings.Trim(coll, "/"), "/")):]
	i := slices.Index(tail, "_git")
	if (i != 0 && i != 1) || i+2 > len(tail) {
		return false
	}
	verb := tail[i+2:]
	return slices.Equal(verb, []string{"info", "refs"}) ||
		slices.Equal(verb, []string{"git-upload-pack"}) || slices.Equal(verb, []string{"git-receive-pack"})
}

// adoGitRepoKeys is adoGitKeys truncated to the repository itself — org, project, "_git", and repo name,
// verb segments dropped — so a held push's pushTarget.repo names the same string adoRESTTarget builds from
// the REST route. A missing "_git" here is an invariant broken, not a request to answer for; the empty
// repo it falls back to still keys as its own approval.
func adoGitRepoKeys(r *http.Request) []string {
	keys, ok := adoGitKeys(r)
	if !ok {
		return nil
	}
	if i := slices.Index(keys, "_git"); i >= 0 && i+1 < len(keys) {
		return keys[:i+2]
	}
	return nil
}

// writeADOGitRefusal answers git in its own terms, never as a 401. A push that asked for report-status
// gets a 200 receive-pack result with an error unpack status and every ref "ng" with the reason (git
// prints "! [remote rejected] <ref> (<reason>)"); anything else gets a 403 with a plain-text body.
func writeADOGitRefusal(w http.ResponseWriter, push *adoGitPush, msg string) {
	if push == nil || !slices.ContainsFunc(push.caps, func(c string) bool { return c == "report-status" || c == "report-status-v2" }) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, msg+"\n")
		return
	}
	var status bytes.Buffer
	status.WriteString(pktLine("unpack refused by Wardyn\n"))
	for _, ref := range push.refs {
		status.WriteString(pktLine("ng " + ref + " " + msg + "\n"))
	}
	status.WriteString("0000")

	body := status.Bytes()
	band := 0
	switch {
	case slices.Contains(push.caps, "side-band-64k"):
		band = 65515
	case slices.Contains(push.caps, "side-band"):
		band = 995
	}
	if band > 0 {
		var out bytes.Buffer
		out.WriteString(pktLine("\x02" + msg + "\n"))
		for b := body; len(b) > 0; {
			n := min(len(b), band)
			out.WriteString(pktLine("\x01" + string(b[:n])))
			b = b[n:]
		}
		out.WriteString("0000")
		body = out.Bytes()
	}
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// pktLine frames s as one git pkt-line.
func pktLine(s string) string { return fmt.Sprintf("%04x", len(s)+4) + s }

// readADOGitPush reads a receive-pack command section and returns its bytes for the forward, the refs it
// moves and the capabilities git asked for. msg is the refusal when it can't be read.
func readADOGitPush(r *http.Request) (head []byte, push *adoGitPush, msg string) {
	if encs := r.Header.Values("Content-Encoding"); len(encs) > 1 ||
		(len(encs) == 1 && encs[0] != "" && !strings.EqualFold(encs[0], "identity")) {
		return nil, nil, "Wardyn refused this git push: an encoded push body cannot be checked."
	}
	var seen bytes.Buffer // what the parser consumed, bounded by its own cap
	head, err := readReceivePackCommands(io.TeeReader(r.Body, &seen), "")
	push = &adoGitPush{}
	for b := seen.Bytes(); len(b) >= 4; {
		n, perr := strconv.ParseUint(string(b[:4]), 16, 32)
		if perr != nil || n < 5 || int(n) > len(b) {
			break
		}
		cmd, caps, hasCaps := strings.Cut(string(b[4:n]), "\x00")
		b = b[n:]
		if hasCaps && push.caps == nil {
			push.caps = strings.Fields(caps)
		}
		cmd = strings.TrimSuffix(cmd, "\n")
		if parts := strings.SplitN(cmd, " ", 3); err == nil && !strings.HasPrefix(cmd, "shallow ") && len(parts) == 3 {
			push.refs = append(push.refs, parts[2])
		}
	}
	if err != nil {
		return nil, push, "Wardyn refused this git push: " + err.Error()
	}
	return head, push, ""
}
