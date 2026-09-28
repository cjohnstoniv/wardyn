/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #1197 L3 — "Start a run" (design.md §1/§2.1). User view only; the Admin
// view has no launch door (M-7). Submitting reuses the SAME clone-prefill
// channel the run header's "Start a run like this one" already rides
// (RunPrefill via route state — wizard-types.ts) rather than a new one: task
// and workspace arrive exactly as a clone's would, with no policy/state
// overlay beyond them. Review F4: this is not a clone (no source run), so
// the prefill carries `source: "composer"`, which New run reads to skip its
// clone banner (the only change this needed there).
import * as React from "react";
import { useNavigate } from "react-router-dom";
import type { RunPrefill } from "../new-run/wizard-types";
import { useWorkspaceList } from "../../../lib/use-workspace-list";
import { Button } from "../../ui/button";
import { Textarea } from "../../ui/textarea";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import { RUNS_COMPOSER } from "../../wardyn/copy/runs-landing";
import { NO_REPO } from "./board-groups";

// Radix's SelectItem refuses an empty-string value — see runs.tsx's own
// historical note on this. A sentinel stands in for "no workspace".
const NO_WORKSPACE = "__none__";

export function RunsComposer() {
  const navigate = useNavigate();
  const { workspaces } = useWorkspaceList();
  const [task, setTask] = React.useState("");
  const [workspaceId, setWorkspaceId] = React.useState(NO_WORKSPACE);

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    const trimmed = task.trim();
    if (!trimmed) return;
    const prefill: RunPrefill = {
      state: {
        task: trimmed,
        workspaces: workspaceId === NO_WORKSPACE ? [] : [{ workspaceId }],
      },
      inlinePolicy: false,
      // Review F4: no source run, so New run must not show the clone banner.
      source: "composer",
    };
    void navigate("/runs/new", { state: { prefill } });
  };

  return (
    <form
      aria-label={RUNS_COMPOSER.LABEL}
      onSubmit={submit}
      className="mt-3 rounded-xl border border-border-strong bg-card p-3"
    >
      <label htmlFor="runs-composer-task" className="text-sm font-medium">
        {RUNS_COMPOSER.LABEL}
      </label>
      <Textarea
        id="runs-composer-task"
        value={task}
        onChange={(e) => setTask(e.target.value)}
        placeholder={RUNS_COMPOSER.PLACEHOLDER}
        className="mt-1 min-h-[3.25rem] border-0 bg-transparent px-0 shadow-none focus-visible:ring-0"
      />
      <div className="flex flex-wrap items-center justify-between gap-2 border-t border-border pt-2">
        <label className="flex items-center gap-1.5 text-xs text-muted-foreground">
          {RUNS_COMPOSER.WORKSPACE}
          <Select value={workspaceId} onValueChange={setWorkspaceId}>
            <SelectTrigger size="sm" aria-label={RUNS_COMPOSER.WORKSPACE} className="w-auto">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={NO_WORKSPACE}>{NO_REPO}</SelectItem>
              {workspaces.map((w) => (
                <SelectItem key={w.id} value={w.id}>
                  {w.name}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </label>
        <Button type="submit" disabled={!task.trim()}>
          {RUNS_COMPOSER.GO}
        </Button>
      </div>
    </form>
  );
}
