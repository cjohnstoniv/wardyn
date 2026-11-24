// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/lifecycle"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The fill step: a person fills one of their own drives from a GitHub repository of one of their workspaces by
// creating their own exec run, composed by the server and passed through handleCreateRun like a workload launch.
// The run clones the repository with the person's own GitHub token into driveFillSource, and its exec task copies
// the clone into the drive. Wardyn refuses before any run when the person has no such token, when the drive is in
// use, and for a drive or repository it does not cover.

const (
	// driveFillSource is where the run clones the repository: strictly under /home/agent and outside every drive.
	driveFillSource = "/home/agent/seed-source"
	driveFillHost   = "github.com"
	driveFillMounts = "/proc/mounts"
)

// driveSeedRefusal is a refusal of the fill: its status, reason and sentence, or a server fault (err set) answered
// by writeServerError. runID is the run a refusal after the create belongs to.
type driveSeedRefusal struct {
	status int
	reason string
	msg    string
	err    error
	op     string
	runID  *uuid.UUID
}

func driveFillRefuse(status int, reason, msg string) *driveSeedRefusal {
	return &driveSeedRefusal{status: status, reason: reason, msg: msg}
}

func driveFillFault(op string, err error) *driveSeedRefusal {
	return &driveSeedRefusal{err: err, op: op, reason: "internal"}
}

// driveFillFromRun is a create-path refusal read as a fill refusal. One with no status of its own is a fault.
func driveFillFromRun(rr *runRefusal, op string) *driveSeedRefusal {
	if rr == nil {
		return nil
	}
	if rr.err != nil || rr.status == 0 {
		err := rr.err
		if err == nil {
			err = errors.New("the request was refused without a status")
		}
		return driveFillFault(op, err)
	}
	return driveFillRefuse(rr.status, rr.body.Reason, rr.body.Error)
}

// driveFillPlan is what the pre-checks settled for the create.
type driveFillPlan struct {
	grant  types.GrantSpec
	target string
}

type driveFillLaunchKey struct{}

// withDriveFillLaunch marks the create a fill makes, with the drive's name, so the queue refuses it rather than
// holding it.
func withDriveFillLaunch(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, driveFillLaunchKey{}, name)
}

// driveFillLaunchFrom reports whether ctx is a fill's create, and the drive it fills.
func driveFillLaunchFrom(ctx context.Context) (string, bool) {
	name, ok := ctx.Value(driveFillLaunchKey{}).(string)
	return name, ok
}

// driveFillBeforeCreate is a test seam between the pre-checks and the create; a no-op in production.
var driveFillBeforeCreate = func() {}

