/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Azure DevOps access chip's tone/label + cause line, off SCMAccess
// (internal/api.SCMAccess, #386) — shared between the two homes the
// connected panel lives in (Getting started, Settings), so they cannot
// render the state differently. Pure: no React, no fetch.
import { ADO } from "./ado-entra-copy";
import { ADO_PAT, gettingStartedOwnChip } from "./ado-pat-copy";
import { adoOrgLabel, daysLeft, formatDay } from "./ado-pat-display";
import type { SCMAccess } from "./types";

// A person's own token (token_mode own_pat) has four states beyond "not added",
// graded the way ado-pat-display.ts's Settings card grades them: expired is
// expired_signin and wins; refused_at on a live or expiring row is Refused and
// wins over both; the wire's expiring is Expiring; live is Connected.
function ownTokenKind(access: SCMAccess): "connected" | "expiring" | "refused" | "expired" | null {
  if (access.token_mode !== "own_pat") return null;
  if (access.state === "expired_signin") return "expired";
  if (access.state !== "live" && access.state !== "expiring") return null;
  if (access.refused_at && access.expires_on) return "refused";
  return access.state === "expiring" ? "expiring" : "connected";
}

// scmAccessChip follows modelAccessChip's own "say nothing rather than
// invent" rule (member-getting-started.tsx): a state this console does not
// recognise renders nothing. `not_applicable` (an admin-token or local-mode
// caller, scmaccess.go's `isMechanism := subject == ""`) IS reachable
// from a browser session (#458) — §7.5 simply freezes no chip for it, since
// Q458-1 draws it as one plain line, not a chip; ado-connection.tsx (its
// only mount) renders ADO.NOT_APPLICABLE_BODY for that state directly.
export function scmAccessChip(
  state: string,
  source?: string,
  cause?: string,
  // The whole answer, for a row where the person adds their own token.
  access?: SCMAccess,
  now: number = Date.now(),
): { label: string; tone: "success" | "warning" | "danger" } | null {
  const own = access ? ownTokenKind(access) : null;
  if (access && own) {
    const days = own === "expiring" && access.expires_on ? daysLeft(access.expires_on, now) : 0;
    const tone = { connected: "success", expiring: "warning", refused: "danger", expired: "danger" } as const;
    return { label: gettingStartedOwnChip(own, days), tone: tone[own] };
  }
  switch (state) {
    case "live":
      if (source === "org") return { label: ADO.ACCESS_LIVE_ORG, tone: "success" };
      if (source === "separate") return { label: ADO.ACCESS_LIVE_SEPARATE, tone: "success" };
      // No source: a shared row's `live` — §7.5's ACCESS_SHARED_NOTE, "makes
      // no per-person claim".
      return { label: ADO.ACCESS_SHARED_LIVE, tone: "success" };
    case "not_configured":
      return { label: ADO.ACCESS_NOT_CONNECTED, tone: "warning" };
    // A stored sign-in that has ended, or that no longer covers what a run on
    // the row needs (scmaccess.go's two expired_signin causes).
    case "expired_signin":
      return { label: cause === "consent_needed" ? ADO.ACCESS_NEEDS_CONSENT : ADO.ACCESS_EXPIRED, tone: "warning" };
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
  switch (cause) {
    case "row_is_newer":
      return ADO.CAUSE_ROW_IS_NEWER;
    case "ended":
      return ADO.CAUSE_ENDED;
    case "consent_needed":
      return ADO.CAUSE_CONSENT_NEEDED;
    // The per-person token rows' causes (#1428, #1430), in the mock's words.
    case "permissions_missing":
      return ADO_PAT.SIGN_IN_AGAIN_BODY;
    case "blocked":
      return ADO_PAT.BLOCKED_BODY;
    case "ado_pat_needs_console_app":
      return ADO_PAT.MEMBER_NEEDS_ADMIN;
    case "token_expired":
      return ADO_PAT.OWN_EXPIRED_BODY;
    default:
      return "";
  }
}

// scmAccessNeedsConnect is whether the state is one a (re)connect fixes — the
// states the cause line and CONNECT_ADO render for. Three causes are not the
// person's to fix by signing in: the organisation blocks token creation, the
// app has no client secret (both the admin's), and an own token that has
// expired is replaced, not reconnected.
export function scmAccessNeedsConnect(state?: string, cause?: string): boolean {
  if (cause === "blocked" || cause === "ado_pat_needs_console_app" || cause === "token_expired") return false;
  return state === "not_configured" || state === "expired_signin";
}

// scmOwnTokenAction is the line and the one button an own-token row offers on
// Getting started, in Settings' words: Add for a token not added or expired,
// Replace for one expiring or refused, and nothing for a live one. `line` is ""
// for a token not added. null for every other row, which keeps scmAccessCause
// and scmAccessNeedsConnect.
export function scmOwnTokenAction(access: SCMAccess): { line: string; button: "add" | "replace" } | null {
  if (access.token_mode !== "own_pat") return null;
  if (access.state === "not_configured") return { line: "", button: "add" };
  const org = adoOrgLabel(access.org ?? "");
  switch (ownTokenKind(access)) {
    case "expired":
      return { line: ADO_PAT.OWN_EXPIRED_BODY, button: "add" };
    case "refused":
      return { line: ADO_PAT.OWN_REFUSED_LINE(formatDay(access.refused_at!), formatDay(access.expires_on!)), button: "replace" };
    case "expiring":
      return { line: access.expires_on ? ADO_PAT.OWN_EXPIRING_LINE(org, formatDay(access.expires_on)) : "", button: "replace" };
    default:
      return null;
  }
}
