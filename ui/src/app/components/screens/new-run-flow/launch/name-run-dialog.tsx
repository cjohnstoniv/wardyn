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
import { ADO_PAT } from "../../../../lib/ado-pat-copy";
import type { SCMAccessPAT } from "../../../../lib/types/ado-pat";
import type { CreateRunResult } from "../../../../lib/types/run-create";
import { Button } from "../../../ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogTitle } from "../../../ui/dialog";
import { Input } from "../../../ui/input";
import { Label } from "../../../ui/label";
import { Textarea } from "../../../ui/textarea";
import { NR_LAUNCH } from "../../../wardyn/copy/new-run/launch";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { NR_SHARED } from "../../../wardyn/copy/new-run/shared";
import { AdoLaunchNote, AdoOwnTokenNote, adoLaunchNoteKind } from "../../../wardyn/ado-run-token-line";
import { useOwnTokenRows } from "../../../../lib/hooks/use-own-token-rows";
import { useModelAccessDoor, useShellSetupStatus } from "../../../wardyn/model-access-context";
import { PolicyRemedy } from "../../../wardyn/policy-remedy";
import { useAdoLaunchDoor } from "../checks/use-ado-launch-door";
import { intentToWire } from "../draft/intent-to-wire";
import type { RunIntent } from "../draft/run-intent";
import { useDraftIdentity, useIntentChecks } from "../draft/use-intent-checks";
import { useNewRunDispatch, useNewRunDraft } from "../new-run-route";
import { BarrierChip } from "../review/barrier-chip";
import { cpuNumber, resolvedLifetime, resolvedLifetimeText } from "../review/format";
import type { ReviewPreflight } from "../review/review-seams";
import { AdoLaunchDialog } from "./ado-launch-dialog";
import { useLaunchRun, type PendingLaunch } from "./use-launch-run";

// Only the manual check runs here: nothing is asked again while the person types.
const MANUAL_ONLY = { local: false, backendArm: false, modelArm: false };

/**
 * pool · CPU · lifetime, from what the server just answered; a part it did not give is left out.
 * The lifetime is the resolved end: no end when asked for and allowed, else the ask held to the answer's cap,
 * or the answer's default when nothing was asked.
 */
export function summaryLine(pre: ReviewPreflight, intent: RunIntent): string {
  const pool = pre.runner_pool?.name;
  const cpuMillis = pre.resources_applied?.cpu_millis.applied;
  const lifetimeLimit = pre.limits?.lifetime_sec;
  const cpu = cpuMillis === undefined ? undefined : cpuNumber(cpuMillis);
  const lifetime = lifetimeLimit ? resolvedLifetimeText(resolvedLifetime(intent, pre.limits)) : undefined;
  if (pool && cpu !== undefined && lifetime !== undefined) return NR_LAUNCH.SUMMARY(pool, cpu, lifetime);
  const parts = [pool, cpu === undefined ? undefined : NR_SHARED.CPU(cpu), lifetime].filter((p): p is string => !!p);
  return parts.length ? parts.reduce((all, part) => NR_SHARED.JOIN(all, part)) : "";
}

export interface NameRunDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** The create answered 2xx: the caller moves to the Launched screen. */
  onCreated: (created: CreateRunResult) => void;
  /** The create in flight, held by the dialog's owner so that closing and reopening cannot start a second. */
  pending?: PendingLaunch;
}

export function NameRunDialog({ open, onOpenChange, onCreated, pending }: NameRunDialogProps) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        onOpenAutoFocus={(e) => {
          // Input forwards no ref under React 18, so the first field is found by id.
          e.preventDefault();
          (e.currentTarget as HTMLElement).querySelector<HTMLInputElement>("#name-run-title")?.focus();
        }}
      >
        <NameRunForm onOpenChange={onOpenChange} onCreated={onCreated} pending={pending} />
      </DialogContent>
    </Dialog>
  );
}

function NameRunForm({ onOpenChange, onCreated, pending }: Pick<NameRunDialogProps, "onOpenChange" | "onCreated" | "pending">) {
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
  // The Azure DevOps door, from its own dialog or from the note under a refusal: once it is done, ask again.
  const adoBusy = adoDoor.dialog.open || adoDoor.dialog.connecting;
  const wasAdoBusy = React.useRef(false);
  React.useEffect(() => {
    if (wasAdoBusy.current && !adoBusy) void check();
    wasAdoBusy.current = adoBusy;
  }, [adoBusy, check]);
  const { status } = useShellSetupStatus();
  const ownRows = useOwnTokenRows(status);
  const adoAccess = status?.unreachable ? undefined : (status?.scm_access as SCMAccessPAT | undefined);

  const refused = (e: unknown) => {
    adoDoor.notifyLaunchError(e);
    if (!isCredentialRefusal(e)) return;
    // The door of the provider the refusal names, never the one on screen.
    if (e instanceof HttpError && e.provider) {
      modelDoor.openDoor({ for: { provider: e.provider }, returnTo: launchButton.current, onSignedIn: () => void check() });
    }
    void modelDoor.refresh();
  };
  const create = useLaunchRun(identity, onCreated, refused, pending);

  const pre = checks.preflightIsCurrent ? (checks.preflightResult as ReviewPreflight | null) : null;
  const answered = pre !== null;
  const checking = checks.preflighting || (!answered && checks.preflightError === null && !checks.preflightNotChecked);
  const sentence = create.refusal ?? (checking ? null : checks.preflightError);
  // The note for a launch the token refused. When the server's sentence is the note's own, it is said once.
  const adoNote = create.refusal && adoDoor.refusal ? adoLaunchNoteKind(adoAccess, adoDoor.refusal) : null;
  const refusalIsAdoNote = adoNote === "blocked" && sentence === ADO_PAT.LAUNCH_POLICY_REFUSED;
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
                <BarrierChip cc={pre.enforced_confinement_class} />
              </div>
            </>
          )}
          {!checking && !answered && checks.preflightError !== null && (
            <Button type="button" variant="outline" size="sm" className="w-fit" onClick={() => void check()}>
              {NR_REVIEW.CHECK_AGAIN}
            </Button>
          )}
        </div>

        {sentence && !refusalIsAdoNote && (
          <p id="name-run-sentence" role="alert" className="text-sm text-danger">
            {sentence}
            {create.refusal && <PolicyRemedy policy={create.policy} className="mt-1 block" />}
          </p>
        )}
        {/* A launch the run's Azure DevOps token refused: the organisation blocks it, or the person connects again. */}
        {/* Several organisations each take their own token: say which ones a refused launch is missing one for. */}
        {sentence && !adoNote && <AdoOwnTokenNote rows={ownRows} />}
        {adoNote && (
          <div id={refusalIsAdoNote ? "name-run-sentence" : undefined} className={refusalIsAdoNote ? "text-danger" : undefined}>
            <AdoLaunchNote
              access={adoAccess}
              refusal={adoDoor.refusal}
              connecting={adoDoor.dialog.connecting}
              onConnect={adoDoor.dialog.onConfirm}
              blockedUrl={adoDoor.dialog.open ? null : adoDoor.dialog.blockedUrl}
              onFallbackClick={adoDoor.dialog.onFallbackClick}
            />
          </div>
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