// handleFillMyDrive is POST /me/drives/{id}/seed: 202 with the run that fills the caller's own drive from a
// repository of one of their workspaces. Another person's drive, an admin drive and an id that names nothing answer
// the same 404 as the delete. From the drive decision on, every request writes one audit row.
func (s *Server) handleFillMyDrive(w http.ResponseWriter, r *http.Request) {
	owner, ok := memberDriveOwner(w, r)
	if !ok || s.refuseMemberDriveRate(w, owner) {
		return
	}
	id, ok := parseIDParam(w, r, "id", "drive")
	if !ok {
		return
	}
	var req client.MemberDriveSeedRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if status, reason, msg := driveFillBodyRefusal(&req); reason != "" {
		writeErrorReason(w, status, reason, msg)
		return
	}
	ctx := r.Context()
	var d types.UserDrive
	ms, ok := s.cfg.Store.(store.MemberDriveStore)
	gerr := store.ErrNotFound
	if ok {
		d, gerr = ms.GetMemberDriveOf(ctx, owner, id)
	}
	if errors.Is(gerr, store.ErrNotFound) {
		msg := driveNotFoundMsg(id.String())
		s.recordRefusal(ctx, r, authz.Deny(authz.ReasonDriveNotFound, "drives.member.seed", msg))
		writeErrorReason(w, http.StatusNotFound, string(authz.ReasonDriveNotFound), msg)
		return
	}
	if gerr != nil {
		d.ID = id
		s.driveFillAnswer(w, r, &d, req, driveFillFault("read the drive to fill", gerr))
		return
	}
	plan, refusal := s.driveFillPrecheck(ctx, r, ms, owner, d, req)
	if refusal != nil {
		s.driveFillAnswer(w, r, &d, req, refusal)
		return
	}
	driveFillBeforeCreate()
	cw := newQueueRefusalCapture()
	cr, err := driveFillCreateRequest(r, d, id, req, plan)
	if err != nil {
		s.driveFillAnswer(w, r, &d, req, driveFillFault("compose the fill run", err))
		return
	}
	s.handleCreateRun(cw, cr)
	if cw.status != http.StatusCreated {
		reason, msg, _ := cw.refused()
		if reason == reasonWorkspaceSeedNoBaseImage {
			msg = driveFillNoBaseImageMsg()
		}
		if reason == "" {
			s.driveFillAnswer(w, r, &d, req, driveFillFault("create the fill run", fmt.Errorf("the create answered %d", cw.status)))
			return
		}
		s.driveFillAnswer(w, r, &d, req, driveFillRefuse(cw.status, reason, msg))
		return
	}
	var run createRunResponse
	if err := json.Unmarshal(cw.body, &run); err != nil {
		s.driveFillAnswer(w, r, &d, req, driveFillFault("read the fill run", err))
		return
	}
	grants, err := s.cfg.Store.ListGrantsByRun(ctx, run.ID)
	if err != nil || !driveFillGrantsOK(grants, req.Repo) {
		s.driveFillEndRun(w, r, &d, req, run)
		return
	}
	s.driveFillAccepted(w, r, &d, req, run, plan)
}

// driveFillGrantsOK reads the run's grants back: exactly one git, GitHub-App or ssh credential, and it is the own
// token git_pat the fill composed. Other kinds, such as a required integration's api_key, are the workspace's and
// are not judged here.
func driveFillGrantsOK(grants []types.CredentialGrant, repo string) bool {
	var git []types.GrantSpec
	for _, g := range grants {
		switch g.Spec.Kind {
		case types.GrantGitPAT, types.GrantGitHubToken, types.GrantSSHKey:
			git = append(git, g.Spec)
		}
	}
	if len(git) != 1 || git[0].Kind != types.GrantGitPAT || !git[0].OwnerOnly {
		return false
	}
	sc, err := types.DecodeGitPATScope(git[0].Scope)
	if err != nil || !types.IsGitHubOwnTokenSecret(sc.SecretName) || sc.Access != types.PATAccessRead {
		return false
	}
	return sc.Repos != nil && len(*sc.Repos) == 1 && (*sc.Repos)[0] == driveFillRepoPath(repo)
}

// driveFillEndRun ends a fill's run that did not get exactly the own-token grant, as the kill route does, and answers
// the person. A kill that did not apply, or whose teardown failed, is a fault the person sees with the run id.
func (s *Server) driveFillEndRun(w http.ResponseWriter, r *http.Request, d *types.UserDrive, req client.MemberDriveSeedRequest, run createRunResponse) {
	ctx, id := r.Context(), run.ID
	cascadeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), killCascadeTimeout)
	defer cancel()
	cascadeCtx = withKillConfirm(cascadeCtx)
	killerType, killer := actorFromRequest(r)
	applied, killData, err := s.killLiveRun(cascadeCtx, run.AgentRun, killerType, killer, false)
	switch {
	case err != nil || !applied:
		if err == nil {
			err = errors.New("the run changed state before the fill could end it")
		}
		s.driveFillAnswer(w, r, d, req, &driveSeedRefusal{err: err, op: "end the fill run", reason: "internal", runID: &id})
	case len(killData) > 0:
		s.recordAudit(cascadeCtx, s.auditEvent(&id, types.ActorSystem, "wardynd", "run.revoke",
			id.String(), "failure", mustJSON(lifecycle.WithFailedSteps(killData))))
		s.auditDriveSeed(r, d, req, "refused", "internal", &id)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"id":     id,
			"state":  types.RunKilled,
			"errors": slices.Sorted(maps.Keys(killData)), // step names only; the text is in the run.revoke row
			"reason": reasonInternalError,
			"error":  "run marked KILLED but one or more teardown/revocation steps failed; the run may not be fully contained — retry the kill",
		})
	default:
		s.driveFillAnswer(w, r, d, req, &driveSeedRefusal{status: http.StatusUnprocessableEntity,
			reason: reasonDriveSeedOwnCredentialRequired, msg: driveFillEndedMsg(id), runID: &id})
	}
}

