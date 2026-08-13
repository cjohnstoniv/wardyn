/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Integrations step (13 -> 9 -> 10 collapse) — a THIN EMBED of the real
// /integrations page: IntegrationsScreen's `embedded` mode drops its own
// PageHeader; everything else (category sections with their full row actions,
// the empty state, the footnote, and the Add/rotate/delete dialogs) renders
// exactly as it does on the full page.
// This step replaces the old provider/host_proxy/scm_provider/artifact_repo
// steps — their configuration now lives on /integrations, so Getting Started
// links to it instead of forking a second copy of that configuration surface.
//
// Host proxy / egress redirection do NOT appear here any more — Corporate
// network (steps.ts, right before this step) owns both now, and is the actual
// fix for the old "blocked network reads as bad credential" problem (ORDER,
// not a banner). They aren't hidden from this embed, they're gone from the
// Integrations page itself, so there's no hideCategories to pass; the embed's
// only extra is EMBED_SCOPE_NOTE saying where they went — rendered in BOTH the
// empty and connected states, since a first visit is exactly when someone
// wonders where those two categories are (the mock only showed the note once
// connected). The full page renders the forward-pointing half of the same
// pointer itself (T.NETWORK_SCOPE_NOTE).
//
// No footer of its own: "Manage in Integrations" duplicated the embed (this
// IS that page), and "Skip this step" duplicated Next — the step is optional,
// so clicking Next past it with nothing connected marks it Skipped
// (setup-screen.tsx's selectStep). One forward affordance, not three.
import { T } from "../../../lib/integrations";
import { IntegrationsScreen } from "../integrations/integrations-screen";

export function IntegrationsStep({
  onRecheck,
}: {
  /** Re-fetch the orchestrator's own status/siteConfig/secrets so the rail
   *  badge doesn't go stale right after an in-embed add/rotate/delete. */
  onRecheck: () => void;
}) {
  return (
    <div className="space-y-5">
      <p className="text-sm leading-relaxed text-muted-foreground">{T.STEP_LEDE}</p>

      <IntegrationsScreen embedded onChanged={onRecheck} />
      <p className="text-[0.6875rem] leading-snug text-muted-foreground">{T.EMBED_SCOPE_NOTE}</p>
    </div>
  );
}
