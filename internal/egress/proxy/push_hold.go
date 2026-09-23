// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The HELD push: push_rules.require_review_paths.
//
// A push no deny rule refuses, but which introduces a path a review rule
// matches, is parked on its own connection while an admin decides. The
// question is a push_content approval (types.PushContentScope) raised through
// the same internal route and polled on the same cadence as a first-use
// egress hold (approvals.go); git waits on the open request, which it does not
// abort unless http.lowSpeedLimit and http.lowSpeedTime are set.
//
// THE KEY. A push is identified by its repository, the refs it updates, the
// commits it sets them to and a digest of every path a review rule matched —
// not by its pack bytes, which a retry repacks. The control plane deduplicates
// PENDING rows on the whole scope, and pushHolds maps the same key to the
// row's id and its outcome, so:
//
//   - an approved push is forwarded, and so is every later push of the same
//     commits to the same repository and branch (git's retry after the hold,
//     or after a failed forward) without a second question — identical commits
//     are identical content. The same commits to ANOTHER repository or branch
//     are another question, and are held again;
//   - a denied push stays denied for the rest of the run: the same push is
//     refused at once, without asking again;
//   - a hold that times out leaves its row PENDING, and a retry waits on that
//     same row instead of raising a second one;
//   - a row that EXPIRED or was CANCELLED without a decision is forgotten, so
//     the next push of those commits asks afresh.
//
// UNATTENDED RUNS DO NOT HOLD. A non-interactive run has nobody to be asked, so
// a review match is refused at once (brokered:git:push-held-unattended) and no
// row is raised; the control plane refuses such a raise too
// (internal/api's admitPushContentRaise).
//
// BOUNDED. At most maxPushHoldsActive pushes are parked at once, each holding
// its buffer against scanRetained, and at most maxPushHoldKeys distinct pushes
// are remembered per run; past either the push is refused, never waved on.
//
// Paths ride the structured log (capped at maxDeniedPathsLogged, count exact)
// and the refusal body (at most maxDeniedPathsInBody), never the decision log,
// exactly as a deny refusal's do.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/gitpack"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// ruleSourceGitPushHeld marks a brokered push refused after it was held
	// for review: denied, not decided in time, or not askable.
	ruleSourceGitPushHeld = "brokered:git:push-held"
	// ruleSourceGitPushHeldUnattended marks a push a review rule matched on a
	// run nobody is driving, refused without raising an approval.
	ruleSourceGitPushHeldUnattended = "brokered:git:push-held-unattended"
	// defaultPushHold is hold_seconds unset, matching defaultGitApprovalTimeout.
	defaultPushHold = 120 * time.Second
	// maxPushHoldsActive bounds pushes parked at once; each holds a goroutine,
	// a connection and its buffer.
	maxPushHoldsActive = 16
	// maxPushHoldKeys bounds the distinct pushes one run's sidecar remembers.
	maxPushHoldKeys = 256
)

// pushTarget is what a held push's approval names besides its paths and
// commits: the repository as the run's grant names it, and the credential the
// forwarded push authenticates with.
type pushTarget struct{ repo, actsAs string }

// appPushTarget is the GitHub App lane's target.
func appPushTarget(orgRepo string, grantID uuid.UUID) pushTarget {
	return pushTarget{repo: githubHost + "/" + orgRepo, actsAs: string(types.GrantGitHubToken) + ":" + grantID.String()}
}

// patPushTarget is the git_pat lane's: the host and the path git named,
// less the receive-pack verb — "dev.azure.com/org/project/_git/repo" on Azure
// DevOps.
func patPushTarget(host, rest string, g PATGrant) pushTarget {
	repo := strings.Trim(strings.TrimSuffix(rest, "/git-receive-pack"), "/")
	return pushTarget{repo: host + "/" + repo, actsAs: string(types.GrantGitPAT) + ":" + g.GrantID.String()}
}

