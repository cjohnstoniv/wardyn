/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// DemoDetail — one Getting-Started demo sub-step: a single demo shown in full.
// It teaches BOTH what the sandbox does (overview + command walkthrough + a live,
// runnable sandbox) AND how you'd set one up yourself (the exact policy Wardyn
// runs, plus the New Run wizard steps). Composes the shared runner pieces
// (../demos/demo-runner) so launch/terminal/approvals behave identically.
// Default-exported so setup-screen can React.lazy() it and keep xterm out of
// the main setup chunk.
//
// THE single demo renderer since /demos died — which is why it carries two
// things that used to live only on that page's DemoCard:
//  - the `demo-card-<id>` testid on its wrapper, so a demo is still addressable
//    by the same selector after the URL swap (funnel.ts documents that trap);
//  - the "Turn this into a policy" payoff: DemoRunControls only renders that
//    button when given `onTurnIntoPolicy`, and the ProfileReview sheet it opens
//    was mounted by the deleted DemoScreen. record-a-policy's own steps still
//    tell the operator to click it, so it moved here with the renderer.
import { useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { TriangleAlert } from "lucide-react";
import {
  DemoCaution,
  DemoRunControls,
  StepList,
  useDemoRuns,
} from "../demos/demo-runner";
import { type Demo } from "../demos/demo-catalog";
import { ProfileReview } from "../profile-review";
import { YamlBlock } from "../../wardyn/code-block";
import { useOperator } from "../../wardyn/operator-context";
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
  githubAppReady = true,
  onJump,
  onDemoLaunched,
}: {
  demo: Demo;
  barrierReady: boolean;
  /** SetupStatus.secrets.github_app. Only a `needsGitHubApp` demo reads it;
   *  defaults TRUE so an unloaded status never invents a gate. */
  githubAppReady?: boolean;
  onJump: (id: SetupStepId) => void;
  onDemoLaunched: (demoId: string) => void;
}) {
  const { runs, starting, start, end, createErrors } = useDemoRuns(onDemoLaunched);
  // Local open-state only — ProfileReview needs nothing but a runId.
  const [profileRunId, setProfileRunId] = useState<string | null>(null);
  // Member vs operator: this gate reads a SECRETS field, and a member's /setup
  // status has Secrets zeroed by redactSetupStatusForMember — so it reads
  // closed for a member whether or not an App exists. Telling them to go
  // configure one would be doubly false (they cannot write secrets either), so
  // the copy says what is actually true for them.
  const operator = useOperator();
  const appGated = !!demo.needsGitHubApp && !githubAppReady;

  return (
    <div className="space-y-6" data-testid={`demo-card-${demo.id}`}>
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

      {appGated && (
        <div
          className="flex items-start gap-2 rounded-xl border border-warning/30 bg-warning-subtle px-4 py-3 text-sm text-warning"
          data-testid="demo-needs-github-app"
        >
          <TriangleAlert className="mt-0.5 size-4 shrink-0" />
          <p>
            This one needs a GitHub App. Its credential is an installation token minted from the
            LIVE GitHub API, so — unlike every other demo here — there is nothing to fake locally,
            and Start stays closed.{" "}
            {operator ? (
              <>
                Configure one under{" "}
                <Link to="/settings" className="font-medium underline underline-offset-2">
                  Settings
                </Link>{" "}
                (App id + private key), then come back.
              </>
            ) : (
              <>
                Ask an operator to configure one — and note that a member's setup status reports no
                App either way, so this gate stays closed for you regardless.
              </>
            )}{" "}
            Everything below still teaches the lane.
          </p>
        </div>
      )}

      <Section title="What you'll run">
        {/* runId matters: a cmd carrying the literal "{grant_id}" token renders
            it as-is in this pre-launch preview and substitutes the run's real
            grant id once one is live. The dead /demos DemoCard was the only
            renderer passing it, so without this the authorized-not-issued mint
            command would hand the operator a command that cannot work. */}
        <StepList steps={demo.steps} runId={runs[demo.id]?.id} />
      </Section>

      <Section title="The policy Wardyn runs">
        <p className="text-xs text-muted-foreground">
          The exact confinement this sandbox launches under — the readable form of a Wardyn policy
          (the canonical on-disk form is JSON, e.g. <code className="font-mono">examples/policies/*.json</code>).
        </p>
        <div data-testid={`demo-policy-${demo.id}`}>
          <YamlBlock value={demo.policy} />
        </div>
      </Section>

      {/* "Try it" sits directly under the policy so a running demo frames the
          policy, the terminal, and the audit/approvals together — everything
          relevant in one shot. The manual "set up yourself" steps are the
          supplementary alternative, so they move to the bottom. */}
      <Section title="Try it">
        <DemoRunControls
          demo={demo}
          run={runs[demo.id]}
          starting={starting === demo.id}
          barrierReady={barrierReady}
          githubAppReady={githubAppReady}
          createError={createErrors[demo.id]}
          loading={false}
          onStart={() => start(demo)}
          onEnd={(runId) => end(demo, runId)}
          onTurnIntoPolicy={setProfileRunId}
        />
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

      <ProfileReview runId={profileRunId} onClose={() => setProfileRunId(null)} />
    </div>
  );
}
