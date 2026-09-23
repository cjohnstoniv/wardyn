// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// push_rules on the Azure DevOps REST door.
//
// The git broker is not the only way a per-person Azure DevOps run can put
// content in a repository: Git Pushes - Create takes the files inline in a
// JSON body, and a dozen other REST routes put content on a branch without
// naming it (internal/adoscope's ClassifyContent). With push rules set, the
// REST gate therefore answers every such request the way the git door answers
// a push, and BEFORE any capability escalation, so nobody is asked to grant a
// capability for a write the rules refuse:
//
//   - a REST push is read path by path (adoscope.ParsePush): a deny match is
//     refused (brokered:git:push-rules), a review match is held for an admin
//     (holdPush — the same push_content approval, the same unattended refusal),
//     anything else continues to the capability check;
//   - a REST push the gate cannot read whole, and any route that writes content
//     without naming its paths, is refused (brokered:git:push-uninspectable) —
//     the REST counterpart of the git door refusing a ref update with no pack.
//
// A REST push names no commit until the service creates one, so the held
// approval's commits field carries the SHA-256 of the request body instead:
// an approval covers exactly the bytes that were asked about.

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/gitpack"
)

// restChangeMode marks a REST push path for the matcher: not a regular-file
// mode, so a path is matched as well when a pattern could match beneath it —
// a REST change may name a folder, and a folder rename moves everything under
// it.
const restChangeMode = "rest"

// governADOContent applies the run's push rules to one Azure DevOps REST
// request the capability gate has not already refused outright. It reports
// whether the request may continue; on false it has answered.
func (p *Proxy) governADOContent(w http.ResponseWriter, r *http.Request, host string, port int, grant ADOGrant) bool {
	rules := p.policy.contentRules()
	if rules == nil {
		return true
	}
	deny := func(src string) {
		if p.sink != nil {
			p.sink.emit(decisionLog(p.reqOf(r, host, port), egress.Deny, src))
		}
	}
	refuse := func(src, msg string) bool {
		deny(src)
		slog.WarnContext(r.Context(), "wardyn-proxy: Azure DevOps REST write refused by push content rules",
			slog.String("run_id", p.runID.String()), slog.String("host", host),
			slog.String("rule_source", src), slog.String("reason", msg))
		writeADORefusal(w, http.StatusForbidden, "GitPushRulesException", msg)
		return false
	}
	req := adoscope.Request{Method: r.Method, Host: host, Path: adoRawPath(r), Header: r.Header,
		Org: grant.Organization, BodyWithheld: true}
	t, err := adoscope.ClassifyContent(req)
	if err == nil && t.Write == adoscope.ContentPush || errors.Is(err, adoscope.ErrNeedsBody) {
		peek, msg := adoPeekBody(r)
		if msg != "" {
			return refuse(ruleSourceGitPackBlind, msg)
		}
		req.BodyWithheld, req.BodyPeek = false, peek
		t, err = adoscope.ClassifyContent(req)
	}
	var push adoscope.Push
	if err == nil && t.Write == adoscope.ContentPush {
		push, err = adoscope.ParsePush(req)
	}
	switch {
	case err != nil:
		return refuse(ruleSourceGitPackBlind, "Wardyn refused this Azure DevOps request: this run's push rules cannot read what it writes: "+err.Error())
	case t.Write == adoscope.ContentOpaque:
		return refuse(ruleSourceGitPackBlind, "Wardyn refused this Azure DevOps request: it writes repository content by a route that does not "+
			"name the paths it changes, so this run's push rules cannot be applied to it. Push the change with git instead.")
	case t.Write == adoscope.NoContentWrite:
		return true
	}
	changes := make([]gitpack.Change, len(push.Paths))
	for i, pth := range push.Paths {
		changes[i] = gitpack.Change{Path: pth, Mode: restChangeMode}
	}
	subject := slog.String("host", host)
	denied, _, err := match(rules.deny, changes)
	if err == nil && len(denied) > 0 {
		slog.WarnContext(r.Context(), "wardyn-proxy: Azure DevOps REST push denied by content rules",
			slog.String("run_id", p.runID.String()), subject,
			slog.Int("denied_paths", len(denied)), slog.Any("paths", sampleOf(denied)))
		deny(ruleSourceGitRules)
		writeADORefusal(w, http.StatusForbidden, "GitPushRulesException", deniedPathsBody(denied, ""))
		return false
	}
	var review []string
	if err == nil {
		review, _, err = match(rules.review, changes)
	}
	if err != nil {
		return refuse(ruleSourceGitPackBlind, "Wardyn refused this Azure DevOps request: this run's push rules cannot be evaluated against it: "+err.Error())
	}
	if len(review) == 0 {
		return true
	}
	cmds := make([]gitpack.Command, len(push.Refs))
	for i, ref := range push.Refs {
		cmds[i] = gitpack.Command{Ref: ref, New: push.Digest}
	}
	return p.holdPush(w, r, rules, pushReview{paths: review, cmds: cmds}, p.adoRESTTarget(host, grant, t.RepoPath), subject, deny)
}

// adoRESTTarget names a REST push's repository the way the git door does —
// "dev.azure.com/<org>/<project>/_git/<repo>", or "<org>.visualstudio.com/…"
// — and the person's credential it would be sent with (adoPushTarget).
func (p *Proxy) adoRESTTarget(host string, grant ADOGrant, repoPath string) pushTarget {
	if !strings.HasSuffix(host, ".visualstudio.com") {
		repoPath = strings.ToLower(grant.Organization) + "/" + repoPath
	}
	return p.adoPushTarget(host, repoPath)
}
