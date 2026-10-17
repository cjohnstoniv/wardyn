// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// usageDownStore reads runners but fails the usage read, as a dropped connection would.
type usageDownStore struct{ *runnerRegistrationFake }

func (usageDownStore) RunnerUsage(context.Context, []uuid.UUID, []string, uuid.UUID) (map[string]types.RunnerTally, error) {
	return nil, errors.New("pg: connection reset")
}

// TestRunnerFits_AFailedReadIsAnErrorNotAnEmptyAnswer: an empty answer reads as "declares no limits"
// and sent the person advice to set limits on a runner that has them. The read's failure reaches the
// placement step as a server error, which answers a retryable failure.
func TestRunnerFits_AFailedReadIsAnErrorNotAnEmptyAnswer(t *testing.T) {
	srv, f, _ := newRunnerRegistrationServer(t)
	srv.cfg.Store = usageDownStore{f}

	fits, err := srv.runnerFits(context.Background())
	if err == nil || fits != nil {
		t.Fatalf("runnerFits = %v, %v; want the usage read's error", fits, err)
	}
	_, refusal := srv.placementDemand(context.Background(), placementInput{req: &createRunRequest{}})
	if refusal == nil || refusal.err == nil {
		t.Fatalf("placementDemand refusal = %+v; want a server error (internal_error), not a limits refusal", refusal)
	}
}
