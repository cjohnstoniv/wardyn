// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// workspaceSourceIndex indexes onboarded workspaces by each of their local_dir
// (by Path) and repo (by Source slug/URL) entries — the lookup a workspace
// composed of Sources needs, in place of the old single kind+source store
// query (store.GetWorkspaceBySource, removed: a workspace is no longer one
// row per kind+source). Built by scanning every workspace's Sources once;
// callers resolving several sources at once (referencedWorkspaces) build one
// index rather than listing per source.
//
// Nothing makes a source unique across workspaces, so each map holds the one
// workspace a source is judged by (see indexWorkspacesFor), and repoAll every
// workspace carrying a repo, in list order.
type workspaceSourceIndex struct {
	localDir map[string]types.Workspace   // Path -> owning workspace
	repo     map[string]types.Workspace   // Source (slug/URL) -> owning workspace
	repoAll  map[string][]types.Workspace // Source (slug/URL) -> every carrier
}

// indexWorkspacesBySource builds a workspaceSourceIndex from a workspace list.
func indexWorkspacesBySource(all []types.Workspace) workspaceSourceIndex {
	return indexWorkspacesFor(all, nil)
}

// indexWorkspacesFor is indexWorkspacesBySource for a caller who may use only
// the workspaces usable admits: a source several workspaces carry is owned by
// the last carrier in the list usable admits, and by the last carrier only when
// it admits none. nil admits every workspace.
func indexWorkspacesFor(all []types.Workspace, usable func(types.Workspace) bool) workspaceSourceIndex {
	idx := workspaceSourceIndex{localDir: map[string]types.Workspace{}, repo: map[string]types.Workspace{}, repoAll: map[string][]types.Workspace{}}
	hold := func(m map[string]types.Workspace, key string, ws types.Workspace) {
		if cur, ok := m[key]; ok && usable != nil && usable(cur) && !usable(ws) {
			return
		}
		m[key] = ws
	}
	for _, ws := range all {
		for _, src := range ws.Sources {
			switch src.Type {
			case types.WorkspaceSourceTypeLocalDir:
				hold(idx.localDir, src.Path, ws)
			case types.WorkspaceSourceTypeRepo:
				hold(idx.repo, src.Source, ws)
				idx.repoAll[src.Source] = append(idx.repoAll[src.Source], ws)
			}
		}
	}
	return idx
}

// validateWorkspaceSources fails a run closed (422) when any USER-workspace mount
// source or repo on the spec is not a pre-ONBOARDED workspace. It is the
// un-bypassable onboarding gate: called at run-create over the RESOLVED spec
// (inline, stored, OR default policy alike), so no authoring surface — including a
// hand-crafted stored policy — can smuggle an arbitrary host path or repo into a
// sandbox.
//
// It mirrors validateInlineSecretRefs: consults the workspace LIST only (never a
// profile/secret value), returns (statusCode, error) on rejection or (0, nil) when
// every user source is onboarded. The runner.ValidateMount deny-list still runs
// underneath as defense-in-depth; this is a floor-RAISING allow-list on top.
//
// The membership sets are built from EVERY workspace's Sources (not from the
// single-source Kind/Source mirror, which is empty for a multi-source
// workspace) — a multi-source workspace's second/third local_dir or repo entry
// must clear this gate exactly like its first.
// #656 M1: this used to answer every refusal with reasonWorkspaceSourcesInvalid;
// each cause below now names its own — except the two not-onboarded arms, which
// share reasonWorkspaceSourceNotOnboarded with authorizeSpecWorkspaceSources'
// OWN not-onboarded arms on purpose (H1): the sentence is already byte-identical
// across all four for the cross-member existence-oracle reason
// authorizeSpecWorkspaceSources' doc comment explains, and a distinguishable
// reason would reopen exactly that.
func (s *Server) validateWorkspaceSources(ctx context.Context, spec types.RunPolicySpec) (int, string, error) {
	// Fast path: nothing to gate if the run declares no user workspaces.
	if len(spec.WorkspaceMounts) == 0 && len(spec.WorkspaceRepos) == 0 {
		return 0, "", nil
	}

	if s.cfg.Store == nil {
		return http.StatusUnprocessableEntity, reasonWorkspaceSourcesStoreUnavailable, fmt.Errorf(
			"workspace onboarding requires a store, but none is configured")
	}
	all, err := s.cfg.Store.ListWorkspaces(ctx)
	if err != nil {
		return http.StatusUnprocessableEntity, reasonWorkspaceSourcesListUnavailable, fmt.Errorf("list workspaces: %w", err)
	}
	idx := indexWorkspacesBySource(all)

	for _, wm := range spec.WorkspaceMounts {
		ws, ok := idx.localDir[wm.Source]
		if !ok {
			return http.StatusUnprocessableEntity, reasonWorkspaceSourceNotOnboarded, fmt.Errorf(
				"mount source %q is not an onboarded local directory (onboard it first via the workspaces API)", wm.Source)
		}
		// MEMBER-OWNED source (0048): re-run the member-safe mount gate on the
		// RESOLVED spec, not only at onboarding — the same reason the onboarding
		// gate itself lives here rather than in one handler. It closes the case
		// where the operator narrowed the roots (or the source was repointed at
		// a symlink) AFTER the workspace was created. An operator-owned source
		// (OwnedBy == "") is untouched.
		if err := userMountAllowed(s.cfg.UserMounts, ws.OwnedBy, wm); err != nil {
			return http.StatusUnprocessableEntity, reasonWorkspaceSourceMountNotAllowed, err
		}
	}
	for _, wr := range spec.WorkspaceRepos {
		if _, ok := idx.repo[wr.Repo]; !ok {
			return http.StatusUnprocessableEntity, reasonWorkspaceSourceNotOnboarded, fmt.Errorf(
				"repo %q is not an onboarded repository (onboard it first via the workspaces API)", wr.Repo)
		}
	}
	return 0, "", nil
}

