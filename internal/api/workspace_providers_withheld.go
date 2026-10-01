// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// withheldScmHosts is effectiveScmHosts' other half: the hosts a DISABLED row
// claims that the effective set leaves out, each with the row that withholds it.
// Fail-closed stays as it was (the host is out of egress and refused at launch);
// this only says why, so a missing host is not a silent one. A host another
// enabled row admits is in the effective set and is never listed here.
func withheldScmHosts(sc types.SiteConfig) []types.WithheldScmHost {
	rows := gitProviderRows(sc)
	live := map[string]bool{}
	for _, h := range effectiveScmHosts(sc) {
		live[h] = true
	}
	var out []types.WithheldScmHost
	seen := map[string]bool{}
	add := func(h string, row types.GitProvider) {
		if h == "" || live[h] || seen[h] {
			return
		}
		seen[h] = true
		out = append(out, types.WithheldScmHost{Host: h, ProviderID: row.ID, ProviderKind: row.Kind})
	}
	for _, row := range rows {
		if !row.Disabled {
			continue
		}
		for _, raw := range row.BaseURLs {
			add(hostrules.HostOf(raw), row)
		}
	}
	for _, raw := range sc.ScmHosts {
		h := strings.ToLower(strings.TrimSpace(raw))
		for _, row := range rows {
			if row.Disabled && rowClaimsHost(row, cloneTarget{host: h}) {
				add(h, row)
				break
			}
		}
	}
	return out
}
