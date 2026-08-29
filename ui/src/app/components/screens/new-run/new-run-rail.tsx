/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The New Run screen's right-hand rail: what this run can actually do, and the
// one button that commits it.
//
// Split out of new-run-screen.tsx by the same seam new-run-primitives.tsx took
// (the file was past the 1000-line gate): this takes props and renders. It owns
// no screen state — every sentence it shows is derived UP in the screen, so the
// rail cannot describe one run while Launch sends another.
//
// It stays a FIXED 320px beside the form and wraps UNDER it below lg, sections
// side by side. Squeezing a 320px rail into a phone column is how the
// consequences of a choice end up unreadable exactly where they are hardest to
// scroll back to.
import { Link } from "react-router-dom";
import { Loader2, TriangleAlert } from "lucide-react";
import type { ConfinementClass, PreflightResult, RunPolicySpec } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Chip, ConfinementChip, RiskBadge } from "../../wardyn/primitives";
import { CC_META } from "../../wardyn/cc-meta";
import { RailSection } from "./new-run-primitives";

export interface RunRailProps {
  /** The stored policy this run launches by reference, when there is one. */
  savedPolicy?: { name: string; spec: RunPolicySpec };
  /** The barrier the run REQUESTS (a separate wire field from the spec floor). */
  cc: ConfinementClass;
  /** An agent run with no model path launches, then fails its first model call. */
  showModelWarning: boolean;
  /** What happens the moment this launches, in one sentence. */
  startup: string;
  /** Autonomous + held tool approvals: every call parks for a human. */
  showHoldNote: boolean;
  /** The run's tool_rules in one line, or null when it has none. */
  toolRules: string | null;
  launch: {
    onLaunch: () => void;
    /** useDeferredBusy: disabled the instant it fires. */
    disabled: boolean;
    /** useDeferredBusy: the spinner arrives ~200ms later. */
    spinning: boolean;
    inFlight: boolean;
    /** Why Launch cannot be pressed — a disabled button that won't say is a dead end. */
    problem: string | null;
    error: string | null;
  };
  preflight: { error: string | null; result: PreflightResult | null };
}

