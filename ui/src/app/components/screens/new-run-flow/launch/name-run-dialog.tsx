/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Name this run: the last step before the create. Opening it asks the server one
// fresh question, and "Checked just now" and the summary under it come only from
// that answer. A create the check did not foresee is shown here in the server's
// own sentence, with the typed title and description kept. A credential refusal
// opens the existing door for it; signing in re-checks, and Launch is pressed again.

import * as React from "react";
import { Check, Loader2 } from "lucide-react";
import { HttpError } from "../../../../lib/api/core";
import { isCredentialRefusal } from "../../../../lib/api/runs";
import type { CreateRunResult } from "../../../../lib/types/run-create";
import { Button } from "../../../ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogTitle } from "../../../ui/dialog";
import { Input } from "../../../ui/input";
import { Label } from "../../../ui/label";
import { Textarea } from "../../../ui/textarea";
import { NR_LAUNCH } from "../../../wardyn/copy/new-run/launch";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { NR_SHARED } from "../../../wardyn/copy/new-run/shared";
import { BarrierMark } from "../../../wardyn/barrier-mark";
import { useModelAccessDoor } from "../../../wardyn/model-access-context";
import { useAdoLaunchDoor } from "../../new-run/use-ado-launch-door";
import { intentToWire } from "../draft/intent-to-wire";
import type { RunIntent } from "../draft/run-intent";
import { useDraftIdentity, useIntentChecks } from "../draft/use-intent-checks";
import { useNewRunDispatch, useNewRunDraft } from "../new-run-route";
import { cpuNumber, lifetimeText } from "../review/format";
import type { ReviewPreflight } from "../review/review-seams";
import { AdoLaunchDialog } from "./ado-launch-dialog";
import { useLaunchRun } from "./use-launch-run";

// Only the manual check runs here: nothing is asked again while the person types.
const MANUAL_ONLY = { local: false, backendArm: false, modelArm: false };

/**
 * pool · CPU · lifetime, from what the server just answered; a part it did not give is left out.
 * The lifetime is the ask held to the answer's cap, or the answer's default when nothing was asked.
 */
export function summaryLine(pre: ReviewPreflight, intent: RunIntent): string {
  const pool = pre.runner_pool?.name;
  const cpuMillis = pre.resources_applied?.cpu_millis.applied;
  const lifetimeLimit = pre.limits?.lifetime_sec;
  const wanted = intent.ends_after_sec ?? (lifetimeLimit?.default.unlimited ? undefined : lifetimeLimit?.default.value);
  const seconds = wanted === undefined || !lifetimeLimit || lifetimeLimit.cap.unlimited ? wanted : Math.min(wanted, lifetimeLimit.cap.value);
  const cpu = cpuMillis === undefined ? undefined : cpuNumber(cpuMillis);
  const lifetime = !lifetimeLimit || seconds === undefined ? undefined : lifetimeText(seconds);
  if (pool && cpu !== undefined && lifetime !== undefined) return NR_LAUNCH.SUMMARY(pool, cpu, lifetime);
  const parts = [pool, cpu === undefined ? undefined : NR_SHARED.CPU(cpu), lifetime].filter((p): p is string => !!p);
  return parts.length ? parts.reduce((all, part) => NR_SHARED.JOIN(all, part)) : "";
}

export interface NameRunDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** The create answered 2xx: the caller moves to the Launched screen. */
  onCreated: (created: CreateRunResult) => void;
}

export function NameRunDialog({ open, onOpenChange, onCreated }: NameRunDialogProps) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        onOpenAutoFocus={(e) => {
          // Input forwards no ref under React 18, so the first field is found by id.
          e.preventDefault();
          (e.currentTarget as HTMLElement).querySelector<HTMLInputElement>("#name-run-title")?.focus();
        }}
      >
        <NameRunForm onOpenChange={onOpenChange} onCreated={onCreated} />
      </DialogContent>
    </Dialog>
  );
}

