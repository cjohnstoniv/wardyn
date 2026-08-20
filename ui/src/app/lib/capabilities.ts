/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The member half of permissioning (0.6 pillar 2): the caller's own effective
// capability set, and the two questions the why-denied surfaces ask of it.
//
// This is ADVISORY ONLY. The enforcement points are server-side —
// authorizeMemberDecision, resolveRunPolicy's member branch, denyMemberRequest
// and handleListSecrets — and every one of them re-resolves the grants itself.
// What lives here exists so a member is told WHY before they click, instead of
// discovering it as a bare 403.
//
// The matcher below mirrors internal/api/capabilities.go's capValueMatches
// exactly, including the host semantics it borrows from entryCoversAny. Two
// matchers that disagree is how a refusal becomes unexplainable, so any change
// to one belongs in the other in the same commit.
import * as React from "react";
import { permissions } from "./api/permissions";
import type { CapabilityKind } from "./permissions-copy";
import type { CapabilityGrant, MeCapabilities } from "./types";

// egressEntryHost (internal/api/runs_dispatch_gitbroker.go): lowercase, drop a
// ":port" suffix, drop a trailing dot.
function egressEntryHost(entry: string): string {
  let h = entry.trim().toLowerCase();
  const colon = h.indexOf(":");
  if (colon >= 0) h = h.slice(0, colon);
  return h.endsWith(".") ? h.slice(0, -1) : h;
}

// capValueMatches: "*" is the per-kind wildcard; egress_host additionally
// honours a "*.suffix" pattern (note that "*.github.com" does NOT cover bare
// "github.com" — the suffix carries the dot, exactly as entryCoversAny does).
// Every other kind is an exact compare: a secret name, a workspace uuid and an
// image ref are identifiers where a near-miss must not match.
export function capabilityValueMatches(kind: string, grantValue: string, want: string): boolean {
  const g = grantValue.trim();
  if (g === "*") return true;
  if (kind === "egress_host") {
    const h = egressEntryHost(g);
    if (h.startsWith("*")) return egressEntryHost(want).endsWith(h.slice(1));
    return egressEntryHost(want) === h;
  }
  return g === want.trim();
}

function scan(grants: CapabilityGrant[], kind: string, value: string): { deny: boolean; allow: boolean } {
  let allow = false;
  for (const g of grants) {
    if (g.capability !== kind || !capabilityValueMatches(kind, g.value, value)) continue;
    if (g.effect === "deny") return { deny: true, allow: false }; // deny is final
    allow = true;
  }
  return { deny: false, allow };
}

// The NARROWING answer (capAllowed): deny beats allow beats the enforcement
// switch. `caps === null` means the question doesn't apply to this caller — an
// admin (exempt) or a set that hasn't loaded — and answers ALLOWED, the same
// fail-open default useOperator carries: an advisory check must never be the
// thing that disables a control the server would have honoured.
export function capabilityAllowed(caps: MeCapabilities | null, kind: CapabilityKind, value: string): boolean {
  if (!caps) return true;
  const { deny, allow } = scan(caps.grants, kind, value);
  if (deny) return false;
  if (allow) return true;
  return !caps.enforcement[kind];
}

// useMyCapabilities — GET /me/capabilities, once, for the surfaces that explain
// a refusal. `enabled` is the caller's "this question applies to me" gate (in
// practice `!operator`): an admin is exempt server-side, so asking would only
// produce an answer no seam will ever act on.
//
// A failed fetch stays null, which reads as "not bounded" everywhere above —
// deliberate: a network blip must not annotate a member's whole console with
// refusals that aren't happening.
export function useMyCapabilities(enabled: boolean): MeCapabilities | null {
  const [caps, setCaps] = React.useState<MeCapabilities | null>(null);
  React.useEffect(() => {
    if (!enabled) {
      setCaps(null);
      return;
    }
    let live = true;
    permissions
      .getMyCapabilities()
      .then((c) => live && setCaps(c))
      .catch(() => {
        /* leave null: advisory copy stays silent rather than guessing */
      });
    return () => {
      live = false;
    };
  }, [enabled]);
  return caps;
}

// Whether ANY kind is being enforced against this caller. The stale-group hint
// hangs off this: a snapshot that predates group recording only costs a member
// something once something is actually being refused.
export function anyCapabilityEnforced(caps: MeCapabilities | null): boolean {
  return !!caps && Object.values(caps.enforcement).some(Boolean);
}
