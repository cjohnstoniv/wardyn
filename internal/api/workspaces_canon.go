// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// workspaces_canon.go holds the workspace door's canonicalisation and bound
// helpers. They live beside workspaces.go rather than in it because that file
// is at the repo's 1000-line ceiling (scripts/check-file-size.sh) and new logic
// for a file at the cap goes into a new file, never an allowlist entry.

// maxWorkspaceSources bounds a workspace composition, the last unbounded list
// on the workspace door (siblings: maxWorkspaceRequirements,
// maxApprovedEgress, maxProviderBaseURLs). Every entry costs one UpsertSource
// plus one hash-chained source.write audit append, and POST/PUT /workspaces is
// member-reachable, so an uncapped list is a serialised write chain one request
// can start. 64 is far past any real composition.
const maxWorkspaceSources = 64

// DRAFT (M2 canon pending)

const (
	// workspaceSources400TooMany is the cap refusal for POST/PUT /workspaces.
	// Spelled like its siblings' ("too many requirements (max %d)", "too many
	// domains (max 64)") so the door answers one shape for every bound it has.
	workspaceSources400TooMany = "too many sources (max %d)"
	// workspaceDelete409ActiveRun is the in-use refusal for DELETE
	// /workspaces/{id} — handleDeleteSource's own shape (name what is holding
	// it, tell the caller what to do), because the caller's only alternative to
	// being told is a silently lost recording. It names the run id rather than a
	// title: the id is what /runs/{id} takes and what the audit trail keys on.
	workspaceDelete409ActiveRun = "workspace is in use by run %s — stop that run first, then delete"
)

// canonicalWorkspaceSource returns src with its IDENTITY fields spelled the way
// the library stores them — canonicalSourceIdentity, the same normalisation
// upsertAndAttach applies before every UpsertSource. Compare-only: nothing here
// is persisted, so the operator's authored spelling still round-trips.
//
// The store keeps what upsertAndAttach gave it and hydrateWorkspace serves the
// sources BACK from those library rows, so ws.Sources on a read is always
// canonical while a request body carries whatever the caller typed. Comparing
// the two raw is comparing two different alphabets.
func canonicalWorkspaceSource(src types.WorkspaceSource) types.WorkspaceSource {
	switch src.Type {
	case types.WorkspaceSourceTypeLocalDir:
		// A local_dir carries no ref in the library (canonicalSourceIdentity
		// returns "" for one); a body that sets one is asking for nothing, so it
		// is not content either.
		src.Path, src.Ref = canonicalSourceIdentity(types.SourceLocalDir, src.Path, "")
	case types.WorkspaceSourceTypeRepo:
		src.Source, src.Ref = canonicalSourceIdentity(types.SourceRepo, src.Source, src.Ref)
	}
	return src
}

// workspaceSourceContentEqual compares everything about a WorkspaceSource
// EXCEPT Overrides: content — Type/Path/Source/Ref/Target/Writable — is what
// "sourcesChanged" (handleUpdateWorkspace) means by "everything reviewed
// against the old sources is stale". An Overrides edit changes which
// requirement rows apply, never what's mounted, so it must not reset
// ApprovedEgress/Requirements/RecordResults (and the Overrides map makes the
// type non-comparable for slices.Equal). Identity is compared canonically: a
// re-PUT differing only in slug case, a ".git" suffix or clone-URL host case
// must not read as a CONTENT CHANGE, which would silently discard the reviewed
// state and delete the built image; the library treats those as ONE source.
func workspaceSourceContentEqual(a, b types.WorkspaceSource) bool {
	a, b = canonicalWorkspaceSource(a), canonicalWorkspaceSource(b)
	return a.Type == b.Type && a.Path == b.Path && a.Source == b.Source &&
		a.Ref == b.Ref && a.Target == b.Target && a.Writable == b.Writable
}

// deadApprovedEgressHosts is the set of hosts a direct ApprovedEgress entry can
// never do anything for: everything the git broker owns (dispatch routes
// github.com/api.github.com/codeload.github.com/*.githubusercontent.com and
// every SSH-over-443 forge host through the broker/proxy specially —
// runs_dispatch_gitbroker.go — never as a plain allowlist host) plus the
// control plane's own host.
//
// ONE set, TWO callers: handleSetApprovedEgress 400s the WHOLE list when any
// entry is in it, and handleObservedEgress PRODUCES the candidates an operator
// promotes. They must agree, or one dead suggestion breaks the promote for
// every real host beside it.
func (s *Server) deadApprovedEgressHosts() map[string]struct{} {
	dead := map[string]struct{}{}
	add := func(hosts []string) {
		for _, h := range hosts {
			dead[strings.ToLower(strings.TrimSpace(h))] = struct{}{}
		}
	}
	add(gitBrokerManagedHosts)
	add(gitBrokerSSHHosts())
	if selfHost := controlPlaneHost(s.cfg.ControlPlaneURL); selfHost != "" {
		dead[selfHost] = struct{}{}
	}
	return dead
}
