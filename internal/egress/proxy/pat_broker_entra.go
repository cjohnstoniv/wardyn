// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// git through the /wardyn/git/ broker for a host the run's per-person Azure
// DevOps (Entra) grant covers.
//
// The REST gate (ado_gate.go) refuses git on the intercepted connection, so
// this route is the one door git has to Azure DevOps on this lane. It does
// what the REST gate does, in git's terms:
//
//   - THE ORGANISATION PIN on every request. An Entra access token carries no
//     organisation claim (measured), so only the URL binds it.
//   - THE CAPABILITY CHECK, through adoscope.Permits. The token carries every
//     scope the person consented to, so this is what bounds the run. Clone and
//     fetch need read. The receive-pack ADVERTISEMENT is a read too: Azure
//     DevOps serves it to a read-only credential (measured), so a push is
//     decided on the pack POST, from the ref updates it carries.
//   - THE CONTENT RULES (push_rules.go), first for a push: deny, hold for
//     review, or refuse on an unattended run, before any capability is asked.
//   - THE CREDENTIAL is the person's bearer, resolved through the same
//     injector entry the REST lane's MITM uses for this host.
//
// A refusal never reaches git as a 401: git reads a 401 as a credential
// challenge and prints "could not read Username", which hides the reason.

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
	if _, ok := adoGitKeys(r); !ok {
		p.refuseADOGit(w, r, host, nil, nil,
			"Wardyn refused this git request: its path spells a project or repository name Azure DevOps would read as another.")
		return
	}
	if !adoOrgMatches(host, rest, grant.Organization) {
		p.refuseADOGit(w, r, host, nil, nil, fmt.Sprintf(
			"Wardyn refused this git request: this run is granted the %q Azure DevOps organisation only.", grant.Organization))
		return
	}

	var body io.Reader = r.Body
	need := adoscope.CapRead
	var push *adoGitPush
	if verb == "git-receive-pack" {
		head, pp, msg := readADOGitPush(r)
		if msg != "" {
			p.refuseADOGit(w, r, host, nil, pp, msg)
			return
		}
		push, body = pp, io.MultiReader(bytes.NewReader(head), r.Body)
		// CONTENT rules, the same step the other two lanes run and on the same
		// trigger, and BEFORE the capability check below: nobody is asked to
		// approve policy_bypass for a push the rules refuse, and a push held
		// for review is decided before any capability hold. A probe that moves
		// no ref carries nothing to inspect and stays the read it is. What
		// the pack does not carry cannot be compared with Azure DevOps' trees
		// (push_forge.go reads GitHub only), so it keeps the strict reading.
		if len(push.refs) > 0 {
			inspected, release, ok := p.applyPushRules(w, r, body, slog.String("host", host),
				func(ruleSource string) { p.emitPATDecision(r, host, egress.Deny, ruleSource) },
				nil, p.adoPushTarget(host, rest))
			defer release()
			if !ok {
				return
			}
			body = inspected
		}
		// A command section that moves no ref is git's auth probe ahead of a
		// large pack (remote-curl's probe_rpc): it writes nothing, so it is a
		// read and never raises, or spends, an approval meant for the push.
		switch {
		case slices.ContainsFunc(push.refs, p.adoRunRefProtected):
			need = adoscope.CapPolicyBypass
		case len(push.refs) > 0:
			need = adoscope.CapCodeWrite
		}
	}
	if v := adoGitVerdict(need, push); !adoscope.Permits(grant.Capabilities, v) &&
		!p.refuseADOGit(w, r, host, &v, push, fmt.Sprintf(
			"Wardyn refused this git request: it needs %q (%s), and this run was not granted it.",
			adoscope.Label(need), need)) {
		return
	}

	hdr, ok, err := p.inject.resolveCtx(r.Context(), host)
	if err != nil && r.Context().Err() != nil {
		return // the client is gone; the hold, if any, carries on without it
	}
	if err != nil {
		msg, src := adoCredentialRefusalFor(err, ruleSourceADOGitDenied)
		p.emitPATDecision(r, host, egress.Deny, src)
		if push != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, adoGitDrainLimit))
		}
		writeADOGitRefusal(w, push, "Wardyn's git broker: "+msg)
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
	resp, ok := p.forwardBrokeredGit(w, r, host, rest, body, ruleSourceADOGit, ruleSourceADOGitDenied,
		func(out *http.Request) {
			if noThin {
				out.Header.Set("Accept-Encoding", "identity")
			}
			out.Header.Set(hdr.name, hdr.value)
		})
	if !ok {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case p.refuseADOGitUpstream(w, r, host, rest, push, resp):
	case noThin:
		relayNoThinAdvert(w, resp) // relay(), with no-thin added to the advertisement
	default:
		relay(w, resp)
	}
}