// userMountAllowed re-checks ONE resolved mount against its owning member's
// mount policy. owner=="" (operator-owned) is always fine — the member gate is
// additive and never narrows an operator mount.
func userMountAllowed(policy runner.UserMountPolicy, owner string, wm types.WorkspaceMount) error {
	if owner == "" {
		return nil
	}
	if err := policy.ValidateUserMount(owner, wm.Source, !wm.ReadOnlyOrDefault()); err != nil {
		return fmt.Errorf("member workspace mount: %w", err)
	}
	return nil
}

// sourceRefusal is validateWorkspaceSources' (status, reason, error) answer as
// one error, for the requirements fold: its record-session caller holds no
// ResponseWriter and hands the refusal back to its handler as an error.
type sourceRefusal struct {
	status int
	reason string
	err    error
}

// Error carries the "workspace: " prefix the create door answers with.
func (e *sourceRefusal) Error() string { return "workspace: " + e.err.Error() }

// requirementWriteRefusal holds every mount a requirements fold made writable
// to the member mount gate. The door ran that gate while the mount was still
// read-only (validateWorkspaceSources), and a write:<path> row turns any mount
// at its path writable, whichever workspace the row came from. So the owner is
// read here from the workspaces that hold the directory, not from the one
// being folded.
//
// The holders that count are the run's own: attached is every workspace the
// request names (the record door's one workspace), and each member-owned one
// that holds the directory must be allowed to write it, while an operator-owned
// one passes as it always has. A workspace that merely stores the same
// directory is not part of the run and decides nothing about it. Only a
// directory no attached workspace holds (a mount a policy authored) is held to
// every stored holder, and there a directory no workspace holds, or a list
// that cannot be read, refuses, since no owner can be named.
//
// The workspace whose row made the mount writable is held as well, whether or
// not it is attached: a workspace pulled into the run because it holds the same
// directory (referencedWorkspacesIn's index keeps one holder per directory) can
// carry a required write row its member owner set, and the attached holders
// alone would not answer for that member.
// wasReadOnly is each mount's state before the fold, and suppliers the
// workspaces whose write rows granted it, both by index.
func (s *Server) requirementWriteRefusal(ctx context.Context, attached []types.Workspace, mounts []types.WorkspaceMount, wasReadOnly []bool, suppliers map[int][]types.Workspace) *sourceRefusal {
	var widened []types.WorkspaceMount
	var supplied [][]types.Workspace
	for i, wm := range mounts {
		if i < len(wasReadOnly) && wasReadOnly[i] && !wm.ReadOnlyOrDefault() {
			widened = append(widened, wm)
			supplied = append(supplied, suppliers[i])
		}
	}
	if len(widened) == 0 {
		return nil
	}
	refuse := func(reason string, err error) *sourceRefusal {
		return &sourceRefusal{status: http.StatusUnprocessableEntity, reason: reason, err: err}
	}
	var stored []types.Workspace // read once, and only for a mount no attached workspace holds
	for k, wm := range widened {
		for _, ws := range supplied[k] {
			if err := userMountAllowed(s.cfg.UserMounts, ws.OwnedBy, wm); err != nil {
				return refuse(reasonWorkspaceSourceMountNotAllowed, err)
			}
		}
		holders := localDirHolders(attached, wm.Source)
		if len(holders) == 0 {
			if stored == nil {
				if s.cfg.Store == nil {
					return refuse(reasonWorkspaceSourcesStoreUnavailable, errors.New("workspace onboarding requires a store, but none is configured"))
				}
				var err error
				if stored, err = s.cfg.Store.ListWorkspaces(ctx); err != nil {
					return refuse(reasonWorkspaceSourcesListUnavailable, fmt.Errorf("list workspaces: %w", err))
				}
			}
			holders = localDirHolders(stored, wm.Source)
		}
		if len(holders) == 0 {
			return refuse(reasonWorkspaceSourceNotOnboarded, fmt.Errorf(
				"mount source %q is not an onboarded local directory (onboard it first via the workspaces API)", wm.Source))
		}
		for _, ws := range holders {
			if err := userMountAllowed(s.cfg.UserMounts, ws.OwnedBy, wm); err != nil {
				return refuse(reasonWorkspaceSourceMountNotAllowed, err)
			}
		}
	}
	return nil
}

