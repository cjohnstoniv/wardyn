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
	kind  string
	owner string // the workspace it is shown under (its id), or "drive"
	name  string // the owner's NAME, as a refusal and Review show it
	// carriers are the owner ids the target belongs to: the workspace that
	// seeded it, or every one of the run's workspaces that carries a source the
	// policy names. The owner is always one of them.
	carriers []string
	target   string
	repo     string
	ref      string
	readOnly bool
}

// sharesCarrier reports whether two targets belong to a common workspace.
func sharesCarrier(a, b []string) bool {
	return slices.ContainsFunc(a, func(id string) bool { return slices.Contains(b, id) })
}

// runTargets lists every resolved path of a run, the drive first, then each
// workspace's mounts, clones and scratch directories in the spec's order (the
// seeded workspaces lead it, the primary first). drive is nil when the run has
// none.
//
// ponytail: the legacy drive is the only drive; D-112-113's plural drive list
// joins here, each drive at its own server-derived target.
func runTargets(spec types.RunPolicySpec, wsRefs []types.Workspace, index workspaceSourceIndex, seed workspaceSeed, drive *runTarget) []runTarget {
	var out []runTarget
	if drive != nil {
		d := *drive
		d.carriers = []string{d.owner}
		out = append(out, d)
	}
	// A source the policy carries is shown under the workspace the onboarding
	// gate held it to (authorizeSpecWorkspaceSources reads the same index) and
	// belongs to every one of the run's workspaces that carries it. Every mount
	// and repo of a run passed that gate, so one always does; an unowned one is
	// still its own owner, never everyone's.
	policyOwned := func(local bool, source, fallback string) runTarget {
		gate, ok := index.repo[source]
		if local {
			gate, ok = index.localDir[source]
		}
		if !ok {
			return runTarget{owner: "source:" + source, name: fallback, carriers: []string{"source:" + source}}
		}
		t := runTarget{owner: gate.ID.String(), name: gate.Name, carriers: []string{gate.ID.String()}}
		for _, ws := range wsRefs {
			if ws.ID != gate.ID && slices.ContainsFunc(ws.Sources, func(src types.WorkspaceSource) bool {
				return local && src.Type == types.WorkspaceSourceTypeLocalDir && src.Path == source ||
					!local && src.Type == types.WorkspaceSourceTypeRepo && src.Source == source
			}) {
				t.carriers = append(t.carriers, ws.ID.String())
			}
		}
		return t
	}
	// The entries the seed prepended belong to the workspace that seeded them.
	owned := func(owners []types.Workspace, i int, local bool, source, fallback string) runTarget {
		if i < len(owners) {
			return runTarget{owner: owners[i].ID.String(), name: owners[i].Name, carriers: []string{owners[i].ID.String()}}
		}
		return policyOwned(local, source, fallback)
	}
	for i, m := range spec.WorkspaceMounts {
		t := owned(seed.mountOwners, i, true, m.Source, m.Target)
		t.kind, t.target, t.readOnly = attachmentFolder, m.Target, m.ReadOnlyOrDefault()
		out = append(out, t)
	}
	for i, r := range spec.WorkspaceRepos {
		dest, ok := repoCloneTarget(r)
		if !ok {
			continue
		}
		t := owned(seed.repoOwners, i, false, r.Repo, r.Repo)
		t.kind, t.target, t.repo, t.ref = attachmentRepository, dest, r.Repo, r.Ref
		out = append(out, t)
	}
	for _, ws := range seed.attached {
		for _, src := range workspaceSourcesOfType(ws, types.WorkspaceSourceTypeEphemeral) {
			if src.Target != "" {
				out = append(out, runTarget{owner: ws.ID.String(), name: ws.Name, carriers: []string{ws.ID.String()}, target: src.Target})
			}
		}
	}
	return out
}

// placedMount is one target placed in the overlap tree; seq is its position.
type placedMount struct {
	name, target string
	carriers     []string
	seq          int
}

// firstMounts answers "the earliest mount placed here that shares no carrier
// with x". A mount with one carrier is kept only as the first of its owner, and
// only up to limit owners: when the earliest mount disjoint from x was not kept,
// limit owners came before it, and since x has fewer carriers than limit, one
// of those earlier mounts is disjoint from x too. A mount with several carriers
// (a source two of the run's workspaces carry, which is rare) is always kept.
type firstMounts struct{ single, multi []*placedMount }

func (f *firstMounts) add(m *placedMount, limit int) {
	switch {
	case len(m.carriers) > 1:
		f.multi = append(f.multi, m)
	case len(f.single) < limit && !slices.ContainsFunc(f.single, func(p *placedMount) bool { return p.carriers[0] == m.carriers[0] }):
		f.single = append(f.single, m)
	}
}

