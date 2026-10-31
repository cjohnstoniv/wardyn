// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A member's local_dir folder can be bound to one of their own runners (types.WorkspaceSource.RunnerID):
// the folder lives on that machine, never on this host. The control plane checks its path by text alone
// (runner.UserMountPolicy.ValidateUserMountLexical), places a run with it only on that runner, and never
// reads, writes or binds the path here. The runner checks the folder on its own disk at bind.
//
// Nothing unbinds a folder behind its member's back: revoking the runner, handing the workspace to someone
// else, or the leaver purge leave runner_id as it is, and every door that would use the folder then
// refuses it by name (workspace_source_runner_unavailable) until it is rebound or removed.

// boundRunnerRow reads the runner a folder is bound to; found is false when no runner has the id. A store
// that keeps no runners holds none.
func (s *Server) boundRunnerRow(ctx context.Context, id uuid.UUID) (rn types.Runner, found bool, err error) {
	st, ok := s.cfg.Store.(store.RunnerStore)
	if !ok {
		return types.Runner{}, false, nil
	}
	rn, err = st.GetRunner(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return types.Runner{}, false, nil
	}
	if err != nil {
		return types.Runner{}, false, fmt.Errorf("read runner %s: %w", id, err)
	}
	return rn, true, nil
}

// boundRunnerState is how a folder bound to runner id stands now for a workspace owned by owner, one of
// the types.WorkspaceSourceRunner* values, with the runner row it read.
func (s *Server) boundRunnerState(ctx context.Context, id uuid.UUID, owner string) (types.Runner, string, error) {
	rn, found, err := s.boundRunnerRow(ctx, id)
	if err != nil {
		return types.Runner{}, "", err
	}
	return rn, sourceRunnerState(rn, found, owner), nil
}

// sourceRunnerState is the binding rule: a folder is usable only on a claimed, unrevoked, person-owned
// runner of the workspace's current owner. A workspace an operator owns (owner "", as after a reassign)
// has no runner of its own, so every binding it carries is not_owner.
func sourceRunnerState(rn types.Runner, found bool, owner string) string {
	switch {
	case !found:
		return types.WorkspaceSourceRunnerMissing
	case rn.OwnerKind != types.RunnerOwnerPerson || owner == "" || !strings.EqualFold(rn.Owner, owner):
		return types.WorkspaceSourceRunnerNotOwner
	case rn.State != types.RunnerClaimed || rn.RevokedAt != nil:
		return types.WorkspaceSourceRunnerRevoked
	}
	return types.WorkspaceSourceRunnerClaimed
}

// boundRunnerLabel names a runner in a sentence: its name, or the first 8 characters of its id when it is
// gone or has none.
func boundRunnerLabel(id uuid.UUID, rn types.Runner) string {
	if rn.Name != "" {
		return rn.Name
	}
	return id.String()[:8]
}

// runnerUnavailableMsg is the stale-binding refusal (workspace_source_runner_unavailable).
func runnerUnavailableMsg(workspace string, id uuid.UUID, rn types.Runner, state string) string {
	why := map[string]string{
		types.WorkspaceSourceRunnerRevoked:  "runner revoked",
		types.WorkspaceSourceRunnerMissing:  "runner removed",
		types.WorkspaceSourceRunnerNotOwner: "not your runner",
	}[state]
	return fmt.Sprintf("The folder for %s is on %s, which can't run it now (%s). "+
		"Add a new workspace with a folder on a runner you own or on the Wardyn server.", workspace, boundRunnerLabel(id, rn), why)
}

// sourceScanRunnerBoundMsg is the library scan door's refusal of a folder bound to a runner.
func sourceScanRunnerBoundMsg(id uuid.UUID) string {
	return "This folder is on runner " + id.String() + "; Wardyn can't scan it from here."
}

// sessionRunnerBoundMsg is the record and verify doors' refusal of a folder bound to a runner whose
// binding still holds.
const sessionRunnerBoundMsg = "Recording and verify run on Wardyn's own executor; a folder on a runner can't be used there."

