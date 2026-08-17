/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Secrets" — the funnel's one credentials step.
//
// This used to be a thin embed of the ENTIRE /integrations page: a catalog of
// seven closed integration kinds plus a generic escape hatch, an Add dialog, a
// probe framework and an adopt/derive lifecycle, all rendered during first-run
// setup. It asked an abstract question ("which integration kind?") when the
// operator has two concrete ones: what runs my agent, and how do you clone my
// private repos. Its own empty state deferred both — "add an integration when a
// run or a Wardyn feature needs one".
//
// It now renders the SAME two cards as /settings (connection-cards.tsx), which
// is the point: one component in both places can't drift, and the embed is
// exactly how the old surface leaked back into the funnel in the first place.
//
// Still optional. Demos, governed commands and terminal recordings need nothing
// connected; clicking Next past this step with nothing set marks it Skipped
// (setup-screen.tsx's selectStep).
import type { SetupStatus, SiteConfig } from "../../../lib/types";
import { GitHostCard, ModelProviderCard } from "../settings/connection-cards";

export const STEP_LEDE =
  "Optional. Wardyn runs governed commands, interactive runs and recordings with nothing connected. The two secrets most runs want are a model credential (for an agent to do the work) and a git credential (for your private code) — any other secret a run needs is added the same way, on the Secrets page, and handed to runs by name.";

export function IntegrationsStep({
  status,
  siteConfig,
  onRecheck,
}: {
  status: SetupStatus;
  siteConfig: SiteConfig | null;
  /** Re-fetch the orchestrator's own status/siteConfig/secrets so the rail
   *  badge doesn't go stale right after a connect or disconnect. */
  onRecheck: () => void;
}) {
  return (
    <div className="space-y-4">
      <p className="text-sm leading-relaxed text-muted-foreground">{STEP_LEDE}</p>
      <ModelProviderCard status={status} siteConfig={siteConfig} onChanged={onRecheck} />
      <GitHostCard status={status} siteConfig={siteConfig} onChanged={onRecheck} />
    </div>
  );
}