// driveFillAccepted audits and answers a fill whose run was created. Accepted means the run was created and is
// starting; the run's own end says whether the drive was filled.
func (s *Server) driveFillAccepted(w http.ResponseWriter, r *http.Request, d *types.UserDrive, req client.MemberDriveSeedRequest, run createRunResponse, plan driveFillPlan) {
	s.auditDriveSeed(r, d, req, "accepted", "", &run.ID)
	writeJSON(w, http.StatusAccepted, client.MemberDriveSeedResult{RunID: run.ID.String(), Target: plan.target})
}

// driveFillAnswer audits a refusal and answers it.
func (s *Server) driveFillAnswer(w http.ResponseWriter, r *http.Request, d *types.UserDrive, req client.MemberDriveSeedRequest, f *driveSeedRefusal) {
	s.auditDriveSeed(r, d, req, "refused", f.reason, f.runID)
	if f.err != nil {
		writeServerError(w, r, f.op, f.err)
		return
	}
	writeErrorReason(w, f.status, f.reason, f.msg)
}

// driveFillBodyRefusal checks the body's own shape, before the drive is read.
func driveFillBodyRefusal(req *client.MemberDriveSeedRequest) (int, string, string) {
	ws, err := uuid.Parse(req.WorkspaceID)
	if err != nil || req.Repo == "" || repoSelectionsRefusal(0, &[]client.RepoSelection{{Repo: req.Repo, Ref: req.Ref}}) != nil {
		return http.StatusBadRequest, reasonDriveSeedInvalid, driveFillInvalidMsg()
	}
	req.WorkspaceID = ws.String()
	if !driveFillIntoValid(req.Into) {
		return http.StatusBadRequest, reasonDriveSeedIntoInvalid, driveFillIntoInvalidMsg()
	}
	return 0, "", ""
}

