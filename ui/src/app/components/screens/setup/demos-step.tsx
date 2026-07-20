/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// DemoDetail — one Getting-Started "Demos" sub-step: a single demo shown in full.
// It teaches BOTH what the sandbox does (overview + command walkthrough + a live,
// runnable sandbox) AND how you'd set one up yourself (the exact policy Wardyn
// runs, plus the New Run wizard steps). Reuses the /demos runner pieces so the
// launch/terminal/approvals behave identically. Default-exported so setup-screen
// can React.lazy() it and keep xterm out of the main setup chunk.
import type { ReactNode } from "react";
import { TriangleAlert } from "lucide-react";
import {
  DemoCaution,
  DemoRunControls,
  StepList,
  useDemoRuns,
} from "../demos/demo-screen";
import { policyToYaml, type Demo } from "../demos/demo-catalog";
import type { SetupStepId } from "./steps";

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="space-y-2">
      <h3 className="text-sm font-semibold text-foreground">{title}</h3>
      {children}
    </section>
  );
}

export default function DemoDetail({
  demo,
  barrierReady,
  onJump,
  onDemoLaunched,
}: {
  demo: Demo;
  barrierReady: boolean;
  onJump: (id: SetupStepId) => void;
  onDemoLaunched: (demoId: string) => void;
}) {
  const { runs, starting, start, end } = useDemoRuns(onDemoLaunched);

  return (
    <div className="space-y-6">
      <p className="text-sm leading-relaxed text-muted-foreground">{demo.overview}</p>

      {demo.caution && <DemoCaution text={demo.caution} />}

      {!barrierReady && (
        <div
          className="flex items-start gap-2 rounded-xl border border-warning/30 bg-warning-subtle px-4 py-3 text-sm text-warning"
          data-testid="demos-step-not-ready"
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

      <Section title="What you'll run">
        <StepList steps={demo.steps} />
      </Section>

      <Section title="The policy Wardyn runs">
        <p className="text-xs text-muted-foreground">
          The exact confinement this sandbox launches under — the readable form of a Wardyn policy
          (the canonical on-disk form is JSON, e.g. <code className="font-mono">examples/policies/*.json</code>).
        </p>
        <pre
          className="overflow-x-auto rounded-lg border border-border bg-background/70 p-3 font-mono text-xs leading-relaxed text-foreground"
          data-testid={`demo-policy-${demo.id}`}
        >
          {policyToYaml(demo.policy)}
        </pre>
      </Section>

      <Section title="Set up a sandbox like this yourself">
        <ol className="space-y-1.5">
          {demo.setupUi.map((step, i) => (
            <li key={i} className="flex gap-2 text-sm text-muted-foreground">
              <span className="mt-0.5 flex size-5 shrink-0 items-center justify-center rounded-full bg-muted text-[0.6875rem] font-medium text-foreground">
                {i + 1}
              </span>
              <span className="min-w-0 leading-snug">{step}</span>
            </li>
          ))}
        </ol>
      </Section>

      <Section title="Try it">
        <DemoRunControls
          demo={demo}
          run={runs[demo.id]}
          starting={starting === demo.id}
          barrierReady={barrierReady}
          loading={false}
          onStart={() => start(demo)}
          onEnd={(runId) => end(demo, runId)}
        />
      </Section>
    </div>
  );
}