function NameRunForm({ onOpenChange, onCreated }: Pick<NameRunDialogProps, "onOpenChange" | "onCreated">) {
  const draft = useNewRunDraft();
  const dispatch = useNewRunDispatch();
  const identity = useDraftIdentity();
  const modelDoor = useModelAccessDoor();
  const adoDoor = useAdoLaunchDoor();
  const launchButton = React.useRef<HTMLButtonElement>(null);
  const { title, description } = draft.launch;

  const wire = React.useMemo(() => intentToWire(draft.intent), [draft.intent]);
  const checks = useIntentChecks({
    body: wire.body,
    selectionKey: "",
    identity,
    externalRevision: "",
    sourceRefreshPending: false,
    autoCheck: MANUAL_ONLY,
    doorOpen: false,
    adoDoorOpen: false,
  });
  const check = checks.preflight;
  // Opening the dialog is the one question; the answer on Review is never reused.
  React.useEffect(() => {
    void check();
  }, [check]);
  const adoOpen = adoDoor.dialog.open;
  const wasAdoOpen = React.useRef(false);
  React.useEffect(() => {
    if (wasAdoOpen.current && !adoOpen) void check();
    wasAdoOpen.current = adoOpen;
  }, [adoOpen, check]);

  const refused = (e: unknown) => {
    adoDoor.notifyLaunchError(e);
    if (!isCredentialRefusal(e)) return;
    // The door of the provider the refusal names, never the one on screen.
    if (e instanceof HttpError && e.provider) {
      modelDoor.openDoor({ for: { provider: e.provider }, returnTo: launchButton.current, onSignedIn: () => void check() });
    }
    void modelDoor.refresh();
  };
  const create = useLaunchRun(identity, onCreated, refused);

  const pre = checks.preflightIsCurrent ? (checks.preflightResult as ReviewPreflight | null) : null;
  const answered = pre !== null;
  const checking = checks.preflighting || (!answered && checks.preflightError === null && !checks.preflightNotChecked);
  const sentence = create.refusal ?? (checking ? null : checks.preflightError);
  const named = title.trim() !== "";
  const canLaunch = named && !checking && !create.creating && !checks.preflightBlock;
  // The visible reason a held Launch is held: the missing title, the check in flight, or the refusal.
  const heldBy = canLaunch || create.creating ? undefined : !named ? "name-run-required" : checking ? "name-run-checking" : sentence ? "name-run-sentence" : undefined;

  return (
    <>
      <form
        className="grid gap-4"
        aria-busy={create.creating}
        onSubmit={(e) => {
          e.preventDefault();
          if (canLaunch) void create.launch(intentToWire(draft.intent, { title: title.trim(), description: description.trim() }).body);
        }}
      >
        <DialogTitle>{NR_LAUNCH.TITLE}</DialogTitle>
        <DialogDescription>{NR_LAUNCH.INTRO}</DialogDescription>

        <div className="space-y-2">
          <div className="flex items-center gap-2">
            <Label htmlFor="name-run-title">{NR_LAUNCH.TITLE_LABEL}</Label>
            <span id="name-run-required" className="text-xs text-muted-foreground">{NR_SHARED.REQUIRED}</span>
          </div>
          <Input
            id="name-run-title"
            value={title}
            placeholder={NR_LAUNCH.TITLE_PLACEHOLDER}
            onChange={(e) => dispatch({ type: "launchFields", patch: { title: e.target.value } })}
          />
        </div>

        <div className="space-y-2">
          <div className="flex items-center gap-2">
            <Label htmlFor="name-run-description">{NR_LAUNCH.DESCRIPTION_LABEL}</Label>
            <span className="text-xs text-muted-foreground">{NR_SHARED.OPTIONAL}</span>
          </div>
          <Textarea
            id="name-run-description"
            value={description}
            placeholder={NR_LAUNCH.DESCRIPTION_PLACEHOLDER}
            onChange={(e) => dispatch({ type: "launchFields", patch: { description: e.target.value } })}
          />
        </div>

        <div
          className={!checking && answered ? "flex gap-3 rounded-lg border border-success/25 bg-success-subtle p-3" : "flex flex-col gap-2 rounded-md border border-border p-3"}
          data-testid="name-run-summary"
        >
          {checking && <p id="name-run-checking" role="status" className="text-sm text-muted-foreground">{NR_LAUNCH.CHECKING}</p>}
          {!checking && answered && (
            <>
              <Check aria-hidden="true" className="mt-0.5 size-4 shrink-0 text-success" />
              <div className="flex min-w-0 flex-1 flex-wrap items-center justify-between gap-3">
                <p className="flex flex-col gap-0.5 text-sm">
                  <strong>{NR_LAUNCH.CHECKED_JUST_NOW}</strong>
                  <span className="text-muted-foreground">{summaryLine(pre, draft.intent)}</span>
                </p>
                <BarrierMark cc={pre.enforced_confinement_class} size="sm" />
              </div>
            </>
          )}
          {!checking && !answered && checks.preflightError !== null && (
            <Button type="button" variant="outline" size="sm" className="w-fit" onClick={() => void check()}>
              {NR_REVIEW.CHECK_AGAIN}
            </Button>
          )}
        </div>

        {sentence && (
          <p id="name-run-sentence" role="alert" className="text-sm text-danger">
            {sentence}
          </p>
        )}

        <DialogFooter>
          <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
            {NR_LAUNCH.CANCEL}
          </Button>
          <Button ref={launchButton} type="submit" disabled={!canLaunch} aria-describedby={heldBy}>
            <Loader2 className={create.spinning ? "size-4 animate-spin" : "hidden"} />
            {NR_LAUNCH.LAUNCH}
          </Button>
        </DialogFooter>
      </form>
    <AdoLaunchDialog dialog={adoDoor.dialog} />
    </>
  );
}
