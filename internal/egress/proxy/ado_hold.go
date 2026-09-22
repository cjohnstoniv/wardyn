// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The Azure DevOps capability HOLD: when the REST gate meets a request needing a
// grantable capability this run does not hold, the request is parked while a
// person decides, and resumes on the same connection if they approve.
//
// The control plane decides everything that is a decision — the administrator's
// ceiling, the run's first-use mode, the per-run cap, whether a `once` approval
// is still unspent — and raises the approval itself. This file only asks,
// waits by approval id on the re-auth workflow machinery (credhold.go), and
// re-resolves exactly once when the answer arrives.
//
// WHAT A `once` APPROVAL BUYS: one request. The control plane spends it (the
// approval row's minted_jti) on the re-resolve, and every request parked on the
// same approval shares one workflow, so the workflow's onceTaken picks the ONE
// waiter that forwards. A `run` approval comes back as a capability in the
// resolve's own set and joins the run's standing set here, so later requests
// need no second ask.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// The sentences a sandbox reads when an escalation ends without an approval.
//
// DRAFT (M2 canon pending)
const (
	adoHoldDeniedRefusal = "Wardyn held this Azure DevOps request while a person was asked to approve more access, " +
		"and it was not approved."
	adoHoldTimedOutRefusal = "Wardyn held this Azure DevOps request while a person was asked to approve more access, " +
		"and nobody answered in time. The request is still in the console; retry once it is approved."
	adoHoldEndedRefusal  = "Wardyn held this Azure DevOps request for an approval, and the hold ended without one."
	adoHoldCappedRefusal = "Wardyn refused this Azure DevOps request: this run has asked for more access too many " +
		"times, and no further approval will be requested."
	adoHoldOnceSpentRefusal = "Wardyn refused this Azure DevOps request: the approval it waited for was for one " +
		"request, and another request used it. Retry to ask again."
)

// The sentences a sandbox reads when its Azure DevOps credential could not be
// renewed: a sign-in hold that ended without a sign-in, or any other failure.
//
// DRAFT (M2 canon pending)
const (
	adoSignInTimedOutRefusal = "Wardyn held this Azure DevOps request while its owner was asked to sign in to Azure " +
		"DevOps again, and nobody signed in before the hold expired. Nothing was substituted; retry once they have signed in."
	adoSignInEndedRefusal = "Wardyn asked this Azure DevOps request's owner to sign in to Azure DevOps again, and the " +
		"request ended before a sign-in arrived. Nothing was substituted."
	adoCredentialFailedRefusal = "Wardyn could not renew this run's Azure DevOps credential."
)

// adoCredentialRefusalFor is the sentence for a failed credential resolve on an
// Azure DevOps host, and the decision row's source: credential:reauth-timeout
// for the FIRST observer of a hold's expiry (one row per hold, as on the AWS
// lane), fallback otherwise. A control-plane refusal (a closed sign-in
// request, the per-run cap) keeps its own sentence.
func adoCredentialRefusalFor(err error, fallback string) (string, string) {
	switch {
	case errors.Is(err, errReauthTimedOutAgain):
		return adoSignInTimedOutRefusal, fallback
	case errors.Is(err, errReauthTimedOut):
		return adoSignInTimedOutRefusal, ruleSourceCredentialReauthTimeout
	case errors.Is(err, errReauthNoCredential):
		return adoSignInEndedRefusal, fallback
	}
	return adoControlPlaneRefusal(err, adoCredentialFailedRefusal), fallback
}

// refuseADOCredential answers a REST request whose credential could not be
// resolved, in Azure DevOps' own error shape. 403 for all of them, never 401:
// git and several tools read a 401 as "try another credential".
func (p *Proxy) refuseADOCredential(w http.ResponseWriter, r *http.Request, host string, port int, err error) {
	msg, src := adoCredentialRefusalFor(err, ruleSourceADODenied)
	if p.sink != nil {
		p.sink.emit(decisionLog(p.reqOf(r, host, port), egress.Deny, src))
	}
	slog.Warn("proxy: an Azure DevOps credential could not be resolved", "host", host, "err", reauthHoldError(err))
	writeADORefusal(w, http.StatusForbidden, "CredentialUnavailableException", msg)
}

// adoAsk is what the control plane is told about a held request besides the
// capability. repo and refClass are part of the approval's canonical identity;
// method and path are for the human-readable card and the audit row only.
type adoAsk struct {
	method, path, repo string
}

// adoRequestDetail extracts an adoAsk from r.
func adoRequestDetail(r *http.Request) adoAsk {
	path := adoRawPath(r)
	return adoAsk{method: r.Method, path: path, repo: adoRepoOf(path)}
}

