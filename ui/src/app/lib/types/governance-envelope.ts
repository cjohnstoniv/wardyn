/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// types.GovernanceEnvelope — a platform's root unit and the IdP groups the security tier approved for it.
// GET /governance lists them as `envelopes`. An envelope changes only who must approve a write below the
// root; it enforces nothing at run time.
export interface GovernanceEnvelope {
  unit_id: string;
  approved_groups: string[];
  // set_by: the setter's principal for the security tier; "security" for a unit admin.
  set_by: string;
  set_at: string;
}
