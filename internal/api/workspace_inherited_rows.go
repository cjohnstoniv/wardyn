// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"maps"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// withholdInheritedRows is the owner's read of a workspace's inherited requirement rows. The owner of a
// workspace reads it in full, but a workspace attaching a library source someone else onboarded inherits
// that source's requirements, which an operator may have annotated with a stored secret's name or an
// internal host: owning the workspace is not a read of the source. So an owner who is not an operator
// reads those rows as the member tier does (withholdInheritedRequirements). Every other reader is
// unchanged: an operator reads in full, and the other tiers already lose these keys.
func (s *Server) withholdInheritedRows(r *http.Request, ws types.Workspace) types.Workspace {
	if s.workspaceReadTierFor(r, ws) != workspaceReadFull || s.isOperator(r.Context()) {
		return ws
	}
	return withholdInheritedRequirements(ws)
}

// withholdInheritedRequirements is ws with each effective requirement row it inherits from a source
// projected as the member tier reads it (redactRequirementsForRead). A row the workspace's own
// requirements hold stays, and so does a row a scan found (provenance scan_seeded): a scan reads the
// source's content, which the owner's run reads too. The stored rows are untouched; runs read them.
func withholdInheritedRequirements(ws types.Workspace) types.Workspace {
	if len(ws.EffectiveRequirements) == 0 {
		return ws
	}
	out := make(map[string]types.WorkspaceRequirement, len(ws.EffectiveRequirements))
	inherited := map[string]types.WorkspaceRequirement{}
	for k, v := range ws.EffectiveRequirements {
		if _, own := ws.Requirements[k]; own || v.Provenance == "scan_seeded" {
			out[k] = v
			continue
		}
		inherited[k] = v
	}
	maps.Copy(out, redactRequirementsForRead(inherited, workspaceReadMember))
	ws.EffectiveRequirements = out
	return ws
}
