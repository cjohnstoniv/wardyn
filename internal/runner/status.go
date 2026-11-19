// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import "github.com/cjohnstoniv/wardyn/internal/types"

// Status reports observed sandbox state.
type Status struct {
	State    types.RunState
	ExitCode *int
	Message  string
	// FailureHint is a sentence for the run's person saying why the run ended; a run finalized from
	// this status records it as its failure hint. Empty from every driver except Kubernetes with the
	// agent-sandbox backend, which runs only in the server, so it is never sent between a runner and
	// the server: runner-supplied text never becomes a person-facing reason.
	FailureHint string `json:"-"`
}