// The workspace door's refusals of a binding. Each ends with how to get out of it, since an edit of a
// workspace whose binding went stale re-checks every source.
const (
	sourcesOnePlaceMsg = "A workspace's folders must all be in one place: on the Wardyn server, or on one of your runners. " +
		"Put them in the same place, or make two workspaces."
	sourceBindingExit      = " Remove runner_id or detach the source."
	operatorRunnerIDMsg    = "runner_id binds a folder to its owner's own runner, and a workspace an operator owns has no owner to bind it for." + sourceBindingExit
	sourceRunnerNotYoursF  = "No runner with the id %s belongs to you." + sourceBindingExit
	sourceRunnerRevokedF   = "The runner %s was revoked and can't hold a folder." + sourceBindingExit
	sourceRunnerOnlyFolder = "runner_id is only for a local_dir source"
)

// sourceBindingRefusal is the workspace door's check of sources' runner bindings for a workspace owned
// by owner, "" when they hold. Every folder is in one place (all on this host, or all on one runner); an
// operator's workspace binds none; and the runner is the owner's own, person-owned, claimed and not
// revoked. Another person's runner and an id no runner has read alike, so the answer confirms neither.
func (s *Server) sourceBindingRefusal(ctx context.Context, owner string, sources []types.WorkspaceSource) (string, error) {
	places := map[uuid.UUID]bool{} // uuid.Nil is this host
	var bound *uuid.UUID
	for _, src := range sources {
		if src.Type != types.WorkspaceSourceTypeLocalDir {
			continue
		}
		place := uuid.Nil
		if src.RunnerID != nil {
			place, bound = *src.RunnerID, src.RunnerID
		}
		places[place] = true
	}
	switch {
	case bound == nil:
		return "", nil
	case owner == "":
		return operatorRunnerIDMsg, nil
	case len(places) > 1:
		return sourcesOnePlaceMsg, nil
	}
	rn, found, err := s.boundRunnerRow(ctx, *bound)
	if err != nil {
		return "", err
	}
	switch {
	case !found || rn.OwnerKind != types.RunnerOwnerPerson || !strings.EqualFold(rn.Owner, owner):
		return fmt.Sprintf(sourceRunnerNotYoursF, bound.String()), nil
	case rn.State == types.RunnerRevoked || rn.RevokedAt != nil:
		return fmt.Sprintf(sourceRunnerRevokedF, bound.String()), nil
	case rn.State != types.RunnerClaimed:
		return fmt.Sprintf(sourceRunnerNotYoursF, bound.String()), nil
	}
	return "", nil
}

// newBinding reports whether incoming binds a folder to a runner that stored does not already bind the
// same way: the same path, target, runner and writability.
func newBinding(stored, incoming []types.WorkspaceSource) bool {
	return slices.ContainsFunc(incoming, func(src types.WorkspaceSource) bool {
		return src.Type == types.WorkspaceSourceTypeLocalDir && src.RunnerID != nil &&
			!slices.ContainsFunc(stored, func(old types.WorkspaceSource) bool {
				return old.Type == types.WorkspaceSourceTypeLocalDir && old.RunnerID != nil && *old.RunnerID == *src.RunnerID &&
					old.Path == src.Path && old.Target == src.Target && old.Writable == src.Writable
			})
	})
}

// runnerStateStamper projects runner_state and runner_name onto every bound folder of a workspace read,
// from the runner row as it stands now; a runner whose row is gone has no name. Each runner is read once
// per request; a read that fails leaves both fields unset.
func (s *Server) runnerStateStamper(ctx context.Context) func(types.Workspace) types.Workspace {
	type row struct {
		rn    types.Runner
		found bool
		err   error
	}
	read := map[uuid.UUID]row{}
	return func(ws types.Workspace) types.Workspace {
		if !slices.ContainsFunc(ws.Sources, func(src types.WorkspaceSource) bool { return src.RunnerID != nil }) {
			return ws
		}
		// Clone before mutating: the store's row shares its slices with everything else that read it.
		ws.Sources = slices.Clone(ws.Sources)
		for i, src := range ws.Sources {
			if src.RunnerID == nil {
				continue
			}
			r, ok := read[*src.RunnerID]
			if !ok {
				r.rn, r.found, r.err = s.boundRunnerRow(ctx, *src.RunnerID)
				read[*src.RunnerID] = r
			}
			if r.err == nil {
				ws.Sources[i].RunnerState = sourceRunnerState(r.rn, r.found, ws.OwnedBy)
				ws.Sources[i].RunnerName = r.rn.Name
			}
		}
		return ws
	}
}

