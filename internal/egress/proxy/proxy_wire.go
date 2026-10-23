// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// canonicalGitGrants canonicalises git-broker allowlist keys to lowercase "<org>/<repo>" so
// lookups match regardless of the slug casing git sends (github owner/repo is case-insensitive).
func canonicalGitGrants(in map[string]uuid.UUID) map[string]uuid.UUID {
	gitGrants := make(map[string]uuid.UUID, len(in))
	for k, id := range in {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" && id != uuid.Nil {
			gitGrants[k] = id
		}
	}
	return gitGrants
}

// writeApprovalPending returns the first-use 403 body the sandbox client sees.
func writeApprovalPending(w http.ResponseWriter, log *egress.DecisionLog) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	id := ""
	if log != nil && log.ApprovalID != nil {
		id = log.ApprovalID.String()
	}
	// {"wardyn":"approval-pending","approval_id":...}
	_, _ = fmt.Fprintf(w, `{"wardyn":"approval-pending","approval_id":%q}`, id)
}
