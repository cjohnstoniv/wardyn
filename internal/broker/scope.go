// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// githubPermissionCeiling is the branch-confinement maximum for github_token
// grants: a Wardyn-governed agent may at most write file contents and open
// pull requests. Anything stronger (administration, workflows, secrets, ...)
// is structurally refused — minted permissions are intersected DOWN to this.
//
// "write" is the strongest level we will hand out; a requested "admin" is
// clamped to "write", and a requested "read" is preserved (narrowing is fine).
var githubPermissionCeiling = map[string]string{
	"contents":      "write",
	"pull_requests": "write",
	"metadata":      "read", // implicitly granted by GitHub; record it explicitly
}

// permissionRank orders GitHub permission access levels for clamping.
var permissionRank = map[string]int{"read": 1, "write": 2, "admin": 3}

// clampGitHubPermissions intersects requested permissions DOWN to the
// branch-confinement ceiling. Keys outside the ceiling are dropped (fail
// closed); values stronger than the ceiling are reduced to it. The ceiling's
// metadata:read is always included so clone/fetch works. This NEVER widens.
func clampGitHubPermissions(requested map[string]string) map[string]string {
	out := map[string]string{"metadata": "read"}
	for perm, maxLevel := range githubPermissionCeiling {
		req, ok := requested[perm]
		if !ok {
			continue
		}
		out[perm] = minLevel(req, maxLevel)
	}
	return out
}

// minLevel returns the weaker of two access levels (read < write < admin),
// defaulting unknown levels to "read" (fail closed).
func minLevel(a, b string) string {
	ra, oka := permissionRank[a]
	rb, okb := permissionRank[b]
	if !oka {
		ra = 1
		a = "read"
	}
	if !okb {
		rb = 1
		b = "read"
	}
	if ra <= rb {
		return a
	}
	return b
}

// ttlFor returns the minted credential TTL, clamped to defaultMaxTTL. A zero or
// over-max GrantSpec.TTLSeconds yields the 1h cap; narrowing is honored.
func ttlFor(spec types.GrantSpec) time.Duration {
	if spec.TTLSeconds <= 0 {
		return defaultMaxTTL
	}
	ttl := time.Duration(spec.TTLSeconds) * time.Second
	if ttl > defaultMaxTTL {
		return defaultMaxTTL
	}
	return ttl
}

// gitPATUsername resolves the git username to pair with a brokered PAT for a
// non-GitHub host. An explicit override always wins; otherwise Azure DevOps
// (dev.azure.com and *.visualstudio.com) uses "pat" and everything else (GitLab
// and other PAT hosts) uses "oauth2".
//
// ponytail: this two-row table covers the ADO/GitLab conventions Wardyn brokers
// today; extend it if another host needs a different username convention.
func gitPATUsername(host, override string) string {
	if override != "" {
		return override
	}
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "dev.azure.com" || strings.HasSuffix(h, ".visualstudio.com") {
		return "pat"
	}
	return "oauth2"
}

// newJTI returns a fresh, opaque credential id for the audit/revocation join.
func newJTI() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand should not fail; fall back to a uuid so we never panic.
		return uuid.NewString()
	}
	return hex.EncodeToString(b[:])
}

// spiffeForRun builds the canonical run SPIFFE id used as the audit actor when
// the caller claims are synthesized (e.g. MintOnApproval / revoke cascade).
func spiffeForRun(runID uuid.UUID) string {
	return "spiffe://wardyn.local/agent-run/" + runID.String()
}
