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
import { useConfinementCheckedAt, useConfinementPosture, useConfinementSource, useOperator } from "./operator-context";
import type { ConsoleView } from "./console-view";
import {
  POSTURE_ACKNOWLEDGED_BANNER,
  POSTURE_NOT_PROVEN_BANNER,
  POSTURE_REFUSED_BANNER,
  POSTURE_RUNNER_ACKNOWLEDGED_BANNER,
  POSTURE_RUNNER_NOT_PROVEN_BANNER,
  POSTURE_RUNNER_REFUSED_BANNER,
  POSTURE_RUNNER_UNENFORCED_BANNER,
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
  // The refused body names its check time, filled in below.
  refused: { tone: "danger", title: POSTURE_REFUSED_BANNER.TITLE, body: "", action: POSTURE_REFUSED_BANNER.ACTION },
  not_proven: {
    tone: "danger",
    title: POSTURE_NOT_PROVEN_BANNER.TITLE,
    body: POSTURE_NOT_PROVEN_BANNER.BODY,
    action: POSTURE_NOT_PROVEN_BANNER.ACTION,
  },
};

// The bands for a connected Kubernetes runner's verdict (network_policy_source
// "runner"): the same tones, worded for that runner rather than this cluster.
const RUNNER_BANNER_BY_POSTURE: typeof BANNER_BY_POSTURE = {
  acknowledged: {
    tone: "warning",
    title: POSTURE_RUNNER_ACKNOWLEDGED_BANNER.TITLE,
    body: POSTURE_RUNNER_ACKNOWLEDGED_BANNER.BODY,
    action: POSTURE_RUNNER_ACKNOWLEDGED_BANNER.ACTION,
  },
  unenforced: {
    tone: "danger",
    title: POSTURE_RUNNER_UNENFORCED_BANNER.TITLE,
    body: POSTURE_RUNNER_UNENFORCED_BANNER.BODY,
    action: POSTURE_RUNNER_UNENFORCED_BANNER.ACTION,
  },
  refused: { tone: "danger", title: POSTURE_RUNNER_REFUSED_BANNER.TITLE, body: "", action: POSTURE_RUNNER_REFUSED_BANNER.ACTION },
  not_proven: {
    tone: "danger",
    title: POSTURE_RUNNER_NOT_PROVEN_BANNER.TITLE,
    body: POSTURE_RUNNER_NOT_PROVEN_BANNER.BODY,
    action: POSTURE_RUNNER_NOT_PROVEN_BANNER.ACTION,
  },
};

// checkTime renders /healthz's network_policy_checked_at (RFC 3339, UTC) as the
// setup row does, "2006-01-02 15:04 UTC"; "" when absent or not that shape.
function checkTime(iso: string | undefined): string {
  return iso && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}/.test(iso) ? `${iso.slice(0, 10)} ${iso.slice(11, 16)} UTC` : "";
}

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
//
// `view` (admin-member-modes-design.md §4.2, M-3): the band is Admin-view
// only — a cluster-wide posture nobody but an admin can act on. The User view
// gets no band; `ConfinementChip` already carries the same posture on the
// person's own runs and the New Run rail. Defaults to "user" (no band) so a
// caller that mounts this directly, with no shell above it, stays silent
// exactly as it would off any unrecognised view.
export function ConfinementPostureBanner({ view = "user" }: { view?: ConsoleView } = {}) {
  const posture = useConfinementPosture();
  const operator = useOperator();
  const navigate = useNavigate();
  // The shell reads the verdict and its check time from the same /healthz answer.
  const refusedAt = checkTime(useConfinementCheckedAt());
  const fromRunner = useConfinementSource() === "runner";
  const found = (fromRunner ? RUNNER_BANNER_BY_POSTURE[posture] : undefined) ?? BANNER_BY_POSTURE[posture];
  if (!found || view !== "admin") return null;
  // Without a check time (an older daemon), the refused band shows its title alone.
  const refusedBody = fromRunner ? POSTURE_RUNNER_REFUSED_BANNER.BODY : POSTURE_REFUSED_BANNER.BODY;
  const spec = posture === "refused" ? { ...found, body: refusedAt ? refusedBody(refusedAt) : "" } : found;
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
        {spec.body && <p className="mt-0.5 text-xs text-muted-foreground">{spec.body}</p>}
      </div>
      {/* #510-F7 — the action opens the Admin view's setup funnel at its
          environment step, which only a super admin may open: a security
          admin in the Admin view would get a button whose only destination
          refuses them. Informational-only there; the operator keeps the
          action. */}
      {operator && (
        <button
          type="button"
          onClick={() => navigate("/admin/setup?step=environment")}
          className="shrink-0 font-medium underline underline-offset-2"
        >
          {spec.action}
        </button>
      )}
    </div>
  );
}
