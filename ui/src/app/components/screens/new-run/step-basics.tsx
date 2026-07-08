/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Step 1 — Basics: agent, onboarded-workspace multi-select, run mode
// (interactive | batch), and an optional task.
import * as React from "react";
import { Textarea } from "../../ui/textarea";
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
import type { Workspace } from "../../../lib/types";
import { workspaceProfileOptions, type RunMode, type WizardAgent, type WizardState } from "./wizard-types";

export function StepBasics({
  state,
  patch,
  workspaces,
  workspacesLoading,
  profileLoading,
  onSelectProfile,
  onClearProfile,
  onAddWorkspace,
}: {
  state: WizardState;
  patch: (p: Partial<WizardState>) => void;
  workspaces: Workspace[];
  workspacesLoading: boolean;
  // A recorded profile is being synthesized/applied (disables the picker briefly).
  profileLoading: boolean;
  // Apply a recorded profile (a workspace recording) — loads its synthesized spec.
  onSelectProfile: (runId: string, key: string) => void;
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

  return (
    <div className="space-y-5">
      <Field label="Agent" hint="Only the two supported coding agents can be launched.">
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

      <Field
        label="Workspaces"
        hint="Only onboarded local directories and repos can be attached — a raw host path is never accepted. The first one selected is the primary (drives the run's base image); the rest are attached alongside it."
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

      {/* Recorded profiles for the primary workspace — its own recordings, a fast-
          track for already-set-up runs. Picking one loads that recording's access,
          egress & confinement and lets you skip straight to Review. Only shown when
          the workspace has recordings. */}
      {profiles.length > 0 && (
        <Field
          label={
            <span className="inline-flex items-center gap-1.5">
              Profile {profileLoading && <Loader2 className="size-3.5 animate-spin" />}
            </span>
          }
          hint="Reuse a session you recorded on this workspace — it fills in access, egress, and confinement so you can review and launch straight away. Choose “Configure manually” to set them by hand."
        >
          <RadioGroup
            value={state.selectedProfile ?? "__manual__"}
            onValueChange={(v) => {
              if (v === "__manual__") {
                onClearProfile();
                return;
              }
              const p = profiles.find((x) => x.key === v);
              if (p) onSelectProfile(p.runId, p.key);
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
                  <p className="text-[11px] leading-snug text-muted-foreground">
                    Recorded on this workspace — loads its access, egress &amp; confinement.
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
                <p className="text-[11px] leading-snug text-muted-foreground">
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
              <p className="text-[11px] leading-snug text-muted-foreground">You drive via attach</p>
            </div>
          </label>
          <label className="flex items-start gap-2 rounded-lg border border-border p-2.5">
            <RadioGroupItem value="batch" id="mode-batch" className="mt-0.5" />
            <div className="min-w-0">
              <Label htmlFor="mode-batch" className="cursor-pointer">
                Autonomous
              </Label>
              <p className="text-[11px] leading-snug text-muted-foreground">Runs the task</p>
            </div>
          </label>
        </RadioGroup>
      </Field>

      <Field
        label={
          <span>
            Task{" "}
            {state.mode === "interactive" && (
              <span className="font-normal text-muted-foreground">(optional)</span>
            )}
          </span>
        }
        htmlFor="task"
        hint={
          state.mode === "interactive"
            ? "Interactive runs come up idle so you can attach and drive them."
            : "Autonomous runs need a task — the agent runs it unattended."
        }
      >
        <Textarea
          id="task"
          placeholder="Describe what the agent should accomplish…"
          value={state.task}
          onChange={(e) => patch({ task: e.target.value })}
          rows={3}
        />
      </Field>
    </div>
  );
}