// adoPushTarget is the Azure DevOps Entra lane's: the person's injected
// credential, named by the grant their bearer resolves from ("api_key:<id>").
// The control plane recognises that grant as the lane's and labels it with the
// person (internal/api's pushActsAs).
func (p *Proxy) adoPushTarget(host, rest string) pushTarget {
	t := patPushTarget(host, rest, PATGrant{})
	t.actsAs = ""
	if p.inject != nil {
		if id, ok := p.inject.grantIDFor(host); ok {
			t.actsAs = string(types.GrantAPIKey) + ":" + id.String()
		}
	}
	return t
}

// pushReview is what the review rules matched in one push.
type pushReview struct {
	paths []string // every matched path, in match order
	why   string   // why the forge did not clear some, when it was asked
	cmds  []gitpack.Command
}

// pushHolds is the per-run held-push state. The zero value (plus unattended)
// is ready to use.
type pushHolds struct {
	unattended bool

	mu     sync.Mutex
	byKey  map[string]*heldPush
	active int
}

// heldPush is one push's approval and what it came to.
type heldPush struct {
	id    uuid.UUID
	state approvalState // apPending, apApproved or apDenied
}

// pushScope builds the approval's requested_scope from the sorted matched
// paths, and the key pushHolds files it under.
func pushScope(paths []string, cmds []gitpack.Command, t pushTarget) (types.PushContentScope, string) {
	h := sha256.New()
	for _, pth := range paths {
		h.Write([]byte(pth))
		h.Write([]byte{0})
	}
	var refs, commits []string
	for _, c := range cmds {
		refs = append(refs, c.Ref)
		if strings.Trim(c.New, "0") != "" { // a delete sets its ref to nothing
			commits = append(commits, c.New)
		}
	}
	slices.Sort(refs)
	slices.Sort(commits)
	s := types.PushContentScope{
		Repo:        t.repo,
		Branch:      strings.Join(slices.Compact(refs), ", "),
		ActsAs:      t.actsAs,
		Paths:       paths[:min(len(paths), types.PushContentMaxPaths)],
		PathsTotal:  len(paths),
		Commits:     slices.Compact(commits),
		PathsDigest: hex.EncodeToString(h.Sum(nil)),
	}
	// The WHOLE question, as the control plane dedups it: an approval of these
	// commits for one repository and branch says nothing about another.
	return s, strings.Join([]string{s.Repo, s.Branch, s.PathsDigest, strings.Join(s.Commits, ",")}, "\x00")
}

// holdPush decides a push the review rules matched: forwarded (true) once an
// admin approves it, refused otherwise. It writes the refusal itself.
func (p *Proxy) holdPush(w http.ResponseWriter, r *http.Request, rules *pushRuleSet, rv pushReview,
	target pushTarget, subject slog.Attr, deny func(ruleSource string)) bool {
	paths := slices.Compact(slices.Sorted(slices.Values(rv.paths)))
	scope, key := pushScope(paths, rv.cmds, target)
	slog.WarnContext(r.Context(), "wardyn-proxy: git push touches paths that need review",
		slog.String("run_id", p.runID.String()),
		subject,
		slog.Int("review_paths", len(paths)),
		slog.Any("paths", sampleOf(paths)),
		slog.Any("commits", scope.Commits),
		slog.String("reason", rv.why),
		slog.Bool("unattended", p.pushHolds.unattended))
	refuse := func(src, headline, remedy string) bool {
		deny(src)
		http.Error(w, pathsBody("wardyn: "+headline, paths, "matched", rv.why, remedy), http.StatusForbidden)
		return false
	}
	if p.pushHolds.unattended {
		return refuse(ruleSourceGitPushHeldUnattended,
			"this push touches paths that need an admin's review, and this run is unattended, so nobody can be asked",
			"remove these paths from the push, or run the task interactively")
	}
	id, state, why := p.pushHolds.admit(key)
	switch {
	case state == apApproved:
		return true
	case state == apDenied:
		return refuse(ruleSourceGitPushHeld, "an admin denied this push, which touches paths that need review",
			"remove these paths from the push")
	case why != "":
		return refuse(ruleSourceGitPushHeld, why, "retry the push later")
	}
	defer p.pushHolds.leave()
	return p.awaitPushDecision(w, r, rules.hold, id, scope, key, refuse)
}

