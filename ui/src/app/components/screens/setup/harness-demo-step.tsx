/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// HarnessDemoStep — Getting Started's 13th (last Demos) step: the catalog's
// fifth demo ("The agent in the box", demo-catalog.ts's needsModel entry),
// promoted off /demos-only onto the rail, ALWAYS visible. Two honest states:
//   - locked (no agent-capable AI integration resolves — pickHarnessBinding
//     returns null, the same fact Readiness.llmReady is built from): an
//     invitation, not an error — no Start button, no warning tone.
//   - live (one resolves): the SAME demo composition /demos itself renders
//     (DemoCaution/StepList/DemoRunControls, which embeds DemoAuditPanel while
//     running — demos/demo-screen.tsx), plus a preflight checklist above Start.
// Default-exported + lazy-loaded from setup-screen.tsx (pulls in xterm via
// DemoRunControls, same reasoning as demos-step.tsx's DemoDetail for the other
// four demo steps).
import * as React from "react";
import { Sparkles, TriangleAlert } from "lucide-react";
import { Button } from "../../ui/button";
import { EmptyState } from "../../wardyn/states";
import { useOperator } from "../../wardyn/operator-context";
import {
  DemoCaution,
  DemoRunControls,
  StepList,
  demoRunBody,
  useDemoRuns,
  type DemoOverlay,
} from "../demos/demo-screen";
import { DEMOS } from "../demos/demo-catalog";
import { pickHarnessBinding } from "../demos/harness-demo";
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
  // The demo step is ADMIN-scoped (operator == admin here; a later lane renames
  // role semantics) — launching a run is a write, gated the same way every
  // other write control in this funnel is (disabled + a title hint), never by
  // hiding an honestly-connected model from a viewer.
  const operator = useOperator();
  // null here IS the locked condition — the exact same agentCapableRows pick
  // Readiness.llmReady is derived from (demos/harness-demo.ts), so the two can
  // never disagree about whether a model is connected.
  const binding = pickHarnessBinding(status);
  const overlay: DemoOverlay | undefined = binding
    ? { agent: binding.agent, integration_id: binding.integrationId }
    : undefined;

  const [preflight, setPreflight] = React.useState<PreflightResult | null>(null);
  const [preflightStatus, setPreflightStatus] = React.useState<"loading" | "idle" | "error">("loading");
  React.useEffect(() => {
    if (!binding) return;
    let alive = true;
    setPreflightStatus("loading");
    runsApi
      .preflightRun(demoRunBody(DEMO, overlay))
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
    // Re-preflight only when the resolved binding itself changes, not on every
    // incidental status refresh that leaves it the same (mirrors wizard.tsx's
    // "fire once, not on every unrelated edit" discipline).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [binding?.integrationId, binding?.agent]);

  if (!binding) {
    return (
      <div className="space-y-5">
        <p className="text-sm leading-relaxed text-muted-foreground">{T.HARNESS_LOCKED_LEDE}</p>
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
  // Start is enabled only once a RESOLVED preflight names model access
  // satisfied. A transport error is advisory (never blocks); still-loading
  // stays closed rather than guess — every other checklist row is advisory too
  // (it renders, it just never gates Start).
  const llmBlocked =
    preflightStatus === "loading" || (preflightStatus === "idle" && !!llmItem && llmItem.status !== "satisfied");
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
          Preflight unavailable — you can still start.
        </p>
      )}
      {preflight && preflight.setup_items.length > 0 && (
        <div data-testid="harness-preflight-checklist">
          <SetupChecklist items={preflight.setup_items} />
        </div>
      )}

      <DemoRunControls
        demo={DEMO}
        run={run}
        starting={starting === DEMO.id}
        barrierReady={barrierReady}
        loading={false}
        disabled={!operator || llmBlocked}
        onStart={() => start(DEMO, overlay)}
        onEnd={(runId) => end(DEMO, runId)}
      />
    </div>
  );
}
