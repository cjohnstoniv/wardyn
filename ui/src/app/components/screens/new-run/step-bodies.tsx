/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run's "What to run" section body — split out because new-run-screen.tsx
// sits at the file-size gate's ceiling (scripts/check-file-size.sh, 1000
// lines), same reason as policy-lane.ts/wizard-spec.ts/use-launch.ts. Purely
// presentational: the screen still owns `state` and passes `patch` down, same
// contract as WorkspaceCard.
import type { SetupHarnessTool } from "../../../lib/types";
import { SectionCard, Seg } from "./new-run-primitives";
import { AgentPicker } from "./agent-picker";
import { Checkbox } from "../../ui/checkbox";
import { Textarea } from "../../ui/textarea";
import { Field } from "../../wardyn/form-primitives";
import { RUN_MODE } from "../../wardyn/copy";
import { effectiveToolApprovals } from "./policy-lane";
import type { WizardState } from "./wizard-types";

export interface WhatToRunStepProps {
  state: WizardState;
  patch: (p: Partial<WizardState>) => void;
  isAgent: boolean;
  isInteractive: boolean;
  agentName: string;
  harnesses: SetupHarnessTool[] | undefined;
}

// "What to run" — run type, agent, run mode, and the task/seed/tool-approval
// fields each mode needs. Moved verbatim out of new-run-screen.tsx.
export function WhatToRunStep({ state, patch, isAgent, isInteractive, agentName, harnesses }: WhatToRunStepProps) {
  return (
    <SectionCard title="What to run">
      <div className="space-y-4">
        {/* The choice that proves a run needn't involve AI at all. */}
        <Seg
          label="Run type"
          value={state.runType}
          onChange={(id) => patch({ runType: id as WizardState["runType"] })}
          options={[
            { id: "agent", label: "Agent task" },
            { id: "command", label: "Shell command" },
          ]}
        />

        {isAgent && <AgentPicker value={state.agent} harnesses={harnesses} onChange={(agent) => patch({ agent })} />}

        {/* The mode comes BEFORE the field it selects: an interactive run
            is configured by a startup choice, an autonomous run by a prompt, and
            a shell command by the command — never all three at once.
            Hidden for Shell command, which is unattended by definition. */}
        {isAgent && (
          <Seg
            label="Run mode"
            value={state.mode}
            onChange={(id) => patch({ mode: id as WizardState["mode"] })}
            options={[
              // Label from copy.ts's RUN_MODE canon, not spelled here: the
              // internal id stays "batch" (it is wire-adjacent and renaming
              // it reaches the spec builder and its tests), but the word a
              // human reads is "Autonomous" everywhere else in the product.
              // The two had drifted, and this radio was the last place the
              // console still said "Batch" out loud.
              { id: "batch", label: `${RUN_MODE.autonomous.label} — run it unattended` },
              { id: "interactive", label: "Interactive — I drive the terminal" },
            ]}
          />
        )}

        {isInteractive ? (
          // What an interactive run actually configures is what greets you
          // when you attach — plus, now, an OPTIONAL boot seed (Part A1):
          // the same task text a batch run would use as a prompt, fired
          // once at sandbox boot instead of discarded. Left blank, it's
          // exactly today's idle-until-attach run.
          <>
            <Seg
              label="Start with"
              hint="The workspace is prepared before you land in it. Same barrier, same recording either way."
              value={state.interactiveStart}
              onChange={(id) => patch({ interactiveStart: id as WizardState["interactiveStart"] })}
              options={[
                { id: "agent", label: `${agentName} — launch it in the workspace` },
                { id: "shell", label: "Terminal — a shell in the workspace dir" },
              ]}
            />
            {state.interactiveStart === "agent" ? (
              <Field
                label="Initial prompt (optional)"
                htmlFor="nr-seed"
                hint={`Starts ${agentName} on this at boot, in the same session you attach to. Leave it blank to come up idle instead.`}
              >
                <Textarea
                  id="nr-seed"
                  rows={3}
                  placeholder="Start by reviewing the failing tests in payments/"
                  value={state.task}
                  onChange={(e) => patch({ task: e.target.value })}
                />
              </Field>
            ) : (
              <Field
                label="Startup command (optional)"
                htmlFor="nr-seed"
                hint="Runs at boot, before you attach. Leave it blank to come up idle instead."
              >
                <Textarea
                  id="nr-seed"
                  rows={2}
                  className="font-mono"
                  placeholder="npm ci && npm run dev"
                  value={state.task}
                  onChange={(e) => patch({ task: e.target.value })}
                />
              </Field>
            )}
            {/* Only an agent-started seed has a tool-approval prompt to
                auto-approve; a bare shell command has none, and an empty
                seed has nothing to run unsupervised in the first place. */}
            {state.interactiveStart === "agent" && state.task.trim() && (
              <div className="space-y-2">
                <label
                  htmlFor="nr-seed-auto-tools"
                  className="flex items-center gap-2 text-xs text-foreground"
                >
                  <Checkbox
                    id="nr-seed-auto-tools"
                    checked={state.seedAutoTools}
                    onCheckedChange={(v) => patch({ seedAutoTools: v === true })}
                  />
                  Let it use tools before I attach
                </label>
                <p className="text-xs leading-snug text-muted-foreground">
                  Auto-approves the agent&apos;s own tool use until you join. The sandbox and
                  egress policy still apply.
                </p>
              </div>
            )}
          </>
        ) : (
          <>
            <Field
              label={isAgent ? "Task" : "Command"}
              htmlFor="nr-task"
              required
              hint={
                isAgent
                  ? "Described in plain English. The agent decides how to do it."
                  : "Run verbatim in the sandbox. No agent, no model — the same governance either way."
              }
            >
              <Textarea
                id="nr-task"
                rows={4}
                required
                className={isAgent ? undefined : "font-mono"}
                placeholder={isAgent ? "Fix the flaky test in payments/refund_test.go" : "make test"}
                value={state.task}
                onChange={(e) => patch({ task: e.target.value })}
              />
            </Field>
            {/* Autonomous agent runs only — a shell command has no tool
                calls to approve, and codex has no external approval
                contract to route them through (disabled below, honestly). */}
            {isAgent && (
              <Seg
                label="Tool approvals"
                value={effectiveToolApprovals(state.agent, state.toolApprovals)}
                onChange={(id) => patch({ toolApprovals: id as WizardState["toolApprovals"] })}
                hint={
                  state.agent === "codex-cli"
                    ? "Codex CLI has no external tool-approval contract — this run always keeps the sandbox as its only boundary."
                    : undefined
                }
                options={[
                  { id: "auto", label: "Auto — the sandbox is the boundary" },
                  {
                    id: "hold",
                    label: "Hold in Wardyn — every tool call parks as an approval",
                    disabled: state.agent === "codex-cli",
                  },
                ]}
              />
            )}
          </>
        )}
      </div>
    </SectionCard>
  );
}
