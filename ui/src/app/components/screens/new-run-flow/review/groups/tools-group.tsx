/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Button } from "../../../../ui/button";
import { Input } from "../../../../ui/input";
import { Label } from "../../../../ui/label";
import { Chip } from "../../../../wardyn/primitives";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import type { IncludedTool } from "../../../../../lib/types/new-run-contract";
import type { RunIntent } from "../../draft/run-intent";
import { setCommand } from "../../lenses/run-lens";
import { harnessName, loadRoster } from "../../steps/harnesses/roster";
import { useLoaded } from "../../steps/shape/use-loaded";
import { draftSource, serverSource } from "../provenance";
import { ReviewGroup } from "../review-group";
import { stepLocation, type GroupProps } from "./group-props";
import { JumpButton } from "./jump-button";

const isCommand = (i: RunIntent) => i.shape === "application" || i.workload?.kind === "command";
const harnessesOf = (i: RunIntent): IncludedTool[] => (i.tools ?? []).filter((t) => t.kind === "harness");

/** Autonomous: the agent that runs on its own starts when it has a task. */
const startsAutomatically = (i: RunIntent, id: string, all: IncludedTool[]) =>
  !!i.workload?.task?.trim() && id === (i.workload.agent ?? all[0]?.id);

export function ToolsGroup({ draft, pre, preview, docRows, go, dispatch }: GroupProps) {
  const { intent } = draft;
  const harnesses = harnessesOf(intent);
  const interactive = intent.experience === "interactive";
  const starter = intent.startup?.kind === "harness" ? intent.startup.tool : undefined;
  const command = isCommand(intent);
  const n = harnesses.length;
  // The harness's display name from the roster; its id until the roster answers, or if it never does.
  const roster = useLoaded(loadRoster);
  const name = (id: string) => (roster.status === "ready" ? harnessName(roster.value, id) : id);
  const providerOf = (t?: IncludedTool) =>
    t?.model_provider ?? pre?.harness_set?.harnesses.find((h) => h.id === t?.id)?.model_credential?.provider ?? pre?.model_credential?.provider;

  let detail: string;
  if (command) detail = n === 1 ? NR_REVIEW.TOOLS_COMMAND_ONE : NR_REVIEW.TOOLS_COMMAND(n);
  else if (n === 0) detail = NR_REVIEW.TOOLS_NONE;
  else if (interactive) {
    const t = harnesses.find((h) => h.id === starter);
    const provider = providerOf(t);
    const starts = t ? name(t.id) : NR_REVIEW.TOOLS_NO_HARNESS;
    detail = provider ? NR_REVIEW.TOOLS_STARTS(starts, provider) : NR_SHARED.JOIN(starts, NR_REVIEW.HARNESS_STARTS_LONG);
  } else detail = NR_REVIEW.TOOLS_AUTO(harnesses.filter((h) => startsAutomatically(intent, h.id, harnesses)).length, n);

  const value = command ? <code className="font-mono text-sm">{intent.workload?.command}</code> : n ? harnesses.map((h) => name(h.id)).reduce((all, next) => NR_SHARED.JOIN(all, next)) : NR_REVIEW.NO_HARNESSES;

  return (
    <ReviewGroup
      id="tools"
      label={NR_REVIEW.GROUP_TOOLS}
      value={value}
      detail={detail}
      provenance={
        (command ? undefined : serverSource({ draft, pre, preview }, { kind: "model_provider" })) ??
        draftSource(draft, command ? ["workload.command"] : ["tools", "startup", "workload.task"], NR_REVIEW.PROV_ADDED)
      }
      docRows={docRows}
      editor={(close) => command ? (
        <>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="review-command">{NR_REVIEW.COMMAND_LABEL}</Label>
            <Input id="review-command" className="font-mono" value={intent.workload?.command ?? ""}
              onChange={(e) => dispatch({ type: "edit", apply: (i) => setCommand(i, e.target.value) })} />
          </div>
          <Button type="button" variant="outline" className="w-fit" onClick={close}>{NR_REVIEW.DONE}</Button>
        </>
      ) : (
        <>
          <JumpButton label={NR_REVIEW.EDIT_HARNESSES} to={stepLocation("harnesses")} go={go} />
          <JumpButton label={NR_REVIEW.EDIT_TOOLS_ACCESS} to={stepLocation("tools")} go={go} />
        </>
      )}
    >
      {n > 0 && (
        <div className="my-1.5 grid gap-1.5">
          {harnesses.map((h) => {
            const starts = interactive ? h.id === starter : startsAutomatically(intent, h.id, harnesses);
            const prompt = interactive ? (starts ? intent.startup?.prompt : undefined) : (starts ? intent.workload?.task : undefined);
            return (
              <div key={h.id} className="grid grid-cols-[minmax(100px,1fr)_auto] items-center gap-x-2 gap-y-1 rounded-md border border-border bg-surface-2 px-2.5 py-2 text-sm">
                <strong className="font-medium">{name(h.id)}</strong>
                <Chip>{interactive ? (starts ? NR_REVIEW.HARNESS_STARTS : NR_REVIEW.HARNESS_AFTER) : (starts ? NR_REVIEW.HARNESS_AUTO : NR_REVIEW.HARNESS_NOT_AUTO)}</Chip>
                <span className="col-span-full min-w-0 break-words text-xs text-muted-foreground">{prompt?.trim() ? prompt : NR_REVIEW.HARNESS_NO_PROMPT}</span>
              </div>
            );
          })}
        </div>
      )}
      {pre?.harness_set?.shared_model_access && <p className="text-sm text-muted-foreground">{NR_REVIEW.SHARED_MODEL_ACCESS}</p>}
    </ReviewGroup>
  );
}
