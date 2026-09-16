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
// It now renders the SAME card as /settings (connection-cards.tsx), which is
// the point: one component in both places can't drift, and the embed is
// exactly how the old surface leaked back into the funnel in the first place.
//
// GitHostCard retired with 0.7.2's Workspace Providers round: the git
// credential lanes moved INTO a provider row (`/providers`'s Git tab), and the
// funnel's own `providers` step (steps.ts, phase "Your work", before
// `workspaces`) is where that now lives — this step keeps ModelProviderCard
// only, and its own id/label stay `integrations`/"Secrets" (Q5: renaming the
// id breaks demo-videos.ts's episodesFor).
//
// Still optional. Demos, governed commands and terminal recordings need nothing
// connected; clicking Next past this step with nothing set marks it Skipped
// (setup-screen.tsx's selectStep).
import { Link } from "react-router-dom";
import type { SetupStatus, SiteConfig } from "../../../lib/types";
import { ModelProviderCard } from "../settings/connection-cards";

// X3-F8: the lede NAMED "the Secrets page" with no way to get there — smaller
// than a rail/label/visited-set change (renaming this step is out: its id
// must stay `integrations`, demo-videos.ts's episodesFor keys on it). Split
// around the link so the sentence stays otherwise byte-identical.
export const STEP_LEDE_PREFIX =
  "Optional. Wardyn runs governed commands, interactive runs and recordings with nothing connected. Most agent runs want a model credential — any other secret a run needs (including a git credential, set up under Providers) is added the same way, on the ";
export const STEP_LEDE_LINK = "Secrets page";
export const STEP_LEDE_SUFFIX = ", and handed to runs by name.";
// Preserved for any external byte-parity check against the old single string.
export const STEP_LEDE = `${STEP_LEDE_PREFIX}${STEP_LEDE_LINK}${STEP_LEDE_SUFFIX}`;

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
      <p className="text-sm leading-relaxed text-muted-foreground">
        {STEP_LEDE_PREFIX}
        <Link to="/secrets" className="font-medium text-primary hover:underline">
          {STEP_LEDE_LINK}
        </Link>
        {STEP_LEDE_SUFFIX}
      </p>
      <ModelProviderCard status={status} siteConfig={siteConfig} onChanged={onRecheck} />
    </div>
  );
}