// driveFillIntoValid: into is empty, or one relative path of clean elements, at most 255 bytes.
func driveFillIntoValid(into string) bool {
	if into == "" {
		return true
	}
	if len(into) > 255 || !utf8.ValidString(into) || strings.HasPrefix(into, "/") {
		return false
	}
	for _, r := range into {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	for _, el := range strings.Split(into, "/") {
		if el == "" || el == "." || el == ".." {
			return false
		}
	}
	return true
}

// driveFillPrecheck is the fill's refusals before any run: the drive's kind, the repository, a run of the owner
// starting, a sandbox that mounts the drive, the credential the create would keep and the owner's own token.
func (s *Server) driveFillPrecheck(ctx context.Context, r *http.Request, ms store.MemberDriveStore, owner string, d types.UserDrive, req client.MemberDriveSeedRequest) (driveFillPlan, *driveSeedRefusal) {
	var plan driveFillPlan
	switch {
	case d.Deleting():
		return plan, driveFillRefuse(http.StatusUnprocessableEntity, reasonDriveSeedUnsupported, driveFillDeletingMsg(d.Name))
	case d.LegacyUnsettled || d.Team || (d.Backend != types.DriveBackendDockerVolume && d.Backend != types.DriveBackendK8sPVC):
		return plan, driveFillRefuse(http.StatusUnprocessableEntity, reasonDriveSeedUnsupported, driveFillKindMsg(d.Name))
	}
	if !directGitHubRepo(req.Repo) {
		return plan, driveFillRefuse(http.StatusUnprocessableEntity, reasonDriveSeedUnsupported, driveFillRepoMsg())
	}
	if plan.target = runner.DriveTargetFor(types.ResolvedDrive{Drive: d}, false); plan.target == "" {
		return plan, driveFillRefuse(http.StatusUnprocessableEntity, reasonDriveSeedUnsupported, driveFillKindMsg(d.Name))
	}
	starting, err := ms.MemberDriveOwnerStarting(ctx, owner, watcherLeaseStaleAfter)
	if err != nil {
		return plan, driveFillFault("read the owner's starting runs", err)
	}
	if starting {
		return plan, driveFillRefuse(http.StatusConflict, reasonDriveSeedInUse, driveFillOwnerStartingMsg(d.Name))
	}
	if refusal := s.driveFillHolderRefusal(ctx, d); refusal != nil {
		return plan, refusal
	}
	grant, refusal := s.driveFillCredential(ctx, r, req)
	if refusal != nil {
		return plan, refusal
	}
	plan.grant = grant
	return plan, nil
}

// driveFillHolderRefusal refuses a drive a sandbox mounts, and one the substrate cannot judge.
func (s *Server) driveFillHolderRefusal(ctx context.Context, d types.UserDrive) *driveSeedRefusal {
	unavailable := driveFillRefuse(http.StatusServiceUnavailable, reasonDriveSeedUnavailable, driveFillUnavailableMsg())
	mount, err := memberDriveMount(d)
	if err != nil {
		return driveFillFault("build the drive's mount", err)
	}
	h, ok := s.cfg.Runner.(runner.DriveHolderChecker)
	if !ok {
		return unavailable
	}
	hctx, cancel := context.WithTimeout(ctx, driveReclaimTimeout)
	defer cancel()
	holder, err := h.DriveHolder(hctx, mount)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: could not check whether a run holds a drive to fill",
			slog.String("drive_id", d.ID.String()), slog.Any("err", err))
		return unavailable
	}
	if holder != "" {
		return driveFillRefuse(http.StatusConflict, reasonDriveSeedInUse, driveFillInUseMsg(d.Name))
	}
	return nil
}

// driveFillRepoPath is the path of the URL the run clones for repo, without its leading slash: the entry a git_pat
// scope's repos list must hold for the proxy to let that clone through. A bare org/name clones as org/name.git.
func driveFillRepoPath(repo string) string {
	u, err := url.Parse(repoCloneURL(repo))
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Path, "/")
}

// driveFillGrant is the one grant a fill authors: the ceiling's github.com pairing, narrowed to the repository the
// run clones, read access, and the owner's own row only.
func driveFillGrant(pair types.GitPATScope, repo string) types.GrantSpec {
	scope, err := json.Marshal(types.GitPATScope{
		Host: pair.Host, SecretName: pair.SecretName,
		Repos: types.PATRepoSet(driveFillRepoPath(repo)), Access: types.PATAccessRead,
	})
	if err != nil {
		scope = nil
	}
	return types.GrantSpec{Kind: types.GrantGitPAT, OwnerOnly: true, Scope: scope}
}

// driveFillPairings lists the ceiling's git_pat pairings for github.com, in ceiling order.
func driveFillPairings(grants []types.GrantSpec) []types.GitPATScope {
	var out []types.GitPATScope
	for _, g := range grants {
		if g.Kind != types.GrantGitPAT {
			continue
		}
		if sc, err := types.DecodeGitPATScope(g.Scope); err == nil && hostEqual(sc.Host, driveFillHost) {
			out = append(out, sc)
		}
	}
	return out
}

