// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import "context"

// WorkloadPolicyPins writes a workload's policy pin (workloads.policy_pin): the launch policy digest every
// launch of it must resolve to. Optional for RunPolicyDigests' reason: the api layer type-asserts, and
// production is always PG.
type WorkloadPolicyPins interface {
	// SetWorkloadPolicyPin writes pin, "" to clear it, on owner's workload name, and returns the workload as
	// it now reads and the pin it replaced. ErrNotFound when owner holds no workload of that name. It never
	// touches the spec, its generation, its run or a launch claim.
	SetWorkloadPolicyPin(ctx context.Context, owner, name, pin string) (Workload, string, error)
}

var _ WorkloadPolicyPins = PG{}

// SetWorkloadPolicyPin — see WorkloadPolicyPins. The row is locked before its pin is read, so each of two
// writers reports the pin the other left.
func (s PG) SetWorkloadPolicyPin(ctx context.Context, owner, name, pin string) (Workload, string, error) {
	var w Workload
	var prev string
	err := scanWorkloadInto(s.Pool.QueryRow(ctx, `
		WITH old AS (SELECT id, policy_pin FROM workloads WHERE owner = $1 AND name = $2 FOR UPDATE)
		UPDATE workloads w SET policy_pin = $3 FROM old WHERE w.id = old.id
		RETURNING `+workloadCols+`, old.policy_pin`, owner, name, pin), &w, &prev)
	return w, prev, err
}
