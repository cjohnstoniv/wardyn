/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Azure DevOps access chip's tone/label + cause line, off SCMAccess
// (internal/api.SCMAccess, #386) — shared between the two homes the
// connected panel lives in (Getting started, Settings), so they cannot
// render the state differently. Pure: no React, no fetch.
import { ADO } from "./ado-entra-copy";

// scmAccessChip follows modelAccessChip's own "say nothing rather than
// invent" rule (member-getting-started.tsx): a state this console does not
// recognise, or `not_applicable` (unreachable from a browser session —
// §7.5 freezes no chip for it), renders nothing.
export function scmAccessChip(state: string, source?: string): { label: string; tone: "success" | "warning" } | null {
  switch (state) {
    case "live":
      if (source === "org") return { label: ADO.ACCESS_LIVE_ORG, tone: "success" };
      if (source === "separate") return { label: ADO.ACCESS_LIVE_SEPARATE, tone: "success" };
      // No source: a shared row's `live` — §7.5's ACCESS_SHARED_NOTE, "makes
      // no per-person claim".
      return { label: ADO.ACCESS_SHARED_LIVE, tone: "success" };
    case "not_configured":
      return { label: ADO.ACCESS_NOT_CONNECTED, tone: "warning" };
    case "shared_expired":
      return { label: ADO.ACCESS_SHARED_EXPIRED, tone: "warning" };
    default:
      return null;
  }
}

// scmAccessCause maps scmaccess.go's one derivable cause onto its frozen
// line. A cause this console does not recognise renders "" rather than a
// made-up sentence — the same rule the chip follows.
export function scmAccessCause(cause?: string): string {
  return cause === "row_is_newer" ? ADO.CAUSE_ROW_IS_NEWER : "";
}
