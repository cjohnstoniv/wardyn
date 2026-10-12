// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"net/http"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// A run's attachments (OD-3, A-S4): every workspace it attaches, each repository
// it clones and the drive, at their resolved in-sandbox paths. One list feeds the
// overlap check, the Azure DevOps organisation check, the starting folder and the
// mount map the dry doors return, so Review shows the paths the run uses.

const (
	attachmentRepository = "repository"
	attachmentFolder     = "folder"
	attachmentDrive      = "drive"
	// startFolderImageDefault is WARDYN_START_FOLDER for an explicit image
	// default: the sandbox keeps the image's own working directory.
	startFolderImageDefault = "image_default"
)

// runTarget is one resolved in-sandbox path of a run: a folder mount, a
// repository clone, the drive, or a scratch directory (kind "", not an
// attachment, but a path the overlap check still covers).
type runTarget struct {
	kind     string
	owner    string // the owning workspace's id, or "drive"
	name     string // the owner's NAME, as a refusal and Review show it
	target   string
	repo     string
	ref      string
	readOnly bool
	// seeded: a workspaces[] selection seeded this entry, so its owner is that
	// workspace; otherwise the policy carries it and its owner is the gate's.
	seeded bool
}

// runTargets lists every resolved path of a run, the drive first, then each
// workspace's mounts, clones and scratch directories in the spec's order (the
// seeded workspaces lead it, the primary first). drive is nil when the run has
// none.
//
// ponytail: the legacy drive is the only drive; D-112-113's plural drive list
// joins here, each drive at its own server-derived target.
func runTargets(spec types.RunPolicySpec, carriers workspaceSourceIndex, seed workspaceSeed, drive *runTarget) []runTarget {
	var out []runTarget
	if drive != nil {
		out = append(out, *drive)
	}
	// A source the policy carries is owned by the workspace the onboarding gate
	// held it to (authorizeSpecWorkspaceSources reads the same index). Every
	// mount and repo of a run passed that gate, so one always does; an unowned
	// one is still its own owner, never everyone's.
	owner := func(local bool, source, fallback string) (string, string) {
		ws, ok := carriers.repo[source]
		if local {
			ws, ok = carriers.localDir[source]
		}
		if ok {
			return ws.ID.String(), ws.Name
		}
		return "source:" + source, fallback
	}
	// The entries the seed prepended belong to the workspace that seeded them.
	seededBy := func(owners []types.Workspace, i int) (string, string, bool) {
		if i < len(owners) {
			return owners[i].ID.String(), owners[i].Name, true
		}
		return "", "", false
	}
	for i, m := range spec.WorkspaceMounts {
		id, name, ok := seededBy(seed.mountOwners, i)
		if !ok {
			id, name = owner(true, m.Source, m.Target)
		}
		out = append(out, runTarget{kind: attachmentFolder, owner: id, name: name, target: m.Target, readOnly: m.ReadOnlyOrDefault(), seeded: ok})
	}
	for i, r := range spec.WorkspaceRepos {
		dest, ok := repoCloneTarget(r)
		if !ok {
			continue
		}
		id, name, seeded := seededBy(seed.repoOwners, i)
		if !seeded {
			id, name = owner(false, r.Repo, r.Repo)
		}
		out = append(out, runTarget{kind: attachmentRepository, owner: id, name: name, target: dest, repo: r.Repo, ref: r.Ref, seeded: seeded})
	}
	for _, ws := range seed.attached {
		for _, src := range workspaceSourcesOfType(ws, types.WorkspaceSourceTypeEphemeral) {
			if src.Target != "" {
				out = append(out, runTarget{owner: ws.ID.String(), name: ws.Name, target: src.Target})
			}
		}
	}
	return out
}

// placedMount is one target placed in the overlap tree; seq is its position.
type placedMount struct {
	owner, name, target string
	seq                 int
}

// firstMounts keeps the earliest mount added and the earliest one whose owner
// differs from it, which is enough to answer "earliest mount not owned by x".
type firstMounts struct{ first, other *placedMount }

func (f *firstMounts) add(m *placedMount) {
	switch {
	case f.first == nil:
		f.first = m
	case f.other == nil && m.owner != f.first.owner:
		f.other = m
	}
}

func (f *firstMounts) notOwnedBy(owner string) *placedMount {
	if f.first != nil && f.first.owner != owner {
		return f.first
	}
	return f.other
}

// mountNode is one path element of the targets placed so far: exact holds the
// mounts at this path, tree those at or below it.
type mountNode struct {
	children    map[string]*mountNode
	exact, tree firstMounts
}

