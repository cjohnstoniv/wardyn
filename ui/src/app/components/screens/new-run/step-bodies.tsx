/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Run panel's "what to run" controls — run type, agent, run mode, and the
// one text field each run shape needs. Purely presentational: the screen still
// owns `state` and passes `patch` down, same contract as WorkspaceCard.
import * as React from "react";
import type { SetupHarnessTool } from "../../../lib/types";
import { Seg } from "./new-run-primitives";
import { AgentPicker } from "./agent-picker";
import { Button } from "../../ui/button";
import { Checkbox } from "../../ui/checkbox";
import { Input } from "../../ui/input";
import { Textarea } from "../../ui/textarea";
import { Field } from "../../wardyn/form-primitives";
import { RUN_MODE } from "../../wardyn/copy";
import { NEW_RUN_FLOW } from "../../wardyn/copy/new-run-flow";
import { ISSUE_TARGET } from "./new-run-launch-gates";
import { effectiveToolApprovals } from "./policy-lane";
import type { WizardState } from "./wizard-types";

export interface WhatToRunStepProps {
  state: WizardState;
  patch: (p: Partial<WizardState>) => void;
  isAgent: boolean;
  isInteractive: boolean;
  agentName: string;
  harnesses: SetupHarnessTool[] | undefined;
  /** The model provider picker, which sits between the run mode and the field
   *  that mode selects. */
  provider: React.ReactNode;
  /** The required Task or Command is empty; `describedBy` names the line above
   *  Launch while it is the one that says so. */
  taskInvalid: boolean;
  taskDescribedBy: string | undefined;
  /** The run's tool_rules in one line (the rail's own sentence), or null. */
  toolRules: string | null;
  /** Reveals Policy on the tool rules. */
  onToolRules: () => void;
}

// A command is one line, in mono, behind a prompt glyph that is decoration and
// never part of the value or the field's name. Enter neither launches nor adds
// a line; a pasted line break is joined by the browser.
const CommandInput = React.forwardRef<HTMLInputElement, React.ComponentProps<typeof Input>>(function CommandInput(props, ref) {
  return (
    <div className="relative">
      <span aria-hidden="true" className="pointer-events-none absolute top-1/2 left-3 -translate-y-1/2 font-mono text-sm text-muted-foreground">
        $
      </span>
      <Input ref={ref} type="text" autoComplete="off" autoCapitalize="off" autoCorrect="off" spellCheck={false} {...props} className="pl-8 font-mono" />
    </div>
  );
});

// Run type, agent, run mode, then the task/seed/tool-approval fields that mode
// needs. Task, Command and Startup command each hold their own value.
export function WhatToRunStep({
  state, patch, isAgent, isInteractive, agentName, harnesses, provider, taskInvalid, taskDescribedBy, toolRules, onToolRules,
}: WhatToRunStepProps) {
  const toolApprovals = effectiveToolApprovals(state.agent, state.toolApprovals);
  return (
      <>
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
            id={ISSUE_TARGET.RUN_MODE}
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

        {provider}

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
                <CommandInput
                  id="nr-seed"
                  placeholder="npm ci && npm run dev"
                  value={state.startupCommand}
                  onChange={(e) => patch({ startupCommand: e.target.value })}
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
              {isAgent ? (
                <Textarea
                  id={ISSUE_TARGET.TASK}
                  rows={4}
                  required
                  placeholder="Fix the flaky test in payments/refund_test.go"
                  value={state.task}
                  onChange={(e) => patch({ task: e.target.value })}
                  aria-invalid={taskInvalid || undefined}
                  aria-describedby={taskDescribedBy}
                />
              ) : (
                <CommandInput
                  id={ISSUE_TARGET.TASK}
                  required
                  placeholder="make test"
                  value={state.command}
                  onChange={(e) => patch({ command: e.target.value })}
                  aria-invalid={taskInvalid || undefined}
                  aria-describedby={taskDescribedBy}
                />
              )}
            </Field>
            {/* Autonomous agent runs only — a shell command has no tool
                calls to approve, and codex has no external approval
                contract to route them through (disabled below, honestly). */}
            {isAgent && (
              <Seg
                label="Tool approvals"
                value={toolApprovals}
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
                    label: NEW_RUN_FLOW.HOLD_LABEL,
                    disabled: state.agent === "codex-cli",
                  },
                ]}
              />
            )}
            {/* Which calls a hold actually stops is the policy's tool rules:
                the rail's own sentence, and the way to them. Rule editing stays
                in Policy. */}
            {isAgent && toolApprovals === "hold" && (
              <div className="-mt-2 flex flex-wrap items-baseline gap-2 text-xs text-muted-foreground">
                {toolRules && <span>{toolRules}</span>}
                <Button type="button" variant="link" size="sm" className="h-auto p-0 text-xs" onClick={onToolRules}>
                  {NEW_RUN_FLOW.TOOL_RULES_LINK}
                </Button>
              </div>
            )}
          </>
        )}
      </>
  );
}