// writeSessionRunnerBoundRefusal refuses a record or verify session over a workspace with a folder bound
// to a runner, before anything launches: a session runs on Wardyn's own executor, which never binds a
// folder on a runner. A binding that no longer holds says so first.
func (s *Server) writeSessionRunnerBoundRefusal(w http.ResponseWriter, r *http.Request, ws types.Workspace) bool {
	for _, src := range ws.Sources {
		if src.Type != types.WorkspaceSourceTypeLocalDir || src.RunnerID == nil {
			continue
		}
		rn, state, err := s.boundRunnerState(r.Context(), *src.RunnerID, ws.OwnedBy)
		if err != nil {
			writeServerError(w, r, "read the folder's runner", err)
			return true
		}
		if state != types.WorkspaceSourceRunnerClaimed {
			writeErrorReason(w, http.StatusUnprocessableEntity, reasonWorkspaceSourceRunnerUnavailable, runnerUnavailableMsg(ws.Name, *src.RunnerID, rn, state))
			return true
		}
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonWorkspaceSessionRunnerBound, sessionRunnerBoundMsg)
		return true
	}
	return false
}

// sessionWorkspaceOr404 is the workspace a record or verify session runs over: false once it has written
// getWorkspaceOr404's answer or writeSessionRunnerBoundRefusal's.
func (s *Server) sessionWorkspaceOr404(w http.ResponseWriter, r *http.Request, id uuid.UUID) (types.Workspace, bool) {
	ws, ok := s.getWorkspaceOr404(w, r, id)
	return ws, ok && !s.writeSessionRunnerBoundRefusal(w, r, ws)
}

// mountBinding is where a run's holders put path: the runner every one of them binds it to, or nil for
// this host. ok is false when they disagree (bound and unbound, or two runners), and the path then has no
// one place to run.
func mountBinding(holders []types.Workspace, path string) (rid *uuid.UUID, ok bool) {
	seen := false
	for _, ws := range holders {
		for _, src := range ws.Sources {
			if src.Type != types.WorkspaceSourceTypeLocalDir || src.Path != path {
				continue
			}
			if !seen {
				rid, seen = src.RunnerID, true
				continue
			}
			if (rid == nil) != (src.RunnerID == nil) || (rid != nil && *rid != *src.RunnerID) {
				return nil, false
			}
		}
	}
	return rid, true
}

// mountPlacesMsg refuses a folder the run's workspaces put in two places.
func mountPlacesMsg(path string) string {
	return fmt.Sprintf("the folder %q is in more than one place among this run's workspaces (on the Wardyn server and on a runner, "+
		"or on two runners); a run can use it in one place only", path)
}

// boundMountAllowed is the run gate on a folder bound to runner rid, held by ws: the binding must still
// hold (workspace_source_runner_unavailable), and its path clears the owner's member rules by text, since
// the folder is not on this host (workspace_source_mount_not_allowed).
func (s *Server) boundMountAllowed(ctx context.Context, ws types.Workspace, rid uuid.UUID, wm types.WorkspaceMount) (int, string, error) {
	rn, state, err := s.boundRunnerState(ctx, rid, ws.OwnedBy)
	if err != nil {
		return http.StatusServiceUnavailable, reasonWorkspaceSourceRunnerUnavailable, err
	}
	if state != types.WorkspaceSourceRunnerClaimed {
		return http.StatusUnprocessableEntity, reasonWorkspaceSourceRunnerUnavailable, errors.New(runnerUnavailableMsg(ws.Name, rid, rn, state))
	}
	if err := s.cfg.UserMounts.ValidateUserMountLexical(ws.OwnedBy, wm.Source, !wm.ReadOnlyOrDefault()); err != nil {
		return http.StatusUnprocessableEntity, reasonWorkspaceSourceMountNotAllowed, fmt.Errorf("member workspace mount: %w", err)
	}
	return 0, "", nil
}

