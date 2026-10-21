/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import {
  canDecideAdoCapability,
  canDecideApproval,
  isAdoCapabilityRequest,
  type ApprovalRequest,
} from "../../../lib/types";

/**
 * True when the run waits on a decision this viewer cannot make (an admin-only credential or tool call).
 * The Runs list says "Waiting for an admin" for the same case, so the run page's header and Overview bar
 * use this one predicate rather than telling the viewer it is their turn.
 *
 * A re-auth row is excluded from the predicate: no admin decides it, and the person who can fix it is the
 * credential's own owner. An Azure DevOps escalation the viewer owns is excluded too, because
 * canDecideApproval does not know the ownership carve-out (canDecideAdoCapability does).
 */
export function viewerBlockedOnDecision(pending: ApprovalRequest[], securityOperator: boolean, isRunOwner: boolean): boolean {
  return (
    pending.length > 0 &&
    !securityOperator &&
    pending.some((p) => p.kind !== "credential_reauth") &&
    !pending.some(
      (p) =>
        p.kind !== "credential_reauth" &&
        (canDecideApproval(false, p.kind) || (isAdoCapabilityRequest(p) && canDecideAdoCapability(false, isRunOwner))),
    )
  );
}
