// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"errors"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// SnapshotArgs is the snapshot calls' input. Ref names the sandbox of a cut, a commit or an abort; ID is the
// snapshot (every call but list_snapshots). No call carries layer bytes: a layer never leaves its runner.
type SnapshotArgs struct {
	Ref string    `json:"ref,omitempty"`
	ID  uuid.UUID `json:"id,omitempty"`
}

// SnapshotReply is the snapshot calls' answer: the cut, the committed layer, or the runner's layers. A
// snapshot refusal comes back as Refusal, its reason, rather than an error code, so an organisation reads it
// as the sentinel it stands for (Err).
type SnapshotReply struct {
	Cut     *runner.SnapshotCut    `json:"cut,omitempty"`
	Layer   *runner.SnapshotLayer  `json:"layer,omitempty"`
	Layers  []runner.SnapshotLayer `json:"layers,omitempty"`
	Refusal string                 `json:"refusal,omitempty"`
}

var snapshotRefusals = []struct {
	reason string
	err    error
}{
	{"snapshot_unsupported", runner.ErrSnapshotUnsupported},
	{"snapshot_not_pending", runner.ErrSnapshotNotPending},
	{"snapshot_in_use", runner.ErrSnapshotInUse},
	{"snapshot_busy", runner.ErrSnapshotBusy},
	{"snapshot_no_space", runner.ErrSnapshotNoSpace},
	{"fork_base_changed", runner.ErrForkBaseChanged},
}

// SnapshotRefusal is the reason err carries when it is a snapshot refusal, and "" for any other error.
func SnapshotRefusal(err error) string {
	for _, r := range snapshotRefusals {
		if errors.Is(err, r.err) {
			return r.reason
		}
	}
	return ""
}

// Err is the refusal the reply carries, as its sentinel; nil when it carries none. A reason this side does
// not know is still a refusal, never a success.
func (r SnapshotReply) Err() error {
	if r.Refusal == "" {
		return nil
	}
	for _, s := range snapshotRefusals {
		if s.reason == r.Refusal {
			return s.err
		}
	}
	return Refuse("snapshot refused: %.80s", r.Refusal)
}
