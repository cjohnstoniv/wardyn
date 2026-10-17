// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"fmt"
	"net/http"
	"slices"
	"strings"

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
	// seeded it or the one the onboarding gate held it to, and every other of the
	// run's workspaces that carries the same source. The owner is always one of them.
	carriers []string
	target   string
	driveID  string // a drives[] entry's id
	repo     string
	ref      string
	readOnly bool
}

// sharesCarrier reports whether two targets belong to a common workspace.
func sharesCarrier(a, b []string) bool {
	return slices.ContainsFunc(a, func(id string) bool { return slices.Contains(b, id) })
}

// runTargets lists every resolved path of a run, the drives first (driveTargets,
// each at its own server-derived path), then each workspace's mounts, clones and
// scratch directories in the spec's order (the seeded workspaces lead it, the
// primary first).
func runTargets(spec types.RunPolicySpec, wsRefs []types.Workspace, index workspaceSourceIndex, seed workspaceSeed, drives []runTarget) []runTarget {
	var out []runTarget
	for _, d := range drives {
		d.carriers = []string{d.owner}
		out = append(out, d)
	}
	// A source the policy carries is shown under the workspace the onboarding
	// gate held it to (authorizeSpecWorkspaceSources reads the same index) and
	// belongs to every one of the run's workspaces that carries it. Every mount
	// and repo of a run passed that gate, so one always does; an unowned one is
	// still its own owner, never everyone's.
	carriedBy := func(owner types.Workspace, local bool, source string) runTarget {
		t := runTarget{owner: owner.ID.String(), name: owner.Name, carriers: []string{owner.ID.String()}}
		for _, ws := range wsRefs {
			if ws.ID != owner.ID && slices.ContainsFunc(ws.Sources, func(src types.WorkspaceSource) bool {
				return local && src.Type == types.WorkspaceSourceTypeLocalDir && src.Path == source ||
					!local && src.Type == types.WorkspaceSourceTypeRepo && src.Source == source
			}) {
				t.carriers = append(t.carriers, ws.ID.String())
			}
		}
		return t
	}
	policyOwned := func(local bool, source, fallback string) runTarget {
		gate, ok := index.repo[source]
		if local {
			gate, ok = index.localDir[source]
		}
		if !ok {
			return runTarget{owner: "source:" + source, name: fallback, carriers: []string{"source:" + source}}
		}
		return carriedBy(gate, local, source)
	}
	// The entries the seed prepended are shown under the workspace that seeded them.
	owned := func(owners []types.Workspace, i int, local bool, source, fallback string) runTarget {
		if i < len(owners) {
			return carriedBy(owners[i], local, source)
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

// carrierSet is a target's carriers for the overlap tree, interned and sorted,
// so comparing two is one merge of short lists rather than a scan of one per
// element of the other.
type carrierSet struct {
	ids []int
	key string // the set as one string, for an exact set match
}

// carrierSets builds the carrierSet of each target.
func carrierSets(targets []runTarget) []carrierSet {
	index := map[string]int{}
	out := make([]carrierSet, len(targets))
	for i, t := range targets {
		for _, id := range t.carriers {
			n, ok := index[id]
			if !ok {
				n = len(index)
				index[id] = n
			}
			out[i].ids = append(out[i].ids, n)
		}
		slices.Sort(out[i].ids)
		out[i].ids = slices.Compact(out[i].ids)
		out[i].key = fmt.Sprint(out[i].ids)
	}
	return out
}

// placedMount is one target placed in the overlap tree; seq is its position.
type placedMount struct {
	name, target string
	set          carrierSet
	seq          int
}

// disjoint reports whether two carrier sets share no carrier.
func disjoint(a, b carrierSet) bool {
	for i, j := 0, 0; i < len(a.ids) && j < len(b.ids); {
		switch {
		case a.ids[i] == b.ids[j]:
			return false
		case a.ids[i] < b.ids[j]:
			i++
		default:
			j++
		}
	}
	return true
}

// firstMounts answers "the earliest mount placed here that shares no carrier
// with x". A mount with one carrier is kept only as the first of its owner, and
// only up to limit owners: when the earliest mount disjoint from x was not kept,
// limit owners came before it, and since x has fewer carriers than limit, one
// of those earlier mounts is disjoint from x too. A mount with several carriers
// (a source two of the run's workspaces carry, which is rare) is kept as the
// first of its carrier set: an earlier mount with the same set is disjoint from
// x whenever it is. That bounds the list by the distinct shared sources, not
// by how many targets repeat one.
type firstMounts struct {
	single, multi []*placedMount
	multiSets     map[string]bool // the carrier sets in multi
}

func (f *firstMounts) add(m *placedMount, limit int) {
	switch {
	case len(m.set.ids) > 1:
		if !f.multiSets[m.set.key] {
			if f.multiSets == nil {
				f.multiSets = map[string]bool{}
			}
			f.multiSets[m.set.key] = true
			f.multi = append(f.multi, m)
		}
	case len(f.single) < limit && !slices.ContainsFunc(f.single, func(p *placedMount) bool { return p.set.ids[0] == m.set.ids[0] }):
		f.single = append(f.single, m)
	}
}

func (f *firstMounts) disjointFrom(x carrierSet) *placedMount {
	var earliest *placedMount
	for _, list := range [][]*placedMount{f.single, f.multi} {
		if i := slices.IndexFunc(list, func(m *placedMount) bool { return disjoint(m.set, x) }); i >= 0 &&
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
	sets := carrierSets(targets)
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
			consider(node.exact.disjointFrom(sets[seq])) // an enclosing mount
			if node = node.children[e]; node == nil {
				break
			}
		}
		if node != nil {
			consider(node.tree.disjointFrom(sets[seq])) // an equal or enclosed mount
		}
		if earliest != nil {
			return overlapRefusal(t.target, t.name, earliest.name, earliest.target)
		}
		m := &placedMount{name: t.name, target: t.target, set: sets[seq], seq: seq}
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
// claim, so a run spanning two would need two pins and gets neither. Two
// repositories in different organisations are refused unless one workspace
// carries both (two organisations inside one workspace keep today's rule).
// Every pair is compared: sharing a workspace is not transitive, so comparing
// with one repository would let a third organisation through.
func adoOrgConflictRefusal(targets []runTarget) *runRefusal {
	type seen struct {
		t        runTarget
		org, key string
	}
	var earlier []seen
	for _, t := range targets {
		if t.kind != attachmentRepository || t.owner == "" {
			continue
		}
		org, ok := adoOrganisationOf(t.repo)
		if !ok {
			continue
		}
		key := strings.Join(slices.Sorted(slices.Values(t.carriers)), ",")
		for _, e := range earlier {
			if e.org != org && !sharesCarrier(t.carriers, e.t.carriers) {
				return runError(http.StatusUnprocessableEntity, reasonWorkspaceADOOrgConflict,
					workspaceADOOrgConflictMsg(e.t.name, e.org, t.name, org))
			}
		}
		// One repository per organisation and carrier set answers every later pair.
		if !slices.ContainsFunc(earlier, func(e seen) bool { return e.org == org && e.key == key }) {
			earlier = append(earlier, seen{t, org, key})
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
	switch {
	case f.DriveID != "":
		owner = startFolderDriveOwner(f.DriveID)
	case owner == client.StartFolderDrive:
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
		name = cmp.Or(f.Attachment, f.DriveID)
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
func (s *Server) runAttachments(req createRunRequest, spec types.RunPolicySpec, wsRefs []types.Workspace, carriers workspaceSourceIndex, seed workspaceSeed, drives []runTarget) (
	[]client.RunAttachment, string, *runRefusal,
) {
	if seed.imageOpen {
		if _, _, refusal := workspacesBaseImage(wsRefs, nil); refusal != nil {
			return nil, "", refusal
		}
	}
	targets := runTargets(spec, wsRefs, carriers, seed, drives)
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
		case attachmentDrive:
			a.DriveID = t.driveID
		}
		out = append(out, a)
	}
	return out
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
