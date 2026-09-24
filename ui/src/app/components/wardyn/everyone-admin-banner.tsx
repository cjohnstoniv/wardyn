/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #484 — the everyone-is-an-admin warning, in the shell's banner stack above
// every page (Q457-4). The state is the server's, not re-derived here: the
// /setup/status sso_rbac row warns only when neither a role map nor an admin
// list is set (Q457-5). Members never see it twice over — the row is redacted
// out of their /setup/status (redactSetupStatusForMember), and the band also
// renders only for a RESOLVED admin, so the fail-open operator default can
// never paint it for someone who cannot act on it.
//
// Same markup as ConfinementPostureBanner's warning tone, and like it this
// needs no `role="status"` of its own: app-shell.tsx mounts that region
// eagerly around the lazy bands.
import { AlertTriangle } from "lucide-react";
import { useNavigate } from "react-router-dom";

import { ADMIN_ACCESS_BANNER, ADMIN_ACCESS_PEOPLE_STEP } from "../../lib/access-posture-copy";
import type { SetupStatus } from "../../lib/types";
import { useShellSetupStatus } from "./model-access-context";
import { useOperator, useOperatorResolved } from "./operator-context";

export function everyoneIsAdmin(status: SetupStatus | null): boolean {
  return !!status?.checks?.some((c) => c.id === "sso_rbac" && c.status === "warn");
}

export function EveryoneAdminBanner() {
  const { status } = useShellSetupStatus();
  const operator = useOperator();
  const resolved = useOperatorResolved();
  const navigate = useNavigate();
  if (!(resolved && operator) || !everyoneIsAdmin(status)) return null;
  return (
    <div className="relative z-50 flex shrink-0 flex-wrap items-start gap-2 border-b border-border bg-warning-subtle px-4 py-2 text-sm text-warning">
      <AlertTriangle className="mt-0.5 size-4 shrink-0" />
      <div className="min-w-0 flex-1">
        <p className="font-medium text-foreground">{ADMIN_ACCESS_BANNER.TITLE}</p>
        <p className="mt-0.5 text-xs text-muted-foreground">{ADMIN_ACCESS_BANNER.BODY}</p>
      </div>
      <button
        type="button"
        onClick={() => navigate(ADMIN_ACCESS_PEOPLE_STEP)}
        className="shrink-0 font-medium underline underline-offset-2"
      >
        {ADMIN_ACCESS_BANNER.ACTION}
      </button>
    </div>
  );
}