// localDirHolders is the workspaces of list that carry path as a local_dir source.
func localDirHolders(list []types.Workspace, path string) []types.Workspace {
	var out []types.Workspace
	for _, ws := range list {
		if slices.ContainsFunc(ws.Sources, func(src types.WorkspaceSource) bool {
			return src.Type == types.WorkspaceSourceTypeLocalDir && src.Path == path
		}) {
			out = append(out, ws)
		}
	}
	return out
}

// userMountPosture is ONE run's member-mount posture, resolved once at
// create-run and carried to the driver: the roots the run's member-authored
// binds must resolve inside, plus WHICH mount sources those are. The two are one
// value because they are only ever correct together — roots without sources
// gates operator/Wardyn-authored binds (the subscription creds, the Bedrock
// ~/.aws dir) that live under no member root, and sources without roots gates
// nothing.
type userMountPosture struct {
	// Roots is runner.SandboxSpec.UserMountRoots: nil for an operator run,
	// empty-but-non-nil for a member run on a deployment with no roots.
	Roots []string
	// Sources are the member-owned workspaces' local_dir paths — the exact
	// strings a WorkspaceMount.Source carries for them, so buildRunMounts can
	// stamp runner.Mount.MemberAuthored by lookup. Nil for an operator run.
	Sources map[string]bool
}

// userMountPosture resolves a run's member-mount posture, or the zero value
// when the run has NO member-owned workspace — which is every operator run, and
// which the driver reads as "do exactly what you do today"
// (runner.SandboxSpec.UserMountRoots).
//
// wsRefs is the already-resolved referencedWorkspaces list, so this costs no
// extra store read. The FIRST member-owned workspace decides the ROOTS: a run
// cannot mix two members' workspaces (each member only ever sees their own), so
// there is no second owner to reconcile with — and if there somehow were, the
// first owner's roots are the narrower answer, never a union. The SOURCES still
// cover EVERY member-owned workspace in the list: if an admin ever did compose
// two members' workspaces into one run, the second member's dir is gated against
// the first's roots — refused, which is the fail-closed answer — rather than
// binding unchecked because it belonged to nobody the roots came from.
func (s *Server) userMountPosture(wsRefs []types.Workspace) userMountPosture {
	var p userMountPosture
	for _, ws := range wsRefs {
		if ws.OwnedBy == "" {
			continue
		}
		if p.Sources == nil {
			p.Sources = map[string]bool{}
			p.Roots = s.cfg.UserMounts.RootsFor(ws.OwnedBy)
			if p.Roots == nil {
				// Non-nil, empty: "member run, no roots" must still reach the driver
				// as a member run so every member bind fails closed there, rather
				// than silently degrading to the operator path.
				p.Roots = []string{}
			}
		}
		for _, src := range ws.Sources {
			if src.Type == types.WorkspaceSourceTypeLocalDir {
				p.Sources[src.Path] = true
			}
		}
	}
	return p
}
