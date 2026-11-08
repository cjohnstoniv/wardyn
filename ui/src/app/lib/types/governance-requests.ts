/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Held requests (internal/api/governance_requests.go): GET and POST /governance/requests. A person asks
// for something only a unit's deciders may give (an elevation: membership of the unit for a while), and the
// request waits as a held change until a decider approves it.

import type { GovernanceChange, GovernanceChangeApprover } from "./governance-change";

export type GovernanceRequestKind = "elevation" | "image";

// GovernanceRequestView: the held change with the kind's fields and who decides it.
export interface GovernanceRequest extends GovernanceChange {
  kind: GovernanceRequestKind;
  unit_id?: string;
  unit_name?: string;
  // As max_elevation is stored: "4h", "1h30m".
  duration?: string;
  // An applied elevation's end.
  ends_at?: string;
  image?: string;
  mine: boolean;
  decidable: boolean;
  deciders: GovernanceChangeApprover[];
}

// One unit the caller may ask an elevation in.
export interface ElevationOffer {
  unit_id: string;
  unit_name: string;
  max_elevation: string;
  ticket_required?: boolean;
}

// GET /governance/requests' body.
export interface GovernanceRequestList {
  requests: GovernanceRequest[];
  offered: ElevationOffer[];
}

// POST /governance/requests' body.
export interface GovernanceRequestBody {
  kind: GovernanceRequestKind;
  unit_id?: string;
  duration?: string;
  reason: string;
  ticket?: string;
  image?: string;
}