// admit files key and takes an active slot. A push already decided comes back
// with its state and takes none; one that cannot be held comes back with why.
func (h *pushHolds) admit(key string) (uuid.UUID, approvalState, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.byKey[key]
	switch {
	case e != nil && e.state != apPending:
		return e.id, e.state, ""
	case e == nil && len(h.byKey) >= maxPushHoldKeys:
		return uuid.Nil, apNone, "this run has held too many different pushes for review; no more will be asked about"
	case h.active >= maxPushHoldsActive:
		return uuid.Nil, apNone, "too many pushes are already waiting for review"
	}
	h.active++
	if e == nil {
		return uuid.Nil, apPending, ""
	}
	return e.id, apPending, ""
}

func (h *pushHolds) leave() {
	h.mu.Lock()
	h.active--
	h.mu.Unlock()
}

// record stores what id came to, unless key has since been filed under a
// different approval (discriminate, then store — approvals.go ResolveWait's
// order, for its reason).
func (h *pushHolds) record(key string, id uuid.UUID, state approvalState) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.byKey == nil {
		h.byKey = map[string]*heldPush{}
	}
	e := h.byKey[key]
	switch {
	case e == nil && state != apNone:
		h.byKey[key] = &heldPush{id: id, state: state}
	case e == nil, e.id != id:
	case state == apNone:
		delete(h.byKey, key)
	default:
		e.state = state
	}
}

// awaitPushDecision raises (or rejoins) the push's approval and polls it until
// it is decided or the hold runs out.
func (p *Proxy) awaitPushDecision(w http.ResponseWriter, r *http.Request, hold time.Duration, id uuid.UUID,
	scope types.PushContentScope, key string, refuse func(src, headline, remedy string) bool) bool {
	const headline = "this push touches paths that need an admin's review"
	if p.approval == nil {
		return refuse(ruleSourceGitPushHeld, headline+", and this proxy has no way to ask for one", "retry the push")
	}
	ctx, cancel := context.WithTimeout(r.Context(), hold)
	defer cancel()
	if id == uuid.Nil {
		body, err := json.Marshal(struct {
			Kind           types.ApprovalKind     `json:"kind"`
			RequestedScope types.PushContentScope `json:"requested_scope"`
		}{types.ApprovalPushContent, scope})
		if err == nil {
			id, err = p.approval.raiseBytes(ctx, body)
		}
		if err != nil {
			return refuse(ruleSourceGitPushHeld, headline+", and the request for one could not be raised",
				"retry the push")
		}
		p.pushHolds.record(key, id, apPending)
	}
	tick := time.NewTicker(holdPollInterval)
	defer tick.Stop()
	for {
		// Polled at once as well as on each tick: a retry rejoining a row that
		// was decided while nobody was waiting is answered without a delay.
		if ar, ok := p.approval.fetch(ctx, id); ok {
			switch ar.State {
			case types.ApprovalApproved:
				p.pushHolds.record(key, id, apApproved)
				return true
			case types.ApprovalDenied:
				p.pushHolds.record(key, id, apDenied)
				return refuse(ruleSourceGitPushHeld, "an admin denied this push, which touches paths that need review",
					"remove these paths from the push")
			case types.ApprovalExpired, types.ApprovalCancelled:
				p.pushHolds.record(key, id, apNone)
				return refuse(ruleSourceGitPushHeld, headline+", and the request closed without a decision",
					"push again to ask again")
			}
		}
		select {
		case <-ctx.Done():
			return refuse(ruleSourceGitPushHeld,
				fmt.Sprintf("%s, and nobody decided within %s", headline, hold),
				"the request is still in the console; push the same commits again once it is approved")
		case <-tick.C:
		}
	}
}
