// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// unverifiedEmailGrants is api.Config.UnverifiedEmailCapGrantsMatch: true only
// with WARDYN_CAPABILITY_GRANTS_VERIFIED_EMAIL set off.
func (f *bootFlags) unverifiedEmailGrants() bool { return !*f.capabilityGrantsVerifiedEmail }

// unitPutCreates is api.Config.GovernanceUnitPutCreates: true only with
// WARDYN_GOVERNANCE_UNIT_PUT_EXISTING_ONLY set off.
func (f *bootFlags) unitPutCreates() bool { return !*f.governanceUnitPutExistingOnly }

// hostPortGrantsMatch is api.Config.EgressHostPortGrantsMatch: true only with
// WARDYN_EGRESS_HOST_PORT_GRANTS_INERT set off.
func (f *bootFlags) hostPortGrantsMatch() bool { return !*f.egressHostPortGrantsInert }