// adoRepoOf is the repository a REST path addresses under
// `_apis/git/repositories/{repo}`, lower-cased, or "".
func adoRepoOf(path string) string {
	segs := strings.Split(strings.ToLower(strings.Trim(path, "/")), "/")
	for i := 0; i+3 < len(segs); i++ {
		if segs[i] == "_apis" && segs[i+1] == "git" && segs[i+2] == "repositories" {
			return segs[i+3]
		}
	}
	return ""
}

// awaitADOCapability asks the control plane for v's capability on host and, if
// it answers with an open approval, holds until that is decided. ok=true means
// the request may be forwarded; otherwise refusal is the sentence to answer
// with (fallback when nothing better is known).
//
// THE SEAM for the git broker: a git push arrives on a different door
// (pat_broker_entra.go) and answers in plain text, but the question and the
// wait are this one. It needs the host, the classified verdict and an adoAsk
// (method, path, repo); "once" there must cover one git OPERATION, which is the
// caller's to carry from the receive-pack advertisement to its pack upload.
func (p *Proxy) awaitADOCapability(ctx context.Context, host string, v adoscope.Verdict, ask adoAsk, fallback string) (bool, string) {
	inj := p.inject
	if inj == nil || inj.reauth == nil || inj.approvals == nil {
		return false, fallback
	}
	// A run-scoped widening is cached for the run. The live ceiling is held at
	// each control-plane ask, so an administrator narrowing the row stops the
	// NEXT escalation, not a capability this proxy already widened to.
	if inj.reauth.holds(v.Capability) {
		return true, ""
	}
	grantID, ok := inj.grantIDFor(host)
	if !ok {
		return false, fallback
	}
	q := url.Values{
		"capability": {string(v.Capability)},
		"first_use":  {string(p.policy.FirstUseMode())},
		"method":     {ask.method},
		"path":       {ask.path},
	}
	if ask.repo != "" {
		q.Set("repo", ask.repo)
	}
	if v.Capability == adoscope.CapPolicyBypass {
		// A protected-ref move (adoRunRefProtected). A ref inside the run's
		// own namespace classifies as code_write and is not this class.
		q.Set("ref_class", "protected")
	}
	out, err := resolveInjectionQuery(ctx, inj.base, inj.token.Get(), grantID, q, inj.client)
	if err == nil {
		// A 200 to a FIRST ask is a standing grant, which names the capability.
		// One that does not is a control plane that ignored the ask: refuse.
		if !slices.Contains(out.Capabilities, string(v.Capability)) {
			return false, fallback
		}
		inj.reauth.widen(v.Capability)
		return true, ""
	}
	var pending errReauthPending
	if !errors.As(err, &pending) {
		return false, adoControlPlaneRefusal(err, fallback)
	}
	hq := maps.Clone(q)
	if pending.state == capabilityPendingState {
		hq.Set("approval", pending.approvalID.String())
	}
	wf, fresh, admitted := inj.reauth.admitCapability(pending.approvalID, hq)
	if !admitted {
		return false, adoHoldCappedRefusal
	}
	if fresh {
		go wf.run(inj.base, inj.token, grantID, inj.client, inj.approvals)
	}
	res, herr := wf.await(ctx)
	switch {
	case errors.Is(herr, errReauthTimedOut):
		return false, adoHoldTimedOutRefusal
	case errors.Is(herr, reauthEndedAnswered):
		return false, adoHoldDeniedRefusal
	case herr != nil:
		return false, adoHoldEndedRefusal
	case slices.Contains(res.Capabilities, string(v.Capability)):
		inj.reauth.widen(v.Capability)
		return true, ""
	case !wf.onceTaken.CompareAndSwap(false, true):
		return false, adoHoldOnceSpentRefusal
	}
	return true, ""
}

// adoControlPlaneRefusal is the control plane's own refusal sentence (a 403 it
// wrote for this ask: above the ceiling, always_deny, a raised-and-refused
// request under deny_with_review), or fallback.
func adoControlPlaneRefusal(err error, fallback string) string {
	var se injectionStatusError
	if !errors.As(err, &se) || se.status != http.StatusForbidden {
		return fallback
	}
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(se.body), &body) != nil || body.Error == "" {
		return fallback
	}
	return body.Error
}

// grantIDFor is the grant host's injection resolves against.
func (i *injector) grantIDFor(host string) (uuid.UUID, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	e, ok := i.byHost[strings.ToLower(strings.TrimSuffix(host, "."))]
	if !ok {
		return uuid.Nil, false
	}
	return e.grantID, true
}
