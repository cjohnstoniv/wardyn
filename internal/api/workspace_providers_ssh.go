// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The SSH lane's path-scoping rule (#380): split out of workspace_providers.go
// by seam (check-file-size.sh) rather than trimmed for length. sshLaneExceeds
// PathScope is the shared predicate validateProviderLanes refuses on (the
// console door, always) and the site-config door grandfathers instead of
// refusing (F2) — sshLaneWidePastPathRows/logWarnSSHLaneWidePastPath are that
// door's report-and-warn half, read AFTER validation on the block about to be
// persisted.
package api

import (
	"log/slog"
	"net/url"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sshLaneExceedsPathScope reports whether row's own base URLs would leave the
// SSH lane wider than they read: true when every base URL matching an
// SSH-over-443 host (sshOver443Endpoint) carries a path. SSH scoping is
// host-level only (cloneTarget), so a bare-host base URL for that host means the
// row already claims the whole host and the SSH lane widens nothing; a path on
// every matching entry means the SSH lane silently drops the path the https
// lanes enforce.
func sshLaneExceedsPathScope(row types.GitProvider) bool {
	sawSSHHost := false
	for _, raw := range row.BaseURLs {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			continue
		}
		host := strings.ToLower(u.Hostname())
		if _, ok := sshOver443Endpoint(host); !ok {
			continue
		}
		sawSSHHost = true
		if strings.Trim(u.Path, "/") == "" {
			return false // this entry already bounds the whole host
		}
	}
	return sawSSHHost
}

// sshLaneWidePastPathRows lists the row IDs (#380 F2) that the CONSOLE door
// would refuse (an explicit SSH lane exceeding the row's own path scope) but
// that the site-config door admits anyway, grandfathering a document written
// before this rule existed. Read AFTER validation succeeds, on the block
// about to be persisted, so handlePutSiteConfig can warn loudly — in the
// response body (a true report, never a refusal, the onboarding_completed_
// at_ignored/dangling_secret_refs precedent) and in the deployment log — the
// same way the console names the row and lets an admin fix it deliberately.
func sshLaneWidePastPathRows(p *types.WorkspaceProviders) []string {
	if p == nil {
		return nil
	}
	var out []string
	for _, row := range p.Git {
		if slices.Contains(row.Lanes, types.GitLaneSSH) && sshLaneExceedsPathScope(row) {
			out = append(out, row.ID)
		}
	}
	return out
}

// logWarnSSHLaneWidePastPath is the site-config door's LOUD half of the
// grandfather (#380 F2): the write still succeeds (sshLaneWidePastPathRows'
// rows are reported in the response, never refused), but an operator watching
// the deployment log — not staring at a `site-config apply` terminal — must
// still learn that ROW admits its whole host over SSH, same as the console's
// own admitSSHHostLevel warning names the repo. No rows => silent.
func logWarnSSHLaneWidePastPath(rows []string) {
	if len(rows) == 0 {
		return
	}
	slog.Warn("wardynd: an explicit SSH lane on an org-scoped provider row admits the whole host over SSH — "+
		"drop the SSH lane or widen the row's addresses to the bare host to close it",
		slog.Any("provider_row_ids", rows))
}
