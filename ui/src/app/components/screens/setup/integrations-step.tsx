/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Integrations step (13 -> 9 collapse) — a THIN EMBED of the real /integrations
// page: IntegrationsScreen's `embedded` mode drops its own PageHeader and the
// Integrations/Tools tab strip; everything else (the proxy banner, category
// sections with their full row actions, the empty state, the footnote, and the
// Add/rotate/delete dialogs) renders exactly as it does on the full page. This
// step replaces the old provider/host_proxy/scm_provider/artifact_repo steps —
// all four categories now live on one page, so Getting Started links to it
// instead of forking a second copy of that configuration surface.
//
// The proxy-ordering fact that used to be encoded in step ORDER (host_proxy
// before provider — "you cannot reach Anthropic through an unconfigured
// corporate proxy") now lives in the embedded list itself: T.PROXY_BANNER
// renders here exactly as it does on /integrations, driven by the same
// proxyBannerNeeded() check (see integrations-screen.tsx).
import { ArrowUpRight } from "lucide-react";
import { Link } from "react-router-dom";
import { Button } from "../../ui/button";
import { T } from "../../../lib/integrations";
import { IntegrationsScreen } from "../integrations/integrations-screen";

export function IntegrationsStep({
  count,
  skipped,
  onSkip,
  onRecheck,
}: {
  /** Count of connected integrations (any category) — the same number the
   *  rail's "Ready · N connected" badge shows (see steps.ts stepBadges). */
  count: number;
  /** The operator already clicked "Skip this step" once (per-browser —
   *  setup-gate's integrationsSkipped). Hides the control once true, matching
   *  the old model-skip step's behavior — no reason to offer skipping a step
   *  that's already been decided. */
  skipped: boolean;
  onSkip: () => void;
  /** Re-fetch the orchestrator's own status/siteConfig/secrets so the rail
   *  badge doesn't go stale right after an in-embed add/rotate/delete. */
  onRecheck: () => void;
}) {
  return (
    <div className="space-y-5">
      <p className="text-sm leading-relaxed text-muted-foreground">{T.STEP_LEDE}</p>

      <IntegrationsScreen embedded onChanged={onRecheck} />

      <div className="flex items-center gap-3 border-t border-border pt-4">
        <Link
          to="/integrations"
          className="inline-flex items-center gap-1 text-[0.8125rem] font-medium text-primary hover:underline"
        >
          Manage in Integrations
          <ArrowUpRight className="size-3.5" aria-hidden />
        </Link>
        <span className="flex-1" />
        {/* Once something's connected, "skip" no longer makes sense — this step
            already reads Ready, not Optional. */}
        {count === 0 && !skipped && (
          <Button size="sm" variant="ghost" onClick={onSkip}>
            Skip this step
          </Button>
        )}
      </div>
    </div>
  );
}