// targetOverlapRefusal refuses equal or nested targets across different owners
// (OD-3, the drive included), and two repositories cloning into one directory
// even inside one workspace, since only one of them could be cloned there.
// Nesting inside one workspace's own sources stays allowed. The same repository
// at the same path and ref twice is one clone, not a collision.
//
// Each target is compared with the earlier ones through a tree of path elements,
// so the cost follows the size of the list rather than its square. The refusal
// is the one a pairwise scan in list order gives: the first target that overlaps
// an earlier one, naming the earliest such.
func targetOverlapRefusal(targets []runTarget) *runRefusal {
	root := &mountNode{}
	clones := map[string]runTarget{}
	for seq, t := range targets {
		if t.kind == attachmentRepository {
			if prev, ok := clones[t.target]; ok {
				if prev.repo == t.repo && prev.ref == t.ref {
					continue
				}
				return overlapRefusal(t.target, t.name, prev.name, prev.target)
			}
			clones[t.target] = t
		}
		elems := strings.Split(strings.TrimPrefix(t.target, "/"), "/")
		var earliest *placedMount
		consider := func(m *placedMount) {
			if m != nil && (earliest == nil || m.seq < earliest.seq) {
				earliest = m
			}
		}
		node := root
		for _, e := range elems {
			consider(node.exact.notOwnedBy(t.owner)) // an enclosing mount
			if node = node.children[e]; node == nil {
				break
			}
		}
		if node != nil {
			consider(node.tree.notOwnedBy(t.owner)) // an equal or enclosed mount
		}
		if earliest != nil {
			return overlapRefusal(t.target, t.name, earliest.name, earliest.target)
		}
		m := &placedMount{owner: t.owner, name: t.name, target: t.target, seq: seq}
		node = root
		for _, e := range elems {
			if node.children == nil {
				node.children = map[string]*mountNode{}
			}
			if node.children[e] == nil {
				node.children[e] = &mountNode{}
			}
			node.tree.add(m)
			node = node.children[e]
		}
		node.tree.add(m)
		node.exact.add(m)
	}
	return nil
}

// overlapRefusal words an overlap of target t (of self) with another mount,
// otherPath (of other). A nested pair names the inner path and the outer mount.
func overlapRefusal(t, self, other, otherPath string) *runRefusal {
	msg := workspaceOverlapEqualMsg(t, other)
	switch {
	case t == otherPath:
	case pathWithin(t, otherPath):
		msg = workspaceOverlapNestedMsg(t, other, otherPath)
	default:
		msg = workspaceOverlapNestedMsg(otherPath, self, t)
	}
	return runError(http.StatusUnprocessableEntity, reasonWorkspaceTargetOverlap, msg)
}

// adoOrgConflictRefusal holds a run to one Azure DevOps organisation across its
// workspaces (DECISIONS 2026-10-09): an Entra token carries no organisation
// claim, so a run spanning two would need two pins and gets neither.
func adoOrgConflictRefusal(targets []runTarget) *runRefusal {
	var first runTarget
	var firstOrg string
	for _, t := range targets {
		if t.kind != attachmentRepository || t.owner == "" {
			continue
		}
		org, ok := adoOrganisationOf(t.repo)
		switch {
		case !ok:
		case firstOrg == "":
			first, firstOrg = t, org
		case org != firstOrg && t.owner != first.owner:
			return runError(http.StatusUnprocessableEntity, reasonWorkspaceADOOrgConflict,
				workspaceADOOrgConflictMsg(first.name, firstOrg, t.name, org))
		}
	}
	return nil
}

