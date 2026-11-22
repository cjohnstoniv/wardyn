/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The owner's consent screen for a help request (approved board SC, frames consent-*): who asks and why, every
// door off until ticked, the minutes to pick (none chosen to start), where the sandbox runs, the consent words
// with this run's recording answer filled in, and Accept and Decline. Accept stays off until a door and a
// time are chosen. A request older than 15 minutes, or one the server no longer finds, shows the lapsed screen.
import * as React from "react";
import type { RunDetail } from "../../lib/types";
import { HttpError } from "../../lib/api/core";
import { support, type SupportDoor, type SupportGrantView } from "../../lib/api/support";
import { getErrorMessage } from "../../lib/format";
import { Button } from "../ui/button";
import { Checkbox } from "../ui/checkbox";
import { Label } from "../ui/label";
import { RadioGroup, RadioGroupItem } from "../ui/radio-group";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../ui/dialog";
import { RUN_PLACEMENT, runnerLabel } from "../wardyn/copy/runners";
import { SUPPORT_CONSENT, SUPPORT_DOOR, SUPPORT_DOORS, SUPPORT_MINUTES, SUPPORT_RECORDED } from "../wardyn/copy/support";

/** "14:05 UTC": the board's clock for every time on these screens. */
export function utcClock(iso: string | undefined): string {
  const t = iso ? new Date(iso) : null;
  if (!t || Number.isNaN(t.getTime())) return "";
  return `${t.toISOString().slice(11, 16)} UTC`;
}

/** Where the sandbox runs, as the consent screen names it: the owner's computer, or the pool or host. */
function sandboxPlace(run: RunDetail): string {
  if (run.placement === "local") return runnerLabel(run);
  if (run.runner_pool_name) return `${run.runner_pool_name} pool`;
  return run.runner_name || RUN_PLACEMENT.REMOTE;
}

