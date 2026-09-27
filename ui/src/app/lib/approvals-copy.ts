/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Approvals decision toasts (#458) — one canon home for the four outcomes an
// approve/deny click can toast, so the Approvals screen, the run-detail
// Approvals tab and the live strip can't say each one a different way. Not
// ado-entra-copy.ts's ADO namespace (these toasts are not Azure DevOps
// specific — the same four cover every approval kind) and not
// wardyn/copy.ts (that module's own APPROVAL namespace is unrelated banner/
// scope copy — see its own file for what it carries). Pure TS, same
// discipline as ado-entra-copy.ts: the components that consume this add no
// copy of their own.
export const APPROVALS = {
  TOAST_APPROVED: `Request approved`,
  TOAST_DENIED: `Request denied`,
  TOAST_APPROVE_FAILED: `Couldn't approve this request`,
  TOAST_DENY_FAILED: `Couldn't deny this request`,
} as const;