// startFolderDir resolves the run's one starting folder over its targets: ""
// when the request names none (the sandbox keeps today's choice),
// startFolderImageDefault for the image's own directory, else the absolute
// folder inside one attachment, with that attachment's index. A workspace
// attachment with no repo is the workspace's first attachment. The subpath is
// already relative and cleaned (startFolderShapeRefusal), so the folder stays
// inside the mount; symlinks are resolved inside the sandbox.
//
// A repository the policy carries may sit in several of the run's workspaces
// while one owner is shown. A start folder naming another workspace that
// carries it is that clone too: it resolves, and the clone is shown as that
// workspace's, so the mount map and the starting folder agree.
func startFolderDir(f *client.StartFolder, targets []runTarget, wsRefs []types.Workspace) (string, int, *runRefusal) {
	switch {
	case f == nil:
		return "", -1, nil
	case f.Kind == client.StartFolderImageDefault:
		return startFolderImageDefault, -1, nil
	}
	owner := f.Attachment
	if owner == client.StartFolderDrive {
		owner = attachmentDrive
	}
	name := f.Repo
	at := func(i int) (string, int, *runRefusal) {
		return strings.TrimSuffix(targets[i].target+"/"+f.Subpath, "/"), i, nil
	}
	for i, t := range targets {
		if t.kind == "" || t.owner != owner {
			continue
		}
		if f.Repo == "" || t.kind == attachmentRepository && t.repo == f.Repo {
			return at(i)
		}
	}
	if i := slices.IndexFunc(wsRefs, func(ws types.Workspace) bool { return ws.ID.String() == f.Attachment }); i >= 0 && f.Repo != "" &&
		slices.ContainsFunc(workspaceSourcesOfType(wsRefs[i], types.WorkspaceSourceTypeRepo), func(src types.WorkspaceSource) bool { return src.Source == f.Repo }) {
		named := wsRefs[i]
		for j, t := range targets {
			if t.kind == attachmentRepository && !t.seeded && t.repo == f.Repo {
				targets[j].owner, targets[j].name = named.ID.String(), named.Name
				return at(j)
			}
		}
	}
	if name == "" {
		name = f.Attachment
		for _, t := range targets {
			if t.owner == owner {
				name = t.name
				break
			}
		}
	}
	return "", -1, runError(http.StatusBadRequest, reasonStartFolderInvalid, startFolderAttachmentMsg(name))
}

// runAttachments answers the run's cross-workspace checks in order: the base
// image (when the workspaces decide it), overlap, the Azure DevOps
// organisation, then the starting folder. It
// returns the mount map and the starting folder. A method on Server for the
// reason runContractRefusal is: the parity guard reads gates as s.<Gate>(…) calls.
func (s *Server) runAttachments(req createRunRequest, spec types.RunPolicySpec, wsRefs []types.Workspace, carriers workspaceSourceIndex, seed workspaceSeed, drive *runTarget) (
	[]client.RunAttachment, string, *runRefusal,
) {
	if seed.imageOpen {
		if _, _, refusal := workspacesBaseImage(wsRefs, nil); refusal != nil {
			return nil, "", refusal
		}
	}
	targets := runTargets(spec, carriers, seed, drive)
	if refusal := targetOverlapRefusal(targets); refusal != nil {
		return nil, "", refusal
	}
	if refusal := adoOrgConflictRefusal(targets); refusal != nil {
		return nil, "", refusal
	}
	dir, at, refusal := startFolderDir(req.StartFolder, targets, wsRefs)
	if refusal != nil {
		return nil, "", refusal
	}
	return runAttachmentFacts(targets, at), dir, nil
}

// runAttachmentFacts is the mount map the dry doors return: every attachment of
// targets, with the one holding the starting folder marked. Never nil.
func runAttachmentFacts(targets []runTarget, startsHere int) []client.RunAttachment {
	out := []client.RunAttachment{}
	for i, t := range targets {
		if t.kind == "" {
			continue
		}
		a := client.RunAttachment{Kind: t.kind, Name: t.name, Target: t.target, ReadOnly: t.readOnly, StartsHere: i == startsHere}
		switch t.kind {
		case attachmentRepository:
			a.WorkspaceID, a.Repo, a.Ref = t.owner, t.repo, t.ref
		case attachmentFolder:
			a.WorkspaceID = t.owner
		}
		out = append(out, a)
	}
	return out
}

// legacyDriveTarget is the run's drive as a target: the resolved mount at
// create and Review, the request's narrowing over the allocation at the preview.
func legacyDriveTarget(mount *types.DriveMount, previewReadOnly *bool) *runTarget {
	switch {
	case mount != nil:
		return &runTarget{kind: attachmentDrive, owner: attachmentDrive, name: runDriveName, target: cmp.Or(mount.Target, runner.DriveTarget), readOnly: mount.ReadOnly}
	case previewReadOnly != nil:
		return &runTarget{kind: attachmentDrive, owner: attachmentDrive, name: runDriveName, target: runner.DriveTarget, readOnly: *previewReadOnly}
	}
	return nil
}

// startFolderEnv carries the run's starting folder to the sandbox as the
// non-secret WARDYN_START_FOLDER, which agent-run's resolve_workdir reads on
// every start path. None when the request named no folder.
func startFolderEnv(dir string) map[string]string {
	if dir == "" {
		return nil
	}
	return map[string]string{"WARDYN_START_FOLDER": dir}
}