export function SupportConsentDialog({
  run,
  grant,
  recordedDoors,
  onClose,
  onAnswered,
}: {
  run: RunDetail;
  grant: SupportGrantView;
  /** The doors this run records (GET status's recorded_doors), so the recording answer is shown before Accept. */
  recordedDoors: SupportDoor[];
  onClose: () => void;
  /** The grant the accept or decline returned; none when the request had lapsed. */
  onAnswered: (answered?: SupportGrantView) => void;
}) {
  const [doors, setDoors] = React.useState<SupportDoor[]>([]);
  const [minutes, setMinutes] = React.useState<number | null>(null);
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");
  // Lapsed only on the server's word (support_grant_not_found on an answer), never on this browser's clock.
  const [lapsed, setLapsed] = React.useState(false);
  const asked = SUPPORT_DOORS.filter((d) => grant.doors_asked.includes(d));
  const recording = (door: SupportDoor) => (recordedDoors.includes(door) ? SUPPORT_RECORDED.yes : SUPPORT_RECORDED.no);

  const answer = async (accept: boolean) => {
    setBusy(true);
    setError("");
    try {
      const answered = accept
        ? await support.accept(run.id, grant.grant, SUPPORT_DOORS.filter((d) => doors.includes(d)), minutes as number)
        : await support.decline(run.id, grant.grant);
      onAnswered(answered);
    } catch (e) {
      // The request is gone (answered, ended or lapsed): the one thing left to say is that it lapsed.
      if (e instanceof HttpError && e.reason === "support_grant_not_found") setLapsed(true);
      else setError(getErrorMessage(e));
      setBusy(false);
    }
  };

  if (lapsed) {
    return (
      <Dialog open onOpenChange={(o) => !o && onAnswered()}>
        <DialogContent className="sm:max-w-lg" data-testid="support-consent-lapsed">
          <DialogHeader>
            <DialogTitle>{SUPPORT_CONSENT.TITLE(grant.helper)}</DialogTitle>
            <DialogDescription>{SUPPORT_CONSENT.LAPSED}</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => onAnswered()}>
              {SUPPORT_CONSENT.CLOSE}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    );
  }

  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="sm:max-w-xl" data-testid="support-consent">
        <DialogHeader>
          <DialogTitle>{SUPPORT_CONSENT.TITLE(grant.helper)}</DialogTitle>
          <DialogDescription>{SUPPORT_CONSENT.NOTHING_OPENS}</DialogDescription>
          <p className="text-xs text-muted-foreground">{SUPPORT_CONSENT.REQUESTED(utcClock(grant.requested_at))}</p>
        </DialogHeader>

        <section className="space-y-1.5">
          <h3 className="label-eyebrow">{SUPPORT_CONSENT.THEIR_REASON}</h3>
          <p className="whitespace-pre-wrap rounded-lg border border-border bg-muted/40 px-3 py-2.5 text-sm text-foreground [overflow-wrap:anywhere]" data-testid="support-reason">
            {grant.reason}
          </p>
        </section>

        <fieldset className="space-y-3">
          <legend className="text-sm font-medium text-foreground">{SUPPORT_CONSENT.DOORS}</legend>
          <p className="text-xs text-muted-foreground">{SUPPORT_CONSENT.DOORS_HINT}</p>
          {asked.map((d) => (
            <div key={d} className="flex items-start gap-3">
              <Checkbox
                id={`consent-door-${d}`}
                checked={doors.includes(d)}
                onCheckedChange={(on) => setDoors((cur) => (on === true ? [...cur, d] : cur.filter((x) => x !== d)))}
                className="mt-0.5"
              />
              <Label htmlFor={`consent-door-${d}`} className="flex flex-col items-start gap-0.5">
                <span className="text-sm font-medium text-foreground">{SUPPORT_DOOR[d].name}</span>
                <span className="text-xs font-normal text-muted-foreground">{SUPPORT_DOOR[d].help}</span>
              </Label>
            </div>
          ))}
        </fieldset>

        <section className="space-y-2">
          <h3 id="consent-how-long" className="text-sm font-medium text-foreground">
            {SUPPORT_CONSENT.HOW_LONG}
          </h3>
          <RadioGroup
            aria-labelledby="consent-how-long"
            className="flex flex-wrap gap-2"
            value={minutes === null ? "" : String(minutes)}
            onValueChange={(v) => setMinutes(Number(v))}
          >
            {SUPPORT_MINUTES.map((m) => (
              <Label
                key={m}
                htmlFor={`consent-minutes-${m}`}
                className="flex cursor-pointer items-center gap-2 rounded-lg border border-border px-3 py-2 text-sm has-[[data-state=checked]]:border-primary"
              >
                <RadioGroupItem id={`consent-minutes-${m}`} value={String(m)} />
                {SUPPORT_CONSENT.MINUTES(m)}
              </Label>
            ))}
          </RadioGroup>
          <p className="text-xs text-muted-foreground">{SUPPORT_CONSENT.NO_EXTENSION}</p>
        </section>

        <section className="space-y-1">
          <h3 className="label-eyebrow">{SUPPORT_CONSENT.WHERE}</h3>
          <p className="text-sm text-foreground" data-testid="support-place">
            {sandboxPlace(run)}
          </p>
          {run.placement === "local" && <p className="text-xs text-muted-foreground">{SUPPORT_CONSENT.OWN_COMPUTER}</p>}
        </section>

        <div className="space-y-3 rounded-lg border border-border bg-muted/40 px-3 py-3 text-sm leading-relaxed text-foreground" data-testid="support-consent-copy">
          <p>{SUPPORT_CONSENT.RECORDING(recording("terminal"))}</p>
          <p>{SUPPORT_CONSENT.REACH}</p>
        </div>
        {grant.doors_asked.includes("desktop") && <p className="text-sm text-foreground">{SUPPORT_CONSENT.DESKTOP(recording("desktop"))}</p>}
        <p className="text-sm text-muted-foreground">{SUPPORT_CONSENT.ACTIVITY}</p>

        {error && (
          <p role="alert" className="text-sm text-danger">
            {error}
          </p>
        )}
        <DialogFooter>
          <Button variant="outline" disabled={busy} onClick={() => void answer(false)}>
            {SUPPORT_CONSENT.DECLINE}
          </Button>
          <Button disabled={busy || doors.length === 0 || minutes === null} onClick={() => void answer(true)}>
            {SUPPORT_CONSENT.ACCEPT}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
