/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { AlertTriangle } from "lucide-react";
import { ALLOWED_MODELS as AM, MEMBER } from "../../../../../lib/governance-copy";
import { Button } from "../../../../ui/button";
import { Input } from "../../../../ui/input";
import { Label } from "../../../../ui/label";
import { Chip } from "../../../../wardyn/primitives";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { OIDC_SIGN_IN } from "../../../../wardyn/copy/oidc-signin";
import { useShellSetupStatus } from "../../../../wardyn/model-access-context";
import type { IncludedTool } from "../../../../../lib/types/new-run-contract";
import type { RunIntent } from "../../draft/run-intent";
import { commandOf, isServer, setCommand } from "../../lenses/apps-lens";
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

// A sentence with one slot, set around a value shown in mono: the copy stays one string, the model gets its own element.
const SLOT = "\u0001";
function AroundModels({ sentence, models }: { sentence: (slot: string) => string; models: string[] }) {
  const [before, after] = sentence(SLOT).split(SLOT);
  return (
    <>
      {before}
      {models.map((m, i) => (
        <span key={m}>
          {i > 0 && ", "}
          <code className="break-all font-mono text-xs">{m}</code>
        </span>
      ))}
      {after}
    </>
  );
}

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

  // The hosted models the run may ask for, when a limit binds it. A harness whose provider fixed a model off
  // the list is said so before launch; the run is not blocked, since a subscription's model is chosen in the session.
  const allowed = pre?.allowed_models ?? preview?.allowed_models;
  const limited = allowed && allowed.length > 0 ? allowed : undefined;
  const limitedBy = pre?.allowed_models ? pre.allowed_models_unit : preview?.allowed_models_unit;
  const offList = limited
    ? harnesses.flatMap((h) => {
        const cred = pre?.harness_set?.harnesses.find((x) => x.id === h.id)?.model_credential;
        return [cred?.model, cred?.fast_model]
          .filter((m): m is string => !!m && !limited.includes(m))
          .map((model) => ({ id: h.id, provider: providerOf(h) ?? cred?.provider ?? "", model }));
      })
    : [];
  // A provider whose sign-ins are not tied to the person's console sign-in says so where the run is reviewed.
  const { status } = useShellSetupStatus();
  const unbound = [...new Set(harnesses.map((h) => providerOf(h)).filter((id): id is string => !!id))]
    .map((id) => status?.model_providers?.find((p) => p.id === id))
    .filter((p) => p?.sign_in === "oidc" && p.identity_binding === "none")
    .map((p) => p!.name || p!.id);

  let detail: string;
  if (command && n === 0 && isServer(intent)) detail = NR_REVIEW.TOOLS_START_COMMAND;
  else if (command) detail = n === 1 ? NR_REVIEW.TOOLS_COMMAND_ONE : NR_REVIEW.TOOLS_COMMAND(n);
  else if (n === 0) detail = NR_REVIEW.TOOLS_NONE;
  else if (interactive) {
    const t = harnesses.find((h) => h.id === starter);
    // The provider by the name setup status gives it, as the frame names it; its id until that answers.
    const id = providerOf(t);
    const provider = id ? status?.model_providers?.find((p) => p.id === id)?.name || id : id;
    const starts = t ? name(t.id) : NR_REVIEW.TOOLS_NO_HARNESS;
    detail = provider ? NR_REVIEW.TOOLS_STARTS(starts, provider) : NR_SHARED.JOIN(starts, NR_REVIEW.HARNESS_STARTS_LONG);
  } else {
    const auto = harnesses.filter((h) => startsAutomatically(intent, h.id, harnesses)).length;
    detail = n === 1 && auto === 1 ? NR_REVIEW.TOOLS_AUTO_ONE : NR_REVIEW.TOOLS_AUTO(auto, n);
  }

  const value = command ? <code className="font-mono text-sm">{commandOf(intent)}</code> : n ? harnesses.map((h) => name(h.id)).reduce((all, next) => NR_SHARED.JOIN(all, next)) : NR_REVIEW.NO_HARNESSES;

  return (
    <ReviewGroup
      id="tools"
      label={NR_REVIEW.GROUP_TOOLS}
      value={value}
      detail={detail}
      provenance={
        (command ? undefined : serverSource({ draft, pre, preview }, { kind: "model_provider" })) ??
        draftSource(draft, command ? ["workload.command", "startup.command"] : ["tools", "startup", "workload.task"], NR_REVIEW.PROV_ADDED)
      }
      docRows={docRows}
      editor={(close) => command ? (
        <>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="review-command">{NR_REVIEW.COMMAND_LABEL}</Label>
            <Input id="review-command" className="font-mono" value={commandOf(intent) ?? ""}
              onChange={(e) => {
                const command = e.target.value;
                dispatch({ type: "edit", apply: (i) => setCommand(i, command) });
              }} />
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
              <div key={h.id} className="grid grid-cols-[minmax(100px,1fr)_auto] items-center gap-x-2 gap-y-1 rounded-md border border-border bg-surface-2 px-2.5 py-2 text-sm max-[520px]:grid-cols-1 max-[520px]:justify-items-start">
                <strong className="font-medium">{name(h.id)}</strong>
                <Chip>{interactive ? (starts ? NR_REVIEW.HARNESS_STARTS : NR_REVIEW.HARNESS_AFTER) : (starts ? NR_REVIEW.HARNESS_AUTO : NR_REVIEW.HARNESS_NOT_AUTO)}</Chip>
                <span className="col-span-full min-w-0 break-words text-xs text-muted-foreground">{prompt?.trim() ? prompt : NR_REVIEW.HARNESS_NO_PROMPT}</span>
              </div>
            );
          })}
        </div>
      )}
      {limited && (
        <div className="grid gap-1" data-testid="review-allowed-models">
          <p className="text-sm text-foreground">
            <AroundModels sentence={AM.REVIEW_LINE} models={limited} />
          </p>
          {limitedBy && (
            <div>
              <Chip>{MEMBER.GS_CHIP(limitedBy)}</Chip>
            </div>
          )}
          <p className="text-xs text-muted-foreground">{AM.REVIEW_READS}</p>
          {offList.map((o) => (
            <p key={`${o.id}:${o.model}`} className="text-sm text-warning" data-testid="review-allowed-models-warning">
              <AroundModels sentence={(slot) => AM.REVIEW_OFF_LIST(name(o.id), o.provider, slot)} models={[o.model]} />
            </p>
          ))}
        </div>
      )}
      {unbound.map((name) => (
        <p key={name} className="flex items-start gap-1.5 text-sm text-warning">
          <AlertTriangle className="mt-0.5 size-4 shrink-0" aria-hidden="true" />
          {OIDC_SIGN_IN.REVIEW_NOT_BOUND(name)}
        </p>
      ))}
      {pre?.harness_set?.shared_model_access && <p className="text-sm text-muted-foreground">{NR_REVIEW.SHARED_MODEL_ACCESS}</p>}
    </ReviewGroup>
  );
}
