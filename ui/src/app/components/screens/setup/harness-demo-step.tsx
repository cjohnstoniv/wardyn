/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// HarnessDemoStep — Getting Started's 13th (last Demos) step: the catalog's
// fifth demo ("The agent in the box", demo-catalog.ts's needsModel entry),
// promoted off /demos-only onto the rail, ALWAYS visible. Claude Code only —
// the catalog's task/policy are Anthropic-specific and the codex-cli agent
// image carries no claude binary (see demos/harness-demo.ts's
// harnessAvailability). Three states:
//   - locked (no agent-capable AI integration resolves): an invitation, not
//     an error — no Start button, no warning tone.
//   - locked, OpenAI-only (one resolves but can't drive Claude Code): the
//     same invitation, plus one honest extra line naming why.
//   - live (a Claude-Code-capable row resolves): the SAME demo composition
//     /demos itself renders (DemoCaution/StepList/DemoRunControls, which
//     embeds DemoAuditPanel while running — demos/demo-screen.tsx), plus a
//     preflight checklist above Start. No integration_id override is sent —
//     the server's own resolution picks the credential, exactly like the
//     four keyless demos (a client-synthesized row id doesn't match a real
//     server integration id and would 400 the launch).
// Launching a run is viewer-tier in this product (server.go puts POST /runs
// on the plain route group, http.go says so explicitly; none of the four
// keyless demo steps gate Start on role either) — no operator check here.
// Default-exported + lazy-loaded from setup-screen.tsx (pulls in xterm via
// DemoRunControls, same reasoning as demos-step.tsx's DemoDetail for the
// other four demo steps).
import * as React from "react";
import { Sparkles, TriangleAlert } from "lucide-react";
import { Button } from "../../ui/button";
import { EmptyState } from "../../wardyn/states";
import { DemoCaution, DemoRunControls, StepList, demoRunBody, useDemoRuns } from "../demos/demo-screen";
import { DEMOS } from "../demos/demo-catalog";
import { harnessAvailability } from "../demos/harness-demo";
import { runs as runsApi } from "../../../lib/api/runs";
import { SetupChecklist } from "../new-run/compose-review";
import { T } from "../../../lib/integrations";
import type { PreflightResult, SetupStatus } from "../../../lib/types";
import type { SetupStepId } from "./steps";

const DEMO = DEMOS.find((d) => d.id === "agent-in-the-box")!;

export default function HarnessDemoStep({
  status,
  barrierReady,
  onJump,
  onDemoLaunched,
}: {
  status: SetupStatus;
  barrierReady: boolean;
  /** Navigates to another Getting-Started step (the orchestrator's step-select). */
  onJump: (id: SetupStepId) => void;
  /** Per-browser "was this demo launched" signal (setup-screen.tsx's launchedDemos). */
  onDemoLaunched: (demoId: string) => void;
}) {
  const { runs, starting, start, end } = useDemoRuns(onDemoLaunched);
  const availability = harnessAvailability(status);
  const live = availability === "claude_ready";

  const [preflight, setPreflight] = React.useState<PreflightResult | null>(null);
  const [preflightStatus, setPreflightStatus] = React.useState<"loading" | "idle" | "error">("loading");
  React.useEffect(() => {
    if (!live) return;
    let alive = true;
    setPreflightStatus("loading");
    runsApi
      .preflightRun(demoRunBody(DEMO))
      .then((r) => {
        if (!alive) return;
        setPreflight(r);
        setPreflightStatus("idle");
      })
      .catch(() => {
        if (!alive) return;
        setPreflight(null);
        setPreflightStatus("error");
      });
    return () => {
      alive = false;
    };
  }, [live]);

  if (!live) {
    return (
      <div className="space-y-5">
        <p className="text-sm leading-relaxed text-muted-foreground">{T.HARNESS_LOCKED_LEDE}</p>
        {availability === "openai_only" && (
          <p className="text-sm leading-relaxed text-muted-foreground">{T.HARNESS_OPENAI_ONLY_NOTE}</p>
        )}
        <div className="rounded-xl border border-border">
          <EmptyState
            icon={Sparkles}
            title={T.HARNESS_LOCKED_PANEL}
            action={<Button onClick={() => onJump("integrations")}>{T.HARNESS_LOCKED_CTA}</Button>}
          />
        </div>
      </div>
    );
  }

  const llmItem = preflight?.setup_items?.find((i) => i.kind === "llm_access");
  // Fails CLOSED: only a RESOLVED preflight naming model access satisfied
  // unblocks Start — an absent row counts as unsatisfied, never a free pass.
  // llmReady/harnessAvailability is a CLIENT-SIDE secret/config scan; the
  // server's own resolution can still disagree (e.g. a stored default that no
  // longer resolves) — this preflight is the one call that can't be wrong. A
  // transport error stays advisory (never blocks) — every other checklist row
  // is advisory too (it renders, it just never gates Start).
  const llmBlocked =
    preflightStatus === "loading" || (preflightStatus === "idle" && llmItem?.status !== "satisfied");
  const run = runs[DEMO.id];

  return (
    <div className="space-y-6">
      <p className="text-sm leading-relaxed text-muted-foreground">{DEMO.overview}</p>

      {!barrierReady && (
        <div
          className="flex items-start gap-2 rounded-xl border border-warning/30 bg-warning-subtle px-4 py-3 text-sm text-warning"
          data-testid="harness-step-not-ready"
        >
          <TriangleAlert className="mt-0.5 size-4 shrink-0" />
          <p>
            Demos need the sandbox runner — finish the{" "}
            <button
              type="button"
              onClick={() => onJump("environment")}
              className="font-medium underline underline-offset-2 hover:text-foreground"
            >
              Environment step
            </button>{" "}
            first, then come back.
          </p>
        </div>
      )}

      {DEMO.caution && <DemoCaution text={DEMO.caution} />}

      <StepList steps={DEMO.steps} />

      {preflightStatus === "error" && (
        <p className="text-xs text-muted-foreground" data-testid="harness-preflight-unavailable">
          {T.HARNESS_PREFLIGHT_UNAVAILABLE}
        </p>
      )}
      {preflight && preflight.setup_items.length > 0 && (
        <div data-testid="harness-preflight-checklist">
          <SetupChecklist items={preflight.setup_items} />
        </div>
      )}
      {/* A resolved-but-unsatisfied llm_access row: the checklist row above
          already carries its label/detail, but a dead Start with no forward
          path is a dead end — give it the same escape the locked panel has. */}
      {preflightStatus === "idle" && llmItem && llmItem.status !== "satisfied" && (
        <div
          className="flex items-start gap-2 rounded-lg border border-warning/40 bg-warning-subtle px-3 py-2.5 text-xs text-warning"
          data-testid="harness-llm-access-blocked"
        >
          <TriangleAlert className="mt-0.5 size-3.5 shrink-0" />
          <div>
            <p>{llmItem.detail || llmItem.label}</p>
            <Button size="sm" variant="outline" className="mt-2" onClick={() => onJump("integrations")}>
              {T.HARNESS_LOCKED_CTA}
            </Button>
          </div>
        </div>
      )}

      <DemoRunControls
        demo={DEMO}
        run={run}
        starting={starting === DEMO.id}
        barrierReady={barrierReady}
        loading={false}
        disabled={llmBlocked}
        onStart={() => start(DEMO)}
        onEnd={(runId) => end(DEMO, runId)}
      />
    </div>
  );
}
