// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The Azure DevOps capability HOLD: when the REST gate meets a request
// needing a capability this run does not hold, the request is parked while a
// person decides, and resumes on the same connection if approved. The
// control plane decides everything (ceiling, first-use mode, per-run cap,
// unspent `once` approvals) and raises the approval; this file only asks,
// waits on the re-auth workflow (credhold.go), and re-resolves once.
//
// A `once` approval buys ONE request (spent on the re-resolve; parked
// requests share one workflow whose onceTaken picks the ONE forwarder). A
// `run` approval joins the run's standing set, so later requests ask again.

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

// adoCredentialRefusalFor is the sentence for a failed credential resolve,
// and the decision row's source (credential:reauth-timeout for the FIRST
// observer of a hold's expiry, fallback otherwise); a control-plane refusal keeps its own sentence.
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

// refuseADOCredential answers a REST request whose credential failed, in
// ADO's own error shape — always 403, never 401 (tools read that as "try another credential").
func (p *Proxy) refuseADOCredential(w http.ResponseWriter, r *http.Request, host string, port int, err error) {
	msg, src := adoCredentialRefusalFor(err, ruleSourceADODenied)
	if p.sink != nil {
		p.sink.emit(decisionLog(p.reqOf(r, host, port), egress.Deny, src))
	}
	slog.Warn("proxy: an Azure DevOps credential could not be resolved", "host", host, "err", reauthHoldError(err))
	writeADORefusal(w, http.StatusForbidden, "CredentialUnavailableException", msg)
}

// adoAsk is what the control plane is told about a held request besides the
// capability: repo and ref class are the approval's canonical identity;
// method/path are for the card and audit row only.
type adoAsk struct {
	method, path, repo string
}

func adoRequestDetail(r *http.Request) adoAsk {
	path := adoRawPath(r)
	return adoAsk{method: r.Method, path: path, repo: adoRepoOf(path)}
}

// adoRepoOf is the repository a REST path addresses under
// `_apis/git/repositories/{repo}` (decoded/case-folded via adoscope.NameKey,
// matching the git broker's key), or "". It is the NAME the sandbox chose,
// not a resolved identity — the approval's scope is keyed on it, so a denied
// repo re-asked under its GUID is a new ask. Bounded: every ask spends one of
// the run's maxCapabilityHolds (16).
func adoRepoOf(path string) string {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i := range segs {
		segs[i] = adoscope.NameKey(segs[i])
	}
	for i := 0; i+3 < len(segs); i++ {
		if segs[i] == "_apis" && segs[i+1] == "git" && segs[i+2] == "repositories" {
			return segs[i+3]
		}
	}
	return ""
}

// awaitADOCapability asks the control plane for v's capability on host and,
// if it answers with an open approval, holds until decided (ok=true means
// forward, else refusal is the sentence to answer with). THE SEAM for the
// git broker: a git push arrives on a different door (pat_broker_entra.go);
// "once" there must cover one git OPERATION, carried by the caller to its
// pack upload.
func (p *Proxy) awaitADOCapability(ctx context.Context, host string, v adoscope.Verdict, ask adoAsk, fallback string) (bool, string) {
	inj := p.inject
	if inj == nil || inj.reauth == nil || inj.approvals == nil {
		return false, fallback
	}
	// A run-scoped widening is cached; the live ceiling is re-checked at each
	// ask, so a narrowed row stops the NEXT escalation, not one already widened.
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
		// A protected-ref move; a ref in the run's own namespace is code_write, not this class.
		q.Set("ref_class", "protected")
	}
	out, err := resolveInjectionQuery(ctx, inj.base, inj.token.Get(), grantID, q, inj.client)
	if err == nil {
		// A 200 to a FIRST ask is a standing grant naming the capability; else refuse (an ignored ask).
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

// adoControlPlaneRefusal is the control plane's own refusal sentence (a 403
// for above-ceiling, always_deny, or a refused deny_with_review ask), or fallback.
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
