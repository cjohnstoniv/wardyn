/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #162 — the console never said when network confinement was unenforced.
// /healthz already carried the verdict (`network_policy`), and app-shell.tsx
// already fetched it, but nothing rendered it: ConfinementChip took its
// posture from nowhere and the shell showed no banner. An install whose
// ambient default-deny is merely acknowledged, or not enforced at all, looked
// identical to an enforced one.
//
// THE PREMISE CORRECTION (mock-approval comment on #162): reading posture off
// `h.network_policy ?? ""` alone cannot work. internal/api/healthz.go omits
// `network_policy` in TWO situations that need OPPOSITE treatment — on Docker
// (genuinely not applicable, stay silent) and on a Kubernetes daemon whose
// Capabilities() call itself errored (could not confirm, must warn). One
// empty string cannot tell them apart, so the resolver also takes `runner`
// (already on the /healthz wire, lib/api/health.ts's `runner?: string`) —
// see lib/confinement-posture.ts's resolveConfinementPosture for the five
// cases the mock's approval ruled on. THAT resolver lives in lib/, not here:
// this module is lazy-loaded (below), and app-shell.tsx — which is in the
// EAGER entry chunk — calls the resolver on every render; importing it FROM
// here would statically pull this file's icon + react-router-dom import back
// into the entry graph and silently collapse the split (bundle-split.test.ts).
import { AlertTriangle } from "lucide-react";
import { useNavigate } from "react-router-dom";

import type { ConfinementPosture } from "./operator-context";
import { useConfinementPosture, useOperator } from "./operator-context";
import {
  POSTURE_ACKNOWLEDGED_BANNER,
  POSTURE_UNENFORCED_BANNER,
  POSTURE_UNKNOWN_BANNER,
} from "./confinement-posture-copy";

const BANNER_BY_POSTURE: Partial<
  Record<ConfinementPosture, { tone: "warning" | "danger"; title: string; body: string; action: string }>
> = {
  acknowledged: {
    tone: "warning",
    title: POSTURE_ACKNOWLEDGED_BANNER.TITLE,
    body: POSTURE_ACKNOWLEDGED_BANNER.BODY,
    action: POSTURE_ACKNOWLEDGED_BANNER.ACTION,
  },
  unenforced: {
    tone: "danger",
    title: POSTURE_UNENFORCED_BANNER.TITLE,
    body: POSTURE_UNENFORCED_BANNER.BODY,
    action: POSTURE_UNENFORCED_BANNER.ACTION,
  },
  unknown: {
    tone: "warning",
    title: POSTURE_UNKNOWN_BANNER.TITLE,
    body: POSTURE_UNKNOWN_BANNER.BODY,
    action: POSTURE_UNKNOWN_BANNER.ACTION,
  },
};

// Mounted by app-shell.tsx LAST in the banner stack, after ModelAccessBanner
// (ruling 3 on the mock approval): a dead control plane, an unknown identity
// and a dying session are each the better explanation of what you are looking
// at, and model access blocks the very thing a run needs to start — this one
// is the quietest of the five, has no per-person urgency, and is read last.
//
// Renders nothing for "enforced" or "" (no live posture to report) — same
// "renders nothing when there is nothing to say" shape as ModelAccessBanner,
// and the same reason it needs no `role="status"` of its own: app-shell.tsx
// mounts that region eagerly around this component (and ModelAccessBanner
// beside it), so a state that arrives later is a text change inside a region
// that was already there.
export function ConfinementPostureBanner() {
  const posture = useConfinementPosture();
  const operator = useOperator();
  const navigate = useNavigate();
  const spec = BANNER_BY_POSTURE[posture];
  if (!spec) return null;
  return (
    <div
      className={
        "relative z-50 flex shrink-0 flex-wrap items-start gap-2 border-b px-4 py-2 text-sm " +
        (spec.tone === "danger"
          ? "border-danger/25 bg-danger-subtle text-danger"
          : "border-border bg-warning-subtle text-warning")
      }
    >
      <AlertTriangle className="mt-0.5 size-4 shrink-0" />
      <div className="min-w-0 flex-1">
        <p className="font-medium text-foreground">{spec.title}</p>
        <p className="mt-0.5 text-xs text-muted-foreground">{spec.body}</p>
      </div>
      {/* #510-F7 — the action lands on /setup?step=environment, which
          App.tsx's SetupRoute renders as MemberGettingStarted for a
          non-operator (the `step` query is ignored there): a member had a
          button whose only destination was a page with no environment step.
          Informational-only for a member; the operator keeps the action. */}
      {operator && (
        <button
          type="button"
          onClick={() => navigate("/setup?step=environment")}
          className="shrink-0 font-medium underline underline-offset-2"
        >
          {spec.action}
        </button>
      )}
    </div>
  );
}
