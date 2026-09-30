// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunPolicyView is GET /api/v1/runs/{id}/policy: the policy a run got, where it
// started from, and what Wardyn changed when it started.
//
// State is "recorded", "not_yet" (the run has not reached sandbox setup) or
// "never" (it ended before that, so no policy was applied); Spec and
// RecordedAt are set only for "recorded". Spec is a normal policy document, with
// hidden values replaced (see Redacted). Complete is false for a run launched
// before Wardyn recorded each change, whose Changes may be missing entries.
type RunPolicyView struct {
	RunID      uuid.UUID            `json:"run_id"`
	State      string               `json:"state"`
	RecordedAt *time.Time           `json:"recorded_at,omitempty"`
	Source     RunPolicySource      `json:"source"`
	Spec       *types.RunPolicySpec `json:"spec,omitempty"`
	// Redacted is true when the reader is below the security admin tier and the
	// policy holds mount sources or secret names, which read as "<redacted>" or
	// are dropped. Such a spec is a starting point, not a policy that validates.
	Redacted bool `json:"redacted"`
	// Changes is never null.
	Changes  []RunPolicyChange `json:"changes"`
	Complete bool              `json:"complete"`
	// StoredPolicyNow is set when the run started from a saved policy.
	StoredPolicyNow *StoredPolicyNow `json:"stored_policy_now,omitempty"`
}

// The RunPolicyView.State values.
const (
	RunPolicyViewRecorded = "recorded"
	RunPolicyViewNotYet   = "not_yet"
	RunPolicyViewNever    = "never"
)

// RunPolicySource says where a run's policy started: Kind is "stored" (a saved
// policy, PolicyID and Name as of launch, Deleted once it is gone), "inline",
// "default", "profile" (Name is the governance profile), or "unknown".
type RunPolicySource struct {
	Kind          string     `json:"kind"`
	PolicyID      *uuid.UUID `json:"policy_id,omitempty"`
	Name          string     `json:"name,omitempty"`
	Deleted       bool       `json:"deleted,omitempty"`
	Preset        string     `json:"preset,omitempty"`
	PresetVersion int        `json:"preset_version,omitempty"`
}

// RunPolicyChange is one group of entries changed at launch for one Cause:
// workspace, source_control, mirror, model_access, git_broker, profile,
// org_disk, restart, limits or launch. Field is a RunPolicySpec json name.
// Entries are keys that never name a hidden value: a grant is "kind:host" or
// "kind:repo,repo", a mount its target, a repo "repo@ref", an app its name.
type RunPolicyChange struct {
	Cause   string   `json:"cause"`
	Field   string   `json:"field"`
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
	// Detail is the launch-time narrowing sentences, verbatim (Cause "limits" only).
	Detail  []string   `json:"detail,omitempty"`
	Profile string     `json:"profile,omitempty"`
	At      *time.Time `json:"at,omitempty"` // when the run was restarted (Cause "restart")
}

// StoredPolicyNow compares the saved policy a run started from with what it
// reads now: State is "same", "changed", "updated" (an older run whose saved
// policy was edited since — possibly a rename only) or "deleted".
type StoredPolicyNow struct {
	State     string     `json:"state"`
	Name      string     `json:"name,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// GetRunPolicy returns the policy a run got (owner-or-admin; a run the caller
// cannot read is a 404). GET /api/v1/runs/{id}/policy.
func (c *Client) GetRunPolicy(ctx context.Context, runID uuid.UUID) (RunPolicyView, error) {
	var out RunPolicyView
	err := c.do(ctx, http.MethodGet, "/api/v1/runs/"+runID.String()+"/policy", nil, &out)
	return out, err
}