func (f *firstMounts) disjointFrom(carriers []string) *placedMount {
	var earliest *placedMount
	for _, list := range [][]*placedMount{f.single, f.multi} {
		if i := slices.IndexFunc(list, func(m *placedMount) bool { return !sharesCarrier(m.carriers, carriers) }); i >= 0 &&
			(earliest == nil || list[i].seq < earliest.seq) {
			earliest = list[i]
		}
	}
	return earliest
}

// mountNode is one path element of the targets placed so far: exact holds the
// mounts at this path, tree those at or below it.
type mountNode struct {
	children    map[string]*mountNode
	exact, tree firstMounts
}

// targetOverlapRefusal refuses equal or nested targets that share no carrier
// (OD-3, the drive included): a target never collides with a workspace it
// belongs to, so nesting inside one workspace's own sources stays allowed. Two
// repositories cloning into one directory are refused whoever carries them,
// since only one could be cloned there; the same repository at the same path
// and ref twice is one clone, not a collision.
//
// Each target is compared with the earlier ones through a tree of path elements,
// so the cost follows the size of the list rather than its square. The refusal
// is the one a pairwise scan in list order gives: the first target that overlaps
// an earlier one, naming the earliest such.
func targetOverlapRefusal(targets []runTarget) *runRefusal {
	limit := 1
	for _, t := range targets {
		limit = max(limit, len(t.carriers)+1)
	}
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
			consider(node.exact.disjointFrom(t.carriers)) // an enclosing mount
			if node = node.children[e]; node == nil {
				break
			}
		}
		if node != nil {
			consider(node.tree.disjointFrom(t.carriers)) // an equal or enclosed mount
		}
		if earliest != nil {
			return overlapRefusal(t.target, t.name, earliest.name, earliest.target)
		}
		m := &placedMount{name: t.name, target: t.target, carriers: t.carriers, seq: seq}
		node = root
		for _, e := range elems {
			if node.children == nil {
				node.children = map[string]*mountNode{}
			}
			if node.children[e] == nil {
				node.children[e] = &mountNode{}
			}
			node.tree.add(m, limit)
			node = node.children[e]
		}
		node.tree.add(m, limit)
		node.exact.add(m, limit)
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
		case org != firstOrg && !sharesCarrier(t.carriers, first.carriers):
			return runError(http.StatusUnprocessableEntity, reasonWorkspaceADOOrgConflict,
				workspaceADOOrgConflictMsg(first.name, firstOrg, t.name, org))
		}
	}
	return nil
}

// startFolderDir resolves the run's one starting folder over its targets: ""
// when the request names none (the sandbox keeps today's choice),
// startFolderImageDefault for the image's own directory, else the absolute
// folder inside one attachment, with that attachment's index. The named
// attachment holds a target when it is one of the target's carriers, so a
// repository the policy carries and two workspaces hold starts the run under
// either; the target is then shown under the named workspace, so the mount map
// and the starting folder agree. A workspace attachment with no repo is the
// workspace's first attachment. The subpath is already relative and cleaned
// (startFolderShapeRefusal), so the folder stays inside the mount; symlinks are
// resolved inside the sandbox.
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
	for i, t := range targets {
		if t.kind == "" || !slices.Contains(t.carriers, owner) || f.Repo != "" && (t.kind != attachmentRepository || t.repo != f.Repo) {
			continue
		}
		if j := slices.IndexFunc(wsRefs, func(ws types.Workspace) bool { return ws.ID.String() == owner }); j >= 0 {
			targets[i].owner, targets[i].name = owner, wsRefs[j].Name
		}
		return strings.TrimSuffix(t.target+"/"+f.Subpath, "/"), i, nil
	}
	name := f.Repo
	if name == "" {
		name = f.Attachment
		if j := slices.IndexFunc(wsRefs, func(ws types.Workspace) bool { return ws.ID.String() == owner }); j >= 0 {
			name = wsRefs[j].Name
		}
	}
	return "", -1, runError(http.StatusBadRequest, reasonStartFolderInvalid, startFolderAttachmentMsg(name))
}

// runAttachments answers the run's cross-workspace checks in order: the base
// image (when the workspaces decide it), the starting folder, overlap, then the
// Azure DevOps organisation. It
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
	targets := runTargets(spec, wsRefs, carriers, seed, drive)
	// The starting folder first: it decides which carrier a shared source is
	// shown under, and the checks after it name that one.
	dir, at, refusal := startFolderDir(req.StartFolder, targets, wsRefs)
	if refusal != nil {
		return nil, "", refusal
	}
	if refusal := targetOverlapRefusal(targets); refusal != nil {
		return nil, "", refusal
	}
	if refusal := adoOrgConflictRefusal(targets); refusal != nil {
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
