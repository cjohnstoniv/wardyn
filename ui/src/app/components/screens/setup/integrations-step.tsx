/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Integrations step (13 -> 9 -> 10 collapse) — a THIN EMBED of the real
// /integrations page: IntegrationsScreen's `embedded` mode drops its own
// PageHeader and the Integrations/Tools tab strip; everything else (category
// sections with their full row actions, the empty state, the footnote, and
// the Add/rotate/delete dialogs) renders exactly as it does on the full page.
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
//
// The one CTA it does carry: once a model provider resolves (llmReady), a
// success-toned banner invites the operator to prove it live on the new
// "agent in the box" demo step (steps.ts's 12 -> 13) — absent until then,
// never shown once already visited/launched (that's the demo step's own badge
// to carry, not a second copy of it here).
import { ShieldCheck } from "lucide-react";
import { Button } from "../../ui/button";
import { T } from "../../../lib/integrations";
import { IntegrationsScreen } from "../integrations/integrations-screen";

export function IntegrationsStep({
  onRecheck,
  llmReady,
  onTryDemo,
}: {
  /** Re-fetch the orchestrator's own status/siteConfig/secrets so the rail
   *  badge doesn't go stale right after an in-embed add/rotate/delete. */
  onRecheck: () => void;
  /** Whether an agent-capable AI integration resolves — the SAME signal the
   *  "agent in the box" step gates on (onboarding/intro.tsx's deriveReadiness). */
  llmReady: boolean;
  /** Navigates to the "agent in the box" step. */
  onTryDemo: () => void;
}) {
  return (
    <div className="space-y-5">
      <p className="text-sm leading-relaxed text-muted-foreground">{T.STEP_LEDE}</p>

      {llmReady && (
        <div
          className="flex items-start gap-2.5 rounded-lg border border-success/30 bg-success-subtle px-3 py-2.5"
          data-testid="integrations-prove-it-cta"
        >
          <ShieldCheck className="mt-0.5 size-4 shrink-0 text-success" />
          <p className="min-w-0 flex-1 text-[0.8125rem] leading-snug text-success">{T.PROVE_IT_BANNER}</p>
          <Button size="sm" variant="outline" className="shrink-0" onClick={onTryDemo}>
            {T.TRY_AGENT_BOX}
          </Button>
        </div>
      )}

      <IntegrationsScreen embedded onChanged={onRecheck} />
      <p className="text-[0.6875rem] leading-snug text-muted-foreground">{T.EMBED_SCOPE_NOTE}</p>
    </div>
  );
}
