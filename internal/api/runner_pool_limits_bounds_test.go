// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPoolLimitBoundsAreTheRequestContractBounds: a pool can never name a CPU, memory
// or number of seconds the run request and the run limits themselves refuse.
func TestPoolLimitBoundsAreTheRequestContractBounds(t *testing.T) {
	if types.RunnerPoolCPUMillisMax != maxRunResourceCPU || types.RunnerPoolMemoryMiBMax != maxRunResourceMemory {
		t.Errorf("pool bounds cpu %d memory %d, request contract %d and %d",
			types.RunnerPoolCPUMillisMax, types.RunnerPoolMemoryMiBMax, maxRunResourceCPU, maxRunResourceMemory)
	}
	if types.RunnerPoolSecondsMax != maxRunLimitSec {
		t.Errorf("pool seconds bound %d, run limits %d", types.RunnerPoolSecondsMax, maxRunLimitSec)
	}
}