// boundMountsRunner is the one runner a run's bound folders are on, nil when it has none. Folders bound
// to two runners have no one place to run.
func boundMountsRunner(mounts []runner.Mount) (*uuid.UUID, *runRefusal) {
	var out *uuid.UUID
	for _, m := range mounts {
		if m.BoundRunner == "" {
			continue
		}
		id, err := uuid.Parse(m.BoundRunner)
		if err != nil || (out != nil && *out != id) {
			return nil, boundFolderRefusal("this run's folders are on more than one runner; a run can use folders from one place only")
		}
		out = &id
	}
	return out, nil
}

// boundFolderRefusal is placement_local_path with msg.
func boundFolderRefusal(msg string) *runRefusal {
	return runError(placement.ReasonPlacementLocalPath.Status(), string(placement.ReasonPlacementLocalPath), msg)
}

// runnerBoundFolderCaps is what runner id last reported of folders bound to it: whether its runnerd binds
// them (runnerwire.Caps.BoundFolders) and the folders its owner shares (--drive-root). ok is false when it
// has reported nothing to this process.
func (s *Server) runnerBoundFolderCaps(id uuid.UUID) (binds bool, roots []string, ok bool) {
	s.runnerStreams.subsMu.Lock()
	ep, found := s.runnerStreams.subs[id]
	s.runnerStreams.subsMu.Unlock()
	if !found {
		return false, nil, false
	}
	caps, ok := ep.sub.Caps()
	if !ok {
		return false, nil, false
	}
	return caps.BoundFolders, slices.Clone(caps.AllowedRoots), true
}

// mountPathsOn is a local plan's VerifiedLocalPaths for its folders on candidate: a folder's path is
// verified only when it is bound to candidate, candidate's runnerd binds such folders, and the path lies
// inside the roots candidate advertises (its owner's --drive-root). The runner checks the bind again
// against the same roots.
func (s *Server) mountPathsOn(candidate uuid.UUID, mounts []runner.Mount) map[string]bool {
	bound := map[string]boundLocalPath{}
	for i, m := range mounts {
		if id, err := uuid.Parse(m.BoundRunner); err == nil {
			bound[indexedMountPath(i)] = boundLocalPath{Runner: id, Path: m.Source}
		}
	}
	if len(bound) == 0 {
		return nil
	}
	binds, roots, _ := s.runnerBoundFolderCaps(candidate)
	if !binds {
		return nil
	}
	return verifiedLocalPaths(candidate, roots, bound)
}

// indexedMountPath is the local plan's key for spec.Mounts[i].
func indexedMountPath(i int) string {
	return fmt.Sprintf("SandboxSpec.Mounts[%d]", i)
}

// mergePaths is a and b in one VerifiedLocalPaths, nil when both are empty.
func mergePaths(a, b map[string]bool) map[string]bool {
	if len(b) == 0 {
		return a
	}
	if a == nil {
		a = map[string]bool{}
	}
	for k, v := range b {
		a[k] = v
	}
	return a
}

// boundFolderElsewhereMsg refuses a run with a folder bound to runner rid anywhere but on rid.
func boundFolderElsewhereMsg(rid uuid.UUID) string {
	return "This run's folder is on your runner " + rid.String()[:8] +
		", so it runs only there: launch it in a runner pool that holds that runner, with placement local."
}

// boundFolderTargets is kindTargets for a run whose folders are bound to runner f.mountRunner: that runner
// alone of the person's own, never the remote-hosted kind, which never binds such a folder. A pin to
// another runner, a pool without that runner, a waiting run the local-eligibility step refused there, or
// drives that do not mount on that runner each leave the run no place (placement_local_path).
func boundFolderTargets(f placementInput, kinds map[types.Placement][]poolTarget) (map[types.Placement][]poolTarget, *runRefusal) {
	rid := *f.mountRunner
	if pin := f.req.RunnerID; pin != "" {
		if id, err := uuid.Parse(pin); err != nil || id != rid {
			return nil, boundFolderRefusal("This run's folder is on your runner " + rid.String()[:8] +
				", so it can't run on the runner it is pinned to: pin that runner, or remove the pin.")
		}
	}
	on := targetsOn(kinds[types.PlacementLocal], &rid)
	if len(on) == 0 || f.noLocal {
		return nil, boundFolderRefusal(boundFolderElsewhereMsg(rid))
	}
	if f.drives && (f.driveRunner == nil || *f.driveRunner != rid) {
		return nil, boundFolderRefusal("This run's folder is on your runner " + rid.String()[:8] +
			", and its drives don't mount there: a run uses its folders and drives in one place.")
	}
	return map[types.Placement][]poolTarget{types.PlacementLocal: on}, nil
}

