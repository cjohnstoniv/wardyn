/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Secrets" — the funnel's one credentials step.
//
// The operator has two concrete questions here: what runs my agent, and how
// do you clone my private repos — not an abstract "which integration kind?".
//
// Git credentials live in a provider row (`/providers`'s Git tab) via the
// funnel's own `providers` step (steps.ts, phase "Your work", before
// `workspaces`), not here — this step is its lede only, and its
// own id/label stay `integrations`/"Secrets" (renaming the id breaks
// demo-videos.ts's episodesFor; Q5).
//
// Still optional. Demos, governed commands and terminal recordings need nothing
// connected; clicking Next past this step with nothing set marks it Skipped
// (setup-screen.tsx's selectStep).
import { Link } from "react-router-dom";

// The lede names "the Secrets page" and must actually link there (renaming
// this step is out: its id must stay `integrations`, demo-videos.ts's
// episodesFor keys on it). Split around the link so the sentence stays
// otherwise byte-identical.
export const STEP_LEDE_PREFIX =
  "Optional. Wardyn runs governed commands, interactive runs and recordings with nothing connected. Most agent runs want a model credential — any other secret a run needs (including a git credential, set up under Providers) is added the same way, on the ";
export const STEP_LEDE_LINK = "Secrets page";
export const STEP_LEDE_SUFFIX = ", and handed to runs by name.";
// Preserved for any external byte-parity check against the old single string.
export const STEP_LEDE = `${STEP_LEDE_PREFIX}${STEP_LEDE_LINK}${STEP_LEDE_SUFFIX}`;

export function IntegrationsStep() {
  return (
    <div className="space-y-4">
      <p className="text-sm leading-relaxed text-muted-foreground">
        {STEP_LEDE_PREFIX}
        <Link to="/secrets" className="font-medium text-primary hover:underline">
          {STEP_LEDE_LINK}
        </Link>
        {STEP_LEDE_SUFFIX}
      </p>
    </div>
  );
}
