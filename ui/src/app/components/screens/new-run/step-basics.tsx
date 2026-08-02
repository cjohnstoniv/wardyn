/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Step 1 — Basics: run type (agent vs governed command), agent (agent runs
// only), sandbox image, onboarded-workspace multi-select, run mode
// (interactive | batch), and the task/command.
import { Textarea } from "../../ui/textarea";
import { Input } from "../../ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "../../ui/select";
import { RadioGroup, RadioGroupItem } from "../../ui/radio-group";
import { Label } from "../../ui/label";
import { Field } from "./step-shell";
import { Loader2 } from "lucide-react";
import { WorkspacePicker } from "./workspace-picker";
import type { RunPolicy, Workspace } from "../../../lib/types";
import {
  workspaceProfileOptions,
  type RunMode,
  type RunType,
  type WizardAgent,
  type WizardState,
} from "./wizard-types";

export function StepBasics({
  state,
  patch,
  workspaces,
  workspacesLoading,
  policies = [],
  profileLoading,
  onSelectProfile,
  onSelectPolicy,
  onClearProfile,
  onAddWorkspace,
}: {
  state: WizardState;
  patch: (p: Partial<WizardState>) => void;
  workspaces: Workspace[];
  workspacesLoading: boolean;
  // The operator's saved policies (any workspace) — offered alongside the
  // recordings so a policy authored in the console can actually be RUN from it.
  policies?: RunPolicy[];
  // A recorded profile is being synthesized/applied (disables the picker briefly).
  profileLoading: boolean;
  // Apply a recorded profile (a workspace recording) — loads its synthesized spec.
  onSelectProfile: (runId: string, key: string) => void;
  // Apply a saved policy — loads its stored spec AND launches by reference.
  onSelectPolicy?: (policy: RunPolicy) => void;
  // Back to manual configuration (clears the fast-track selection).
  onClearProfile: () => void;
  onAddWorkspace: () => void;
}) {
  // Recorded profiles tied to the PRIMARY (first) selected workspace — they ARE its
  // recordings (record_results), not name-matched policies. Picking one populates
  // steps 2-4 and turns Next into "Review now".
  const primary = state.workspaces[0]
    ? workspaces.find((w) => w.id === state.workspaces[0].workspaceId)
    : undefined;
  const profiles = workspaceProfileOptions(primary);
  const savedPolicies = onSelectPolicy ? policies : [];

  return (
    <div className="space-y-5">
      <Field
        label="Run type"
        hint="A governed command runs `task` as a plain shell command in the sandbox — no agent, no model. An agent run drives a coding agent under Wardyn's harness and needs a model."
      >
        <RadioGroup
          value={state.runType}
          onValueChange={(v) => patch({ runType: v as RunType })}
          className="grid grid-cols-2 gap-2"
          data-testid="basics-run-type"
        >
          <label className="flex items-start gap-2 rounded-lg border border-border p-2.5">
            <RadioGroupItem value="agent" id="runtype-agent" className="mt-0.5" />
            <div className="min-w-0">
              <Label htmlFor="runtype-agent" className="cursor-pointer">
                Agent run
              </Label>
              <p className="text-[0.6875rem] leading-snug text-muted-foreground">Coding agent under the harness</p>
            </div>
          </label>
          <label className="flex items-start gap-2 rounded-lg border border-border p-2.5">
            <RadioGroupItem value="command" id="runtype-command" className="mt-0.5" />
            <div className="min-w-0">
              <Label htmlFor="runtype-command" className="cursor-pointer">
                Governed command
              </Label>
              <p className="text-[0.6875rem] leading-snug text-muted-foreground">Plain shell command, no agent</p>
            </div>
          </label>
        </RadioGroup>
      </Field>

      {state.runType === "agent" && (
        <Field label="Agent" hint="Claude Code and Codex CLI are the built-in agents; a custom image (below) can carry your own harness.">
          <Select value={state.agent} onValueChange={(v) => patch({ agent: v as WizardAgent })}>
            <SelectTrigger>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="claude-code">Claude Code</SelectItem>
              <SelectItem value="codex-cli">Codex CLI</SelectItem>
            </SelectContent>
          </Select>
        </Field>
      )}

      <Field
        label="Sandbox image"
        hint="Bring your own base image, or leave blank for the built-in agent image. Wardyn layers its runner tools onto it and clears its entrypoint before use, and runs a self-test before the task. Egress policy, confinement, and secret brokering apply regardless of image contents. Your image needs a shell (and, for an agent run, the agent's CLI). Pre-pull private images on the host; pin a @sha256: digest to avoid tag drift."
      >
        <Input
          placeholder="e.g. myco/dev:latest or ghcr.io/org/image@sha256:…"
          value={state.image}
          onChange={(e) => patch({ image: e.target.value })}
          className="font-mono"
          aria-label="Sandbox image"
        />
      </Field>

      <Field
        label="Workspaces"
        hint="Only onboarded directories, repos, and container images can be attached — a raw host path is never accepted. A container attaches as the run's base image (and its bound model access); directories/repos mount alongside. The first selected is the primary. Optional — leave empty for an ephemeral scratch run."
      >
        <WorkspacePicker
          selections={state.workspaces}
          // Changing the workspace invalidates any picked profile (it's tied to a
          // specific workspace), so clear the fast-track selection.
          onChange={(ws) => patch({ workspaces: ws, selectedProfile: undefined })}
          workspaces={workspaces}
          loading={workspacesLoading}
          onAddWorkspace={onAddWorkspace}
        />
      </Field>

      {/* Start from something already set up: this workspace's own recordings, or
          a saved policy. Picking either loads its access, egress & confinement and
          lets you skip straight to Review; a saved policy additionally launches BY
          REFERENCE (policy_id), so the server enforces the stored spec verbatim.
          Hidden when there is neither. */}
      {(profiles.length > 0 || savedPolicies.length > 0) && (
        <Field
          label={
            <span className="inline-flex items-center gap-1.5">
              Start from {profileLoading && <Loader2 className="size-3.5 animate-spin" />}
            </span>
          }
          hint="Reuse a session you recorded on this workspace, or a saved policy — either fills in access, egress, and confinement so you can review and launch straight away. Choose “Configure manually” to set them by hand."
        >
          <RadioGroup
            value={state.selectedProfile ?? "__manual__"}
            onValueChange={(v) => {
              if (v === "__manual__") {
                onClearProfile();
                return;
              }
              const p = profiles.find((x) => x.key === v);
              if (p) {
                onSelectProfile(p.runId, p.key);
                return;
              }
              const pol = savedPolicies.find((x) => x.id === v);
              if (pol) onSelectPolicy?.(pol);
            }}
            className="space-y-2"
            data-testid="basics-profiles"
          >
            {profiles.map((p) => (
              <label
                key={p.key}
                className="flex items-start gap-2 rounded-lg border border-border p-2.5"
                data-testid={`basics-profile-${p.key}`}
              >
                <RadioGroupItem value={p.key} id={`profile-${p.key}`} className="mt-0.5" disabled={profileLoading} />
                <div className="min-w-0">
                  <Label htmlFor={`profile-${p.key}`} className="cursor-pointer">
                    {p.label}
                  </Label>
                  <p className="text-[0.6875rem] leading-snug text-muted-foreground">
                    Recorded on this workspace — loads its access, egress &amp; confinement.
                  </p>
                </div>
              </label>
            ))}
            {savedPolicies.map((p) => (
              <label
                key={p.id}
                className="flex items-start gap-2 rounded-lg border border-border p-2.5"
                data-testid={`basics-policy-${p.id}`}
              >
                <RadioGroupItem value={p.id} id={`policy-${p.id}`} className="mt-0.5" disabled={profileLoading} />
                <div className="min-w-0">
                  <Label htmlFor={`policy-${p.id}`} className="cursor-pointer">
                    {p.name}
                  </Label>
                  <p className="text-[0.6875rem] leading-snug text-muted-foreground">
                    Saved policy — the run launches under it by reference; editing any step
                    switches back to a one-off inline policy.
                  </p>
                </div>
              </label>
            ))}
            <label className="flex items-start gap-2 rounded-lg border border-dashed border-border p-2.5">
              <RadioGroupItem value="__manual__" id="profile-manual" className="mt-0.5" disabled={profileLoading} />
              <div className="min-w-0">
                <Label htmlFor="profile-manual" className="cursor-pointer">
                  Configure manually
                </Label>
                <p className="text-[0.6875rem] leading-snug text-muted-foreground">
                  Step through access, egress &amp; confinement yourself.
                </p>
              </div>
            </label>
          </RadioGroup>
        </Field>
      )}

      <Field
        label="Mode"
        hint="Interactive is a first-class choice: the sandbox comes up idle and you drive it over an attached terminal. Autonomous runs the task unattended."
      >
        <RadioGroup
          value={state.mode}
          onValueChange={(v) => patch({ mode: v as RunMode })}
          className="grid grid-cols-2 gap-2"
        >
          <label className="flex items-start gap-2 rounded-lg border border-border p-2.5">
            <RadioGroupItem value="interactive" id="mode-interactive" className="mt-0.5" />
            <div className="min-w-0">
              <Label htmlFor="mode-interactive" className="cursor-pointer">
                Interactive
              </Label>
              <p className="text-[0.6875rem] leading-snug text-muted-foreground">You drive via attach</p>
            </div>
          </label>
          <label className="flex items-start gap-2 rounded-lg border border-border p-2.5">
            <RadioGroupItem value="batch" id="mode-batch" className="mt-0.5" />
            <div className="min-w-0">
              <Label htmlFor="mode-batch" className="cursor-pointer">
                Autonomous
              </Label>
              <p className="text-[0.6875rem] leading-snug text-muted-foreground">Runs the task</p>
            </div>
          </label>
        </RadioGroup>
      </Field>

      <Field
        label={
          <span>
            {state.runType === "command" ? "Command" : "Task"}{" "}
            {state.mode === "interactive" && (
              <span className="font-normal text-muted-foreground">(optional)</span>
            )}
          </span>
        }
        htmlFor="task"
        hint={
          state.mode === "interactive"
            ? "Interactive runs come up idle so you can attach and drive them."
            : state.runType === "command"
              ? "Autonomous runs need a command — it runs unattended as a plain shell command in the sandbox."
              : "Autonomous runs need a task — the agent runs it unattended."
        }
      >
        <Textarea
          id="task"
          placeholder={
            state.runType === "command"
              ? "e.g. npm run build && npm test"
              : "Describe what the agent should accomplish…"
          }
          value={state.task}
          onChange={(e) => patch({ task: e.target.value })}
          rows={3}
          className={state.runType === "command" ? "font-mono" : undefined}
        />
      </Field>
    </div>
  );
}