// driveFillCredential settles the grant: the first github.com pairing of the caller's ceiling that the create would
// keep, then the owner's own token that would replace the shared secret in it.
func (s *Server) driveFillCredential(ctx context.Context, r *http.Request, req client.MemberDriveSeedRequest) (types.GrantSpec, *driveSeedRefusal) {
	subject := runIdentitySubject(ctx, principalFromRequest(r))
	ceil, err := s.effectiveCeiling(ctx)
	if err != nil {
		return types.GrantSpec{}, driveFillFromRun(ceilingRunRefusal(err), "resolve governance ceiling")
	}
	pairs := driveFillPairings(ceil.Spec.EligibleGrants)
	if len(pairs) == 0 {
		return types.GrantSpec{}, driveFillRefuse(http.StatusUnprocessableEntity, reasonDriveSeedOwnCredentialRequired, driveFillNoPairingMsg())
	}
	var first *driveSeedRefusal
	var chosen *types.GrantSpec
	for _, pair := range pairs {
		g := driveFillGrant(pair, req.Repo)
		refusal := s.driveFillKeeps(ctx, r, g, pair, ceil.Spec.EligibleGrants, req.Repo)
		if refusal == nil {
			chosen = &g
			break
		}
		if refusal.err != nil || refusal.status == http.StatusServiceUnavailable {
			return types.GrantSpec{}, refusal // could not judge: never read as "refused for this pairing"
		}
		if first == nil {
			first = refusal
		}
	}
	if chosen == nil {
		return types.GrantSpec{}, first
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return types.GrantSpec{}, driveFillFault("get site config", err)
	}
	row, decided, derr := s.gitHubOwnTokenDecider(ctx, sc, driveFillHost, []string{req.Repo})
	var own *gitHubOwnTokenRefusal
	if errors.As(derr, &own) {
		return types.GrantSpec{}, driveFillRefuse(http.StatusUnprocessableEntity, own.reason, own.body)
	}
	if derr != nil {
		return types.GrantSpec{}, driveFillFault("decide the own GitHub token", derr)
	}
	if !decided {
		return types.GrantSpec{}, driveFillRefuse(http.StatusUnprocessableEntity, reasonDriveSeedOwnCredentialRequired, driveFillNoOwnTokenMsg())
	}
	if _, uerr := s.usableGitHubOwnToken(ctx, subject, row); uerr != nil {
		if errors.As(uerr, &own) {
			return types.GrantSpec{}, driveFillRefuse(http.StatusUnprocessableEntity, own.reason, own.body)
		}
		return types.GrantSpec{}, driveFillFault("read the own GitHub token", uerr)
	}
	return *chosen, nil
}

// driveFillKeeps asks whether the create would keep g as a member's grant: the ceiling lists its pairing, the
// ceiling's scopes leave the repository, the caller holds the secret capability on the shared name (or owns a
// secret of that name) and the egress capability on github.com. It asks narrowUserInlinePolicy's questions; it does
// not decide them again.
func (s *Server) driveFillKeeps(ctx context.Context, r *http.Request, g types.GrantSpec, pair types.GitPATScope, ceiling []types.GrantSpec, repo string) *driveSeedRefusal {
	refused := driveFillRefuse(http.StatusUnprocessableEntity, reasonDriveSeedOwnCredentialRequired, driveFillWouldDropMsg(repo))
	if !storedSecretPairingInCeiling(g, ceiling) {
		return refused
	}
	var bounds []json.RawMessage
	for _, cg := range composer.CeilingGrantsCovering(g, ceiling) {
		bounds = append(bounds, cg.Scope)
	}
	var warns []string
	if _, keep := composer.PATScopeMeet(g.Scope, bounds, &warns); !keep {
		return refused
	}
	if s.runUngoverned(ctx) {
		return nil
	}
	cap := s.capBatchFor(ctx)
	ok, err := cap.allowed(ctx, capSecret, pair.SecretName)
	if err != nil {
		return driveFillFromRun(capabilityRunRefusal(err), "resolve capability")
	}
	if !ok {
		owned, oerr := s.ownsSecretChecked(ctx, s.secretOwnerFromRequest(r), pair.SecretName)
		if oerr != nil {
			return driveFillFromRun(capabilityRunRefusal(oerr), "resolve capability")
		}
		if !owned {
			return refused
		}
	}
	egress, err := cap.allowed(ctx, capEgressHost, driveFillHost)
	if err != nil {
		return driveFillFromRun(capabilityRunRefusal(err), "resolve capability")
	}
	if !egress {
		return refused
	}
	return nil
}

