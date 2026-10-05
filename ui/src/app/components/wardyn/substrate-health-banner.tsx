/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// M10 — the substrate_health band. The state is the server's: the
// /setup/status substrate_health row (internal/api/substrate_health.go) is read
// as it stands, never re-derived from the runner gauges. The row is redacted
// out of a member's /setup/status, and the band also renders only in the Admin
// view for a RESOLVED admin. fail paints danger, warn paints warning; the body
// is the row's detail verbatim.
//
// Same markup as ConfinementPostureBanner, and like it this needs no
// `role="status"` of its own: app-shell.tsx mounts that region eagerly around
// the lazy bands.
import { AlertTriangle } from "lucide-react";
import { useNavigate } from "react-router-dom";

import { SUBSTRATE_BANNER, SUBSTRATE_BANNER_STEP } from "../../lib/substrate-banner-copy";
import type { SetupStatus } from "../../lib/types";
import type { ConsoleView } from "./console-view";
import { useShellSetupStatus } from "./model-access-context";
import { useOperator, useOperatorResolved } from "./operator-context";

const TITLE_BY_CAUSE: Record<string, string> = {
  runner_unreachable: SUBSTRATE_BANNER.TITLE_UNREACHABLE,
  runner_auth: SUBSTRATE_BANNER.TITLE_AUTH,
  sweep_stale: SUBSTRATE_BANNER.TITLE_SWEEP,
};

function substrateRow(status: SetupStatus | null) {
  return status?.checks?.find((c) => c.id === "substrate_health" && (c.status === "fail" || c.status === "warn")) ?? null;
}

export function SubstrateHealthBanner({ view = "user" }: { view?: ConsoleView } = {}) {
  const { status } = useShellSetupStatus();
  const operator = useOperator();
  const resolved = useOperatorResolved();
  const navigate = useNavigate();
  const row = substrateRow(status);
  const title = row?.cause ? TITLE_BY_CAUSE[row.cause] : undefined;
  if (view !== "admin" || !(resolved && operator) || !row || !title) return null;
  return (
    <div
      className={
        "relative z-50 flex shrink-0 flex-wrap items-start gap-2 border-b px-4 py-2 text-sm " +
        (row.status === "fail"
          ? "border-danger/25 bg-danger-subtle text-danger"
          : "border-border bg-warning-subtle text-warning")
      }
    >
      <AlertTriangle className="mt-0.5 size-4 shrink-0" />
      <div className="min-w-0 flex-1">
        <p className="font-medium text-foreground">{title}</p>
        <p className="mt-0.5 text-xs text-muted-foreground">{row.detail}</p>
      </div>
      <button
        type="button"
        onClick={() => navigate(SUBSTRATE_BANNER_STEP)}
        className="shrink-0 font-medium underline underline-offset-2"
      >
        {SUBSTRATE_BANNER.ACTION}
      </button>
    </div>
  );
}
