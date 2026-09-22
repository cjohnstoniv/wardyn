/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Azure DevOps connected panel (#386, §2.2/§6 2b) — one of its TWO homes:
// Getting started renders the chip+cause+button inline in "What's set up for
// you" (member-getting-started.tsx); this is the Settings card, "a member
// checking months later what their runs can do looks in Settings, not in an
// onboarding page" (Q9). Both read scmAccessChip/scmAccessCause off the same
// SCMAccess answer, so the two homes cannot disagree about one connection.
//
// Absent entirely when no Azure DevOps row is configured (status.scm_access
// is absent or state "") — the same absent-row doctrine every other card
// here follows. The card shell (a bordered <section>, title + lede) matches
// connection-cards.tsx's own un-exported Card, rather than importing a
// shadcn Card this codebase does not have.
import { Button } from "../../ui/button";
import { ADO } from "../../../lib/ado-entra-copy";
import { PROVIDERS } from "../../../lib/workspace-providers-copy";
import { scmAccessCause, scmAccessChip } from "../../../lib/scm-access-display";
import { useAdoConnect } from "../../../lib/hooks/use-ado-connect";
import type { SetupStatus } from "../../../lib/types";

export function AdoConnectionCard({ status, onChanged }: { status?: SetupStatus; onChanged: () => void }) {
  const { connecting, connect, blockedUrl } = useAdoConnect();
  const access = status?.scm_access;
  const handleConnect = async () => {
    if (await connect()) onChanged();
  };
  if (!access || access.state === "") return null;
  const chip = scmAccessChip(access.state, access.source);

  return (
    <section className="rounded-xl border border-border bg-card p-4">
      <h3 className="text-sm font-medium text-foreground">{PROVIDERS.KIND_AZURE_DEVOPS}</h3>
      <div className="mt-3 space-y-2 text-body">
        {chip && <p className={chip.tone === "success" ? "text-success" : "text-warning"}>{chip.label}</p>}
        {/* review finding F3: `live` branches on `source` — a SHARED row's
            `live` (no source) makes no per-person claim (§7.5's
            ACCESS_SHARED_NOTE) and must never render "Your Wardyn sign-in" /
            "Renewed while you keep using it", which are per-person facts. */}
        {access.state === "live" && access.source && (
          <>
            {access.org && (
              <p className="text-muted-foreground">
                {ADO.PANEL_ORG}: <span className="font-mono">{access.org}</span>
              </p>
            )}
            <p className="text-muted-foreground">
              {ADO.PANEL_HOW}: {access.source === "separate" ? ADO.PANEL_HOW_SEPARATE : ADO.PANEL_HOW_ORG}
            </p>
            <p className="text-muted-foreground">
              {ADO.PANEL_ENDS}: {ADO.PANEL_ENDS_RENEWED}
            </p>
            <p className="text-muted-foreground">{ADO.PANEL_ENDS_HINT}</p>
          </>
        )}
        {access.state === "live" && !access.source && <p className="text-muted-foreground">{ADO.ACCESS_SHARED_NOTE}</p>}
        {access.state === "not_configured" && (
          <>
            <p className="text-warning">{scmAccessCause(access.cause)}</p>
            <Button size="sm" variant="outline" disabled={connecting} onClick={() => void handleConnect()}>
              {ADO.CONNECT_ADO}
            </Button>
            {blockedUrl && (
              <p className="text-xs text-muted-foreground">
                Your browser blocked the popup.{" "}
                <a href={blockedUrl} target="_blank" rel="noopener noreferrer" className="font-medium text-info hover:underline">
                  {ADO.CONNECT_ADO}
                </a>
              </p>
            )}
          </>
        )}
        {access.state === "shared_expired" && <p className="text-warning">{ADO.ACCESS_SHARED_EXPIRED_ACTION}</p>}
      </div>
    </section>
  );
}