// driveFillCreateRequest is the POST /api/v1/runs the fill makes on the caller's own identity: an exec run that
// clones the repository outside the drive, mounts the drive writable and carries the one grant.
func driveFillCreateRequest(r *http.Request, d types.UserDrive, id uuid.UUID, req client.MemberDriveSeedRequest, plan driveFillPlan) (*http.Request, error) {
	writable := false
	repos := []client.RepoSelection{{Repo: req.Repo, Ref: req.Ref}}
	body, err := json.Marshal(client.CreateRunRequest{
		TaskMode: "exec",
		Title:    "Fill " + d.Name,
		Task:     driveFillTask(plan.target, req.Into),
		Workspaces: []client.WorkspaceSelection{{
			WorkspaceID: req.WorkspaceID, Target: driveFillSource, Repos: &repos,
		}},
		Drives: []client.DriveAttachment{{DriveID: id.String(), ReadOnly: &writable}},
		InlinePolicy: &types.RunPolicySpec{
			AllowedDomains: []string{driveFillHost},
			EligibleGrants: []types.GrantSpec{plan.grant},
		},
	})
	if err != nil {
		return nil, err
	}
	cr := r.Clone(withDriveFillLaunch(r.Context(), d.Name))
	cr.Method, cr.URL, cr.RequestURI = http.MethodPost, &url.URL{Path: "/api/v1/runs"}, ""
	cr.Body, cr.ContentLength, cr.GetBody = io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
	cr.Header.Set("Content-Type", "application/json")
	cr.Header.Del(idempotencyKeyHeader)
	return cr, nil
}

// driveFillNoCapacity is the 409 a fill's create gets where it would wait in a pool queue: a fill never waits.
func driveFillNoCapacity(place runPlacement, name string) *runRefusal {
	if place.row.Limits != nil && place.row.Limits.MaxConcurrentRuns != nil {
		return runError(http.StatusConflict, reasonDriveSeedNoCapacity, driveFillCappedPoolMsg(place.pool.Name, name))
	}
	return runError(http.StatusConflict, reasonDriveSeedNoCapacity, driveFillRunnersBusyMsg(name))
}

// shellQuote single-quotes s for a POSIX shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// driveFillTask is the exec run's script: copy the clone into the drive's target, or its sub-folder into, and
// refuse a destination that already holds files.
func driveFillTask(target, into string) string {
	return driveFillScript(driveFillSource, target, into, driveFillMounts)
}

// driveFillScript builds the script over its source, target and mount table, so a test can run it on temp paths.
func driveFillScript(src, target, into, mounts string) string {
	dest := target
	if into != "" {
		dest = target + "/" + into
	}
	return strings.Join([]string{
		"set -eu",
		"src=" + shellQuote(src),
		"target=" + shellQuote(target),
		"dest=" + shellQuote(dest),
		"mounts=" + shellQuote(mounts),
		`[ -d "$src/.git" ] || { echo "wardyn: the repository was not cloned; see the clone line above" >&2; exit 2; }`,
		`awk -v t="$target" '$2 == t { f = 1 } END { exit !f }' "$mounts" || { echo "wardyn: $target is not the drive's mount; nothing was copied" >&2; exit 4; }`,
		`mkdir -p -- "$dest"`,
		`mkdir -- "$dest/.wardyn-fill" 2>/dev/null || { echo "wardyn: another fill of $dest is running" >&2; exit 5; }`,
		`trap 'rmdir -- "$dest/.wardyn-fill"' EXIT`,
		`skip='\.wardyn-fill'`,
		`[ "$dest" = "$target" ] && skip='\.wardyn-fill|lost\+found'`,
		`if ls -A -- "$dest" | grep -qvxE "$skip"; then echo "wardyn: $dest already holds files; fill an empty folder (into)" >&2; exit 3; fi`,
		`cp -a -- "$src/." "$dest/"`,
		`echo "wardyn: filled $dest at $(git -C "$dest" rev-parse HEAD)"`,
		"",
	}, "\n")
}

