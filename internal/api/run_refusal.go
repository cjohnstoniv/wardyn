// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
)

// runRefusal keeps a gate's decision separate from its HTTP/audit emission.
// Preview shares authorization decisions without executing launch readiness.
type runRefusal struct {
	drive     *driveBindFailure
	status    int
	body      errorBody
	decision  *authz.Decision
	operation string
	err       error
}

func runError(status int, reason, message string) *runRefusal {
	return &runRefusal{status: status, body: errorBody{Error: message, Reason: reason}}
}

func runDenied(decision authz.Decision) *runRefusal {
	return &runRefusal{decision: &decision}
}

// runDoorDenied is a governance door's 403 under reason for d: its sentence, and who to ask, as refuseOn
// writes it.
func runDoorDenied(reason string, d authz.Decision) *runRefusal {
	return &runRefusal{status: http.StatusForbidden, decision: &d,
		body: errorBody{Error: d.Sentence, Reason: reason, Policy: policyref.Layered(policyref.AttributedTo(d.Policy, d.Knob))}}
}

func runServerError(operation string, err error) *runRefusal {
	return &runRefusal{operation: operation, err: err}
}

func ceilingRunRefusal(err error) *runRefusal {
	if errors.Is(err, errGroupsSnapshotStale) {
		return runError(http.StatusForbidden, reasonGroupsSnapshotStale, groupsSnapshotStaleMsg)
	}
	if u, ok := isOverlayUnsatisfiable(err); ok {
		return runError(http.StatusForbidden, reasonGovernanceOverlayUnsatisfiable, u.Error())
	}
	if refusal := contextRunRefusal(err); refusal != nil {
		return refusal
	}
	return runServerError("resolve governance ceiling", err)
}

func capabilityRunRefusal(err error) *runRefusal {
	if errors.Is(err, errSecretStoreUnavailable) {
		return runError(http.StatusServiceUnavailable, reasonSinkStoreUnavailable, err.Error())
	}
	var refused *errUngrantedWorkspaceRepo
	if errors.As(err, &refused) {
		return runDenied(authz.Deny(authz.ReasonCapabilityWorkspace, "runs.workspace",
			"you are not granted workspace "+refused.wsID+" — ask an admin for access, or launch without a workspace"))
	}
	return runServerError("resolve capability", err)
}

func (f *runRefusal) write(s *Server, w http.ResponseWriter, r *http.Request) bool {
	if f == nil {
		return false
	}
	if f.drive != nil {
		f.drive.write(s, w)
	} else if f.err != nil {
		writeServerError(w, r, f.operation, f.err)
	} else if f.decision != nil && f.status == 0 {
		s.refuseOn(w, r, *f.decision, f.body.Field)
	} else {
		if f.decision != nil {
			s.recordRefusal(r.Context(), r, *f.decision)
		}
		writeJSON(w, f.status, f.body)
	}
	return true
}