export function RunRail({
  savedPolicy,
  cc,
  showModelWarning,
  startup,
  showHoldNote,
  toolRules,
  launch,
  preflight,
}: RunRailProps) {
  return (
    <aside className="h-fit rounded-xl border border-border bg-card p-4 lg:sticky lg:top-6">
      <p className="mb-3 text-sm font-semibold text-foreground">What this run can do</p>

      {/* Below lg the rail sits UNDER the form at full width, so its sections
          read across instead of stacking into a very tall column. */}
      <div className="grid gap-x-6 gap-y-3 sm:grid-cols-2 md:grid-cols-4 lg:grid-cols-1">
        {savedPolicy && (
          <RailSection title="Policy">
            <p className="text-body font-medium text-foreground">{savedPolicy.name}</p>
            <p className="mt-0.5 text-xs text-muted-foreground">
              The stored spec governs this run — barrier floor{" "}
              {CC_META[savedPolicy.spec.min_confinement_class].label},{" "}
              {savedPolicy.spec.allow_all_egress
                ? "open egress"
                : `${(savedPolicy.spec.allowed_domains ?? []).length} host${(savedPolicy.spec.allowed_domains ?? []).length === 1 ? "" : "s"} allowed`}
              . It launches by reference, so nothing on this page is merged into it.
            </p>
          </RailSection>
        )}

        <RailSection title="Barrier">
          <div className="mb-1 flex items-center gap-2">
            <Chip tone="neutral">{CC_META[cc].label}</Chip>
            <span className="text-xs text-muted-foreground">· {CC_META[cc].tagline}</span>
          </div>
          <p className="text-xs text-muted-foreground">{CC_META[cc].doesntProtect}</p>
        </RailSection>

        <RailSection title="Credentials">
          {showModelWarning && (
            <p className="mb-1.5 rounded-md border border-warning/30 bg-warning-subtle px-2 py-1.5 text-xs text-foreground">
              No model provider is connected. This run launches; its first model call fails.{" "}
              {/* Rulebook §9: the action that fills the gap rides next to the
                  need, not only in a footer. Links are --info, never teal. */}
              <Link to="/settings" className="font-medium text-info hover:underline">
                Connect →
              </Link>
            </p>
          )}
          <p className="text-xs leading-relaxed text-muted-foreground">
            Minted at launch, injected by the proxy. Never written into the sandbox.
          </p>
        </RailSection>

        {/* What actually happens when this launches. The startup choice is a
            real fork, and the rail is where this screen states consequences
            rather than leaving them to be discovered. */}
        <RailSection title="Startup">
          <p className="text-xs text-muted-foreground">{startup}</p>
          {showHoldNote && (
            <p className="mt-2 text-xs text-muted-foreground">
              Tool use parks as approvals — an operator decides each one.
            </p>
          )}
        </RailSection>

        {/* One line, and it NAMES the tools: "3 rules" alone would say nothing
            about which calls still stop for a human. */}
        {toolRules && (
          <RailSection title="Tool rules">
            <p className="text-xs text-muted-foreground">{toolRules}</p>
          </RailSection>
        )}

        <RailSection title="Recording">
          <p className="text-xs text-muted-foreground">
            Every keystroke and every outbound connection.
          </p>
        </RailSection>
      </div>

      {launch.error && (
        <p className="mt-3 flex items-start gap-1.5 text-xs text-danger">
          <TriangleAlert className="mt-0.5 size-3.5 shrink-0" />
          {launch.error}
        </p>
      )}

      {/* Preflight moved ONTO the Policy panel, next to the document it checks —
          one button, not two competing ones. Its result stays here, beside
          Launch, because "what would be clamped" is the last thing read before
          committing. */}
      <div className="mt-4 flex gap-2">
        <Button
          type="button"
          className="flex-1"
          disabled={launch.disabled || !!launch.problem}
          onClick={launch.onLaunch}
        >
          {/* The icon slot always renders (never just on launching) so the
              has-[>svg] padding rule and the icon+gap width never change —
              toggling `invisible` cannot shift "Launch run" sideways the way
              mounting/unmounting the icon would. */}
          <Loader2 className={launch.spinning ? "size-4 animate-spin" : "size-4 animate-spin invisible"} />
          Launch run
        </Button>
      </div>
      {/* A disabled button that doesn't say why is a dead end. This screen had
          NO client-side validation at all before — an empty form launched, and
          the server's rejection arrived after the fact. */}
      {launch.problem && !launch.inFlight && (
        <p className="mt-2 text-center text-xs text-muted-foreground">{launch.problem}</p>
      )}

      {preflight.error && (
        <p className="mt-3 flex items-start gap-1.5 text-xs text-danger">
          <TriangleAlert className="mt-0.5 size-3.5 shrink-0" />
          {preflight.error}
        </p>
      )}
      {/* UNFRAMED: a bordered box inside the rail card is a card in a card
          (CONSOLE-RULES §9). A divider is what separates a section from the
          section above it. */}
      {preflight.result && (
        <div className="mt-4 border-t border-border pt-3" data-testid="preflight-result">
          <div className="mb-1.5 flex flex-wrap items-center gap-2">
            {preflight.result.overall_risk && <RiskBadge level={preflight.result.overall_risk} />}
            <ConfinementChip value={preflight.result.enforced_confinement_class} />
          </div>
          {preflight.result.warnings && preflight.result.warnings.length > 0 ? (
            <ul className="list-disc space-y-0.5 pl-4 text-xs text-warning">
              {preflight.result.warnings.map((w, i) => (
                <li key={i}>{w}</li>
              ))}
            </ul>
          ) : (
            <p className="text-xs text-muted-foreground">No adjustments.</p>
          )}
        </div>
      )}
    </aside>
  );
}
