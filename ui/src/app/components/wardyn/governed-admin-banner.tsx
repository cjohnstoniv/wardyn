/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Constrained-admin mode (WARDYN_GOVERN_ADMIN_RUNS, mock M10): an info band in
// the Admin view's banner stack telling a signed-in admin that their own runs
// are bounded like everyone else's. The state is the server's, read from the
// shell's /setup/status: auth.govern_admin_runs, and auth.mode "sso" for "the
// caller is a signed-in person" (the admin token and local mode are the
// break-glass the band says stays outside). Hide is remembered for the browser
// session, keyed on the viewer's subject because sessionStorage survives a
// sign-out in the same tab.
//
// Same markup as EveryoneAdminBanner's strip in the info tone; app-shell.tsx
// mounts the live region eagerly around the lazy bands.
import * as React from "react";
import { Info } from "lucide-react";

import { GOVERNED_ADMIN_BANNER } from "../../lib/access-posture-copy";
import { ssGet, ssSet } from "../../lib/storage";
import { Button } from "../ui/button";
import type { ConsoleView } from "./console-view";
import { useShellSetupStatus } from "./model-access-context";
import { useOperator, useOperatorResolved, usePrincipal } from "./operator-context";

function hideKey(principal: string): string {
  return `wardyn.governedAdminHidden.${principal}`;
}

export function GovernedAdminBanner({ view = "user" }: { view?: ConsoleView } = {}) {
  const { status } = useShellSetupStatus();
  const operator = useOperator();
  const resolved = useOperatorResolved();
  const principal = usePrincipal();
  const [hiddenNow, setHiddenNow] = React.useState(false);
  const auth = status?.auth;
  if (view !== "admin" || !(resolved && operator) || !auth?.govern_admin_runs || auth.mode !== "sso") return null;
  // "" (/me unresolved or failed) never reads or writes the flag.
  if (hiddenNow || (principal && ssGet(hideKey(principal)) === "1")) return null;
  const exempt = auth.govern_admin_runs_exempt?.includes("recording") ?? false;
  return (
    <div className="relative z-50 flex shrink-0 flex-wrap items-start gap-2 border-b border-border bg-info-subtle px-4 py-2 text-sm text-info">
      <Info className="mt-0.5 size-4 shrink-0" />
      <div className="min-w-0 flex-1">
        <p className="font-medium text-foreground">{GOVERNED_ADMIN_BANNER.TITLE}</p>
        <p className="mt-0.5 text-xs text-muted-foreground">
          {exempt ? GOVERNED_ADMIN_BANNER.BODY_RECORDING_EXEMPT : GOVERNED_ADMIN_BANNER.BODY}
        </p>
      </div>
      <Button
        type="button"
        variant="ghost"
        size="sm"
        onClick={() => {
          if (principal) ssSet(hideKey(principal), "1");
          setHiddenNow(true);
        }}
      >
        {GOVERNED_ADMIN_BANNER.HIDE}
      </Button>
    </div>
  );
}
