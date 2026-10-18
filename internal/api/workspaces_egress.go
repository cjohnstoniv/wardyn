// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maxApprovedEgress bounds the operator-owned approved-egress list — matches
// the scanner's SuggestedEgress cap, its only intended feeder.
const maxApprovedEgress = 64

// handleSetApprovedEgress replaces the workspace's operator-owned approved
// egress list (PUT semantics: the body is the FULL list; un-approve by
// omission — idempotent, no per-host delete endpoint needed). These hosts
// originate from the scanner's content-derived SuggestedEgress — untrusted
// repo content — so promotion is an explicit operator action, audited, and
// the list lives OUTSIDE the scan-owned profile blob: a rescan can neither
// widen nor resurrect it. Plain lowercase dotted hosts only.
func (s *Server) handleSetApprovedEgress(w http.ResponseWriter, r *http.Request) {
	type body struct {
		Domains []string `json:"domains"`
	}
	// A host the git broker (or the control plane itself) already
	// owns is dead by construction as a direct ApprovedEgress entry — dispatch
	// routes it through the broker/proxy specially, never as a plain allowlist
	// host, so "approving" one here writes a row a real run's proxy will never
	// consult. This is the SAME static skip-set promoteSkipHosts applies to the
	// bulk promote-egress writer (record.go) plus the control-plane's own host
	// (handlePromoteRecordEgress's selfHost) — this is the LAST writer of
	// ApprovedEgress that did not share it; reject with the same honest 4xx
	// the shape validator already uses instead of a silent-toast no-op.
	// The set itself is deadApprovedEgressHosts (workspaces_canon.go), shared
	// with the observed-egress panel that produces what gets promoted here.
	deadHosts := s.deadApprovedEgressHosts()
	scopedWorkspaceWrite(s, w, r, "workspace.egress.approve",
		func(req body) ([]string, string, string) {
			if len(req.Domains) > maxApprovedEgress {
				return nil, reasonWorkspaceApprovedEgressInvalid, "too many domains (max 64)"
			}
			set := map[string]struct{}{}
			for _, d := range req.Domains {
				d = strings.ToLower(strings.TrimSpace(d))
				if !hostrules.ValidApprovedHost(d) {
					return nil, reasonWorkspaceApprovedEgressInvalid, "invalid domain (plain lowercase host, no scheme/port/wildcard): " + d
				}
				if _, dead := deadHosts[d]; dead {
					return nil, reasonWorkspaceApprovedEgressDeadHost, "host " + d + " is already routed specially (git broker / control plane) — a direct ApprovedEgress entry for it is never consulted"
				}
				set[d] = struct{}{}
			}
			return sortedKeys(set), "", ""
		},
		// Wrapped, not passed as a method value: the store call must not be
		// resolved until validation has passed.
		func(ctx context.Context, id uuid.UUID, doms []string) (types.Workspace, error) {
			return s.cfg.Store.SetWorkspaceApprovedEgress(ctx, id, doms)
		},
		func(domains []string) map[string]any { return map[string]any{"domains": domains} })
}

// handleSetDeniedEgress replaces the workspace's operator-owned denied-egress
// list (PUT semantics: the body is the FULL list; un-deny by omission — same
// idempotent, no-per-host-delete shape as handleSetApprovedEgress). Backs
// the revocation surface (SetWorkspaceDeniedEgress, store.go): this is
// the ONLY way to undo a `deny · always` decision once made — including a
// deny on a model-provider host, which permanently
// defeats that workspace's credential injection (deny beats allow at the
// proxy, policy.go), because api.anthropic.com/similar can only ever reach
// AllowedDomains through modelProviderEgress, never through this handler's own
// validator. So this route is also the only cure for an ALREADY-bricked
// workspace, which is exactly why its validator is deliberately narrower than
// handleSetApprovedEgress's:
//
// Do NOT copy handleSetApprovedEgress's deadHosts set here. That set is
// ALLOW-shaped — git-broker/control-plane hosts a real run's proxy never
// consults as a plain ApprovedEgress entry, so promoting one is dead weight —
// and it carries no model-provider guard. Copying it would make this PUT a
// SECOND, unguarded door to that brick, on the one route whose entire job is
// to be the escape hatch FROM that brick. So this validates only
// hostrules.ValidApprovedHost (plain lowercase dotted host, no scheme/port/
// wildcard) and nothing else — deliberately unguarded, on purpose, because a
// deny-direction guard here would remove the only way to undo an over-broad
// one.
func (s *Server) handleSetDeniedEgress(w http.ResponseWriter, r *http.Request) {
	type body struct {
		Domains []string `json:"domains"`
	}
	scopedWorkspaceWrite(s, w, r, "workspace.egress.deny",
		func(req body) ([]string, string, string) {
			if len(req.Domains) > maxApprovedEgress {
				return nil, reasonWorkspaceDeniedEgressInvalid, "too many domains (max 64)"
			}
			set := map[string]struct{}{}
			for _, d := range req.Domains {
				d = strings.ToLower(strings.TrimSpace(d))
				if !hostrules.ValidApprovedHost(d) {
					return nil, reasonWorkspaceDeniedEgressInvalid, "invalid domain (plain lowercase host, no scheme/port/wildcard): " + d
				}
				set[d] = struct{}{}
			}
			return sortedKeys(set), "", ""
		},
		// Wrapped, not passed as a method value: the store call must not be
		// resolved until validation has passed.
		func(ctx context.Context, id uuid.UUID, doms []string) (types.Workspace, error) {
			return s.cfg.Store.SetWorkspaceDeniedEgress(ctx, id, doms)
		},
		func(domains []string) map[string]any { return map[string]any{"domains": domains} })
}