// auditDriveSeed writes drive.seed for one request: accepted with the run, or refused with the reason. Every value
// in it has passed the request's own checks. The resolved commit is recorded only for a full commit ref.
func (s *Server) auditDriveSeed(r *http.Request, d *types.UserDrive, req client.MemberDriveSeedRequest, outcome, reason string, runID *uuid.UUID) {
	result := "failure"
	if outcome == "accepted" {
		result = "success"
	}
	data := map[string]any{
		"drive": d.Name, "drive_id": d.ID, "backend": d.Backend, "workspace_id": req.WorkspaceID,
		"repo": req.Repo, "ref": req.Ref, "into": req.Into, "target": runner.DriveTargetFor(types.ResolvedDrive{Drive: *d}, false),
		"outcome": outcome, "reason": reason,
	}
	if runID != nil {
		data["run_id"] = runID
	}
	if driveFillFullCommit(req.Ref) {
		data["commit"] = req.Ref
	}
	actorType, actor := actorTypeFromRequest(r), principalFromRequest(r)
	s.recordAudit(r.Context(), s.auditEvent(runID, actorType, actor, "drive.seed", d.ID.String(), result, mustJSON(data)))
}

// driveFillFullCommit: ref is a full 40-character lowercase hex commit.
func driveFillFullCommit(ref string) bool {
	if len(ref) != 40 {
		return false
	}
	for _, c := range ref {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func driveFillInvalidMsg() string {
	return "Name a workspace and one of its repositories, and, if you give a ref, a branch, tag or commit name."
}

func driveFillIntoInvalidMsg() string {
	return "Name one folder of the drive to fill: a relative path with no .. and no control characters, at most 255 bytes."
}

func driveFillDeletingMsg(name string) string { return fmt.Sprintf("%s is being deleted.", name) }

func driveFillKindMsg(name string) string {
	return fmt.Sprintf("%s can't be filled from a repository here. Only your own Docker volume or cluster volume drives can.", name)
}

func driveFillRepoMsg() string { return "Only a GitHub repository over HTTPS can fill a drive." }

func driveFillOwnerStartingMsg(name string) string {
	return fmt.Sprintf("A run of yours is starting. Fill %s once it has started.", name)
}

func driveFillInUseMsg(name string) string {
	return fmt.Sprintf("A run is using %s. Fill it when no run has it.", name)
}

func driveFillUnavailableMsg() string {
	return "Wardyn could not check whether a run is using this drive. Try again."
}

func driveFillNoPairingMsg() string {
	return "Filling a drive uses your own GitHub token, and this organisation's policy pairs no GitHub token secret for github.com. Ask an admin."
}

func driveFillNoOwnTokenMsg() string {
	return "Filling a drive uses your own GitHub token for github.com, and this organisation has none set up for it. Ask an admin."
}

func driveFillWouldDropMsg(repo string) string {
	return fmt.Sprintf("Your organisation's policy would not give this run the GitHub token for %s. Ask an admin.", repo)
}

func driveFillEndedMsg(id uuid.UUID) string {
	return fmt.Sprintf("Wardyn could not give this run only your own GitHub token, so it ended run %s. Ask an admin.", id)
}

func driveFillNoBaseImageMsg() string {
	return "This workspace has no base image, so there is nothing to run the fill in. Ask an admin to set one."
}

func driveFillCappedPoolMsg(pool, name string) string {
	return fmt.Sprintf("%s starts every run from its queue, and a fill never waits in one, so %s can't be filled there. Ask an admin.", pool, name)
}

func driveFillRunnersBusyMsg(name string) string {
	return fmt.Sprintf("Every runner that could fill %s is busy, and a fill never waits for one. Try again when a runner is free.", name)
}
