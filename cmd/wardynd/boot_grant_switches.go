// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// unitPutCreates is api.Config.GovernanceUnitPutCreates: true only with
// WARDYN_GOVERNANCE_UNIT_PUT_EXISTING_ONLY set off.
func (f *bootFlags) unitPutCreates() bool { return !*f.governanceUnitPutExistingOnly }