// boundFolderPlacementRefusal holds a run whose folders are bound to runner rid to that runner, at every
// door: placed anywhere else, or on a runner whose runnerd does not bind such folders, or whose --drive-root
// does not cover each one, it is refused placement_local_path with what to change.
func (s *Server) boundFolderPlacementRefusal(rid *uuid.UUID, placed types.Placement, mounts []runner.Mount) *runRefusal {
	if rid == nil {
		return nil
	}
	if placed != types.PlacementLocal {
		return boundFolderRefusal(boundFolderElsewhereMsg(*rid))
	}
	binds, roots, reported := s.runnerBoundFolderCaps(*rid)
	switch {
	case !reported:
		return boundFolderRefusal("This run's folder is on your runner " + rid.String()[:8] + ", which has not connected; start it and try again.")
	case !binds:
		return boundFolderRefusal("This run's folder is on your runner " + rid.String()[:8] +
			", whose wardyn-runnerd is too old to bind a workspace folder; update the runner and try again.")
	case len(roots) == 0:
		return boundFolderRefusal("This run's folder is on your runner " + rid.String()[:8] +
			", which shares no folders: start it with a --drive-root that covers the folder.")
	}
	for _, m := range mounts {
		if m.BoundRunner != "" && !withinAnyRoot(m.Source, roots) {
			return boundFolderRefusal(fmt.Sprintf("The folder %q is outside every --drive-root its runner shares (%s); "+
				"restart the runner with a --drive-root that covers it.", m.Source, strings.Join(roots, ", ")))
		}
	}
	return nil
}

// refuseUserSources answers 400 workspace_sources_not_allowed for sources a
// workspace owned by owner may not hold: a runner binding that does not hold
// (sourceBindingRefusal), then the member gate (userSourcesAllowed).
func (s *Server) refuseUserSources(w http.ResponseWriter, r *http.Request, owner string, sources []types.WorkspaceSource) bool {
	msg, err := s.sourceBindingRefusal(r.Context(), owner, sources)
	if err != nil {
		writeServerError(w, r, "read the folder's runner", err)
		return true
	}
	if msg == "" {
		msg = s.userSourcesAllowed(r, owner, sources)
	}
	if msg == "" {
		return false
	}
	writeErrorReason(w, http.StatusBadRequest, reasonWorkspaceSourcesNotAllowed, msg)
	return true
}

// userSourcesAllowed gates a MEMBER-owned workspace's local_dir sources
// through the member-safe mount rules (internal/runner's UserMountPolicy):
// the operator/MDM root allowlist matched on the CANONICALIZED real path, the
// credential-dotfile deny-list, and — only for a source asking to be writable —
// the separate writable allowlist minus its deny carve-out. It returns "" (fine)
// or a 400-worthy message.
//
// owner=="" means OPERATOR-owned, and returns "" immediately: an operator's
// mounts keep exactly the reach they have today (runner.ValidateMountSource
// alone, already run by validateWorkspaceSource). This function only ever
// NARROWS, never widens — it is additive on top of that deny-list, matching
// SandboxSpec.UserMountRoots' nil-means-today's-behavior contract at the
// other end of the same path.
func (s *Server) userSourcesAllowed(r *http.Request, owner string, sources []types.WorkspaceSource) string {
	if owner == "" {
		return ""
	}
	for i, src := range sources {
		if src.Type != types.WorkspaceSourceTypeLocalDir {
			continue
		}
		// A folder bound to the owner's runner is not on this host: its text alone is checked here, and
		// the runner checks the folder itself at bind.
		check := s.cfg.UserMounts.ValidateUserMount
		if src.RunnerID != nil {
			check = s.cfg.UserMounts.ValidateUserMountLexical
		}
		if err := check(owner, src.Path, src.Writable); err != nil {
			return fmt.Sprintf("sources[%d]: %s", i, err.Error())
		}
	}
	return ""
}