// refuseADOGit is the ONE refusal point for git on the Azure DevOps Entra
// lane, and its hold point. held is non-nil only for a capability the run does
// not hold; that request is escalated (awaitADOCapability) and, if a person
// approves it in time, refuseADOGit reports true and the caller forwards.
// Every other refusal, and an escalation that ends without an approval, is
// answered here in git's own terms.
//
// Only a pack upload or an upload-pack/advertisement the run cannot read ever
// reaches the hold: the receive-pack advertisement is a read (F-LIVE-8), so a
// `once` approval raised for a push is spent by its pack upload and never by
// the advertisement in front of it.
func (p *Proxy) refuseADOGit(w http.ResponseWriter, r *http.Request, host string, held *adoscope.Verdict, push *adoGitPush, msg string) bool {
	if held != nil {
		var ok bool
		if ok, msg = p.awaitADOCapability(r.Context(), host, *held, adoGitAsk(r), msg); ok {
			return true
		}
	}
	p.emitPATDecision(r, host, egress.Deny, ruleSourceADOGitDenied)
	if push != nil {
		// The pack is still on the wire: read it so git gets the answer rather
		// than a reset connection.
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, adoGitDrainLimit))
	}
	writeADOGitRefusal(w, push, msg)
	return false
}

// adoGitVerdict is the verdict a git request is held to. Refs ride only a
// policy_bypass verdict: the control plane reads refs as a protected-ref move,
// which a push inside the run's own namespace is not.
func adoGitVerdict(need adoscope.Capability, push *adoGitPush) adoscope.Verdict {
	v := adoscope.Verdict{Capability: need}
	if need == adoscope.CapPolicyBypass {
		v.Refs = push.refs
	}
	return v
}

// adoGitAsk describes a held git request for the approval: its method, the
// broker-stripped path, and the repository — the segment after _git, keyed
// exactly as the REST gate's adoRepoOf keys it.
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

// adoGitKeys is every segment of a git request's broker-stripped path as
// adoscope.NameKey reads it — the rule the REST gate classifies with — taken
// from the path as it ARRIVED (EscapedPath). Never the decoded Path: decoding
// first and keying after would decode a name holding "%" twice. ok=false when
// any segment is one that rule refuses (a trailing dot or edge space the
// service trims, an escape decoding to a separator, a double encoding), so a
// spelling the REST gate refuses is refused here too rather than keyed as a
// second repository.
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

// writeADOGitRefusal answers git in its own terms, never as a 401.
//
// A push that asked for report-status gets a 200 receive-pack result whose
// unpack status is an error (never "unpack ok" for a pack nobody read) and
// whose every ref is "ng" with the reason, so git prints
// "! [remote rejected] <ref> (<reason>)"; over side-band the reason also goes
// out on the progress band, which git prints as "remote: <reason>". Anything
// else gets a 403 with a plain-text body, which git prints as "remote: …".
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

// readADOGitPush reads a receive-pack command section through the same
// parser the branch-namespace confinement uses (an empty prefix admits every
// well-formed ref) and returns its bytes for the forward, the refs it moves and
// the capabilities git asked for. msg is the refusal when it cannot be read;
// push then still carries the first command's capabilities, when there were
// any, so the refusal can be spelled in receive-pack terms.
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
