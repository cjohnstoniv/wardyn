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
			p.refuseADOGit(w, r, host, nil, nil, msg)
			return
		}
		push, body = pp, io.MultiReader(bytes.NewReader(head), r.Body)
		// A command section that moves no ref is git's auth probe ahead of a
		// large pack (remote-curl's probe_rpc): it writes nothing, so it is a
		// read and never raises, or spends, an approval meant for the push.
		switch {
		case slices.ContainsFunc(push.refs, p.adoGitRefProtected):
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
	if err != nil || !ok {
		if err == nil {
			err = fmt.Errorf("no Azure DevOps credential is configured for %s", host)
		}
		p.emitPATDecision(r, host, egress.Deny, ruleSourceADOGitDenied)
		p.httpError(w, "resolve Azure DevOps credential", err, http.StatusBadGateway)
		return
	}
	registerHeaderCredential(hdr.value)

	resp, ok := p.forwardBrokeredGit(w, r, host, rest, body, ruleSourceADOGit, ruleSourceADOGitDenied,
		func(out *http.Request) { out.Header.Set(hdr.name, hdr.value) })
	if !ok {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized {
		// Azure DevOps refused the person's credential. Relayed as a 401, git
		// would prompt for a username instead of saying so.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		slog.Warn("proxy: Azure DevOps refused the brokered git credential", "host", host)
		writeADOGitRefusal(w, push,
			"Wardyn's git broker: Azure DevOps refused this run's Azure DevOps sign-in (401). The owner may need to sign in to Azure DevOps again.")
		return
	}
	relay(w, resp)
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
// broker-stripped path, and the repository — the segment after _git.
func adoGitAsk(r *http.Request) adoAsk {
	_, rest, _ := parsePATBrokerPath(r.URL.Path)
	ask := adoAsk{method: r.Method, path: rest}
	segs := strings.Split(strings.ToLower(strings.Trim(rest, "/")), "/")
	if i := slices.Index(segs, "_git"); i >= 0 && i+1 < len(segs) {
		ask.repo = segs[i+1]
	}
	return ask
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
// the capabilities git asked for. msg is the refusal when it cannot be read.
func readADOGitPush(r *http.Request) (head []byte, push *adoGitPush, msg string) {
	if encs := r.Header.Values("Content-Encoding"); len(encs) > 1 ||
		(len(encs) == 1 && encs[0] != "" && !strings.EqualFold(encs[0], "identity")) {
		return nil, nil, "Wardyn refused this git push: an encoded push body cannot be checked."
	}
	head, err := readReceivePackCommands(r.Body, "")
	if err != nil {
		return nil, nil, "Wardyn refused this git push: " + err.Error()
	}
	push = &adoGitPush{}
	for b := head; len(b) >= 4; {
		n, _ := strconv.ParseUint(string(b[:4]), 16, 32)
		if n == 0 {
			break
		}
		cmd, caps, _ := strings.Cut(string(b[4:n]), "\x00")
		b = b[n:]
		if caps != "" {
			push.caps = strings.Fields(caps)
		}
		cmd = strings.TrimSuffix(cmd, "\n")
		if strings.HasPrefix(cmd, "shallow ") {
			continue
		}
		push.refs = append(push.refs, strings.SplitN(cmd, " ", 3)[2])
	}
	return head, push, ""
}

// adoGitRefProtected is the REST gate's protected-ref predicate (every ref is
// protected until a grant carries a branch-policy list) with one exception:
// a ref inside this run's own branch namespace, refs/heads/wardyn/<run-id>/…,
// which agent-run checks the work tree out onto and nothing else writes.
func (p *Proxy) adoGitRefProtected(ref string) bool {
	prefix := BranchNSPrefix(p.runID)
	if strings.HasPrefix(ref, prefix) && len(ref) > len(prefix) {
		return false
	}
	return adoRefProtected(ref)
}
