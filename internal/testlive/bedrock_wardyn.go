// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package testlive

import (
	"errors"
	"fmt"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// adminTokenPrincipal mirrors internal/api/runs_policy.go's own constant: the
// sentinel principal an admin-bearer-authenticated action is attributed to.
// Duplicated rather than imported — internal/testlive holds no dependency on
// internal/api, and this sentinel is a small, stable, publicly documented
// wire fact (pkg/client's own doc comment states it), not an internal.
const adminTokenPrincipal = "admin-token"

// BedrockWardynCredentialRowsOK reports a descriptive error unless events (a
// completed run's own audit trail, GET /api/v1/audit?run_id=) carries at
// least one credential.* row, and NONE of them is attributed to the shared
// admin bearer — the per-user proof LL3w exists for: this run's own
// credential activity is scoped to it (internal/api/internal.go's
// "credential.mint" carries the run's own SPIFFE identity, never a shared
// one), not the admin token every other lane could reach for.
func BedrockWardynCredentialRowsOK(events []types.AuditEvent) error {
	var n int
	for _, e := range events {
		if !strings.HasPrefix(e.Action, "credential.") {
			continue
		}
		n++
		if e.Actor == adminTokenPrincipal {
			return fmt.Errorf("credential row %s (action %s) is attributed to the shared admin bearer, not this run", e.ID, e.Action)
		}
	}
	if n == 0 {
		return errors.New("no credential.* audit row on this run: the model call may not have carried a per-run credential at all")
	}
	return nil
}

// BedrockWardynForcedFaultOK reports a descriptive error unless hint is the
// sentence internal/api/bedrock_dataplane_fault.go's bedrockFaultHints writes
// to a run's failure_hint when AWS refuses the model call with the given
// error class ("AccessDeniedException" or "ThrottlingException") — the
// forced-fault half of LL3w. class must be one of those two exact strings.
func BedrockWardynForcedFaultOK(hint, class string) error {
	switch class {
	case "AccessDeniedException", "ThrottlingException":
	default:
		return fmt.Errorf("BedrockWardynForcedFaultOK: unknown fault class %q", class)
	}
	if !strings.Contains(hint, class) {
		return fmt.Errorf("run failure_hint does not name %s: %q", class, hint)
	}
	return nil
}
