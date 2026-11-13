/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The elevation request dialog: a person asks for time-limited membership of a unit that offers it, and the
// request waits for the unit's deciders. A screen mounts it and decides when to offer it: the units come in as
// `offers`, or the dialog reads them itself on open. Closing drops everything typed, so it always opens fresh.
import * as React from "react";
import { Check } from "lucide-react";
import { HttpError } from "../../../lib/api/core";
import { requests } from "../../../lib/api/governance-requests";
import { absoluteTime, getErrorMessage } from "../../../lib/format";
import { goDuration, goDurationMs } from "../../../lib/go-duration";
import type { ElevationOffer, GovernanceRequest } from "../../../lib/types/governance-requests";
import { Button } from "../../ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../../ui/dialog";
import { Input } from "../../ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../../ui/select";
import { Textarea } from "../../ui/textarea";
import { cn } from "../../ui/utils";
import { ELEV } from "../../wardyn/copy/elevation";
import { DurationField } from "../../wardyn/duration-field";
import { Field, fieldHintId } from "../../wardyn/form-primitives";
import { Chip } from "../../wardyn/primitives";
import { Note } from "../governance/display";
import { decidersPhrase, lengthWords } from "./request-phrases";

const HOUR_MS = 3_600_000;
const MINUTE_MS = 60_000;
const PRESET_HOURS = [1, 4, 8] as const;
const CUSTOM_UNITS = [
  { value: "minutes", label: ELEV.MINUTES },
  { value: "hours", label: ELEV.HOURS },
];

type Length = number | "custom";
type Units = { status: "loading" } | { status: "failed" } | { status: "ready"; offers: ElevationOffer[] };
type Refusal = { text: string; mono: boolean };

export function ElevationRequestDialog({
  open,
  onOpenChange,
  unitId,
  offers,
  kindChoice,
  onSent,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  unitId?: string;
  offers?: ElevationOffer[];
  // The choice between kinds, when a screen offers more than one: drawn between the lead and the first field.
  kindChoice?: React.ReactNode;
  onSent?: (r: GovernanceRequest) => void;
}) {
  // A request that is out cannot be walked away from: its answer would have nowhere to show.
  const busy = React.useRef(false);
  return (
    <Dialog open={open} onOpenChange={(o) => !(busy.current && !o) && onOpenChange(o)}>
      <DialogContent className="sm:max-w-lg">
        {/* Mounted only while the dialog is open, so every opening starts from the first state. */}
        <RequestForm busy={busy} unitId={unitId} given={offers} kindChoice={kindChoice} onClose={() => onOpenChange(false)} onSent={onSent} />
      </DialogContent>
    </Dialog>
  );
}

function RequestForm({
  busy,
  unitId,
  given,
  kindChoice,
  onClose,
  onSent,
}: {
  busy: React.RefObject<boolean>;
  unitId?: string;
  given?: ElevationOffer[];
  kindChoice?: React.ReactNode;
  onClose: () => void;
  onSent?: (r: GovernanceRequest) => void;
}) {
  const uid = React.useId();
  const unitFieldId = `${uid}-unit`;
  const lengthFieldId = `${uid}-length`;
  const reasonId = `${uid}-reason`;
  const ticketId = `${uid}-ticket`;

  const [loaded, setLoaded] = React.useState<Units>({ status: "loading" });
  const [tries, setTries] = React.useState(0);
  React.useEffect(() => {
    if (given) return;
    let live = true;
    requests.list().then(
      (list) => live && setLoaded({ status: "ready", offers: list?.offered ?? [] }),
      () => live && setLoaded({ status: "failed" }),
    );
    return () => {
      live = false;
    };
  }, [given, tries]);
  const units: Units = given ? { status: "ready", offers: given } : loaded;
  const offerList = units.status === "ready" ? units.offers : [];

  const [pickedUnit, setPickedUnit] = React.useState("");
  const offer = offerList.find((o) => o.unit_id === pickedUnit) ?? offerList.find((o) => o.unit_id === unitId) ?? offerList[0];

  // The longest preset the unit allows, or Custom when none fits. A pick the unit's maximum rules out (after a
  // change of unit) falls back to that.
  const [choice, setChoice] = React.useState<Length | null>(null);
  const [customCount, setCustomCount] = React.useState("");
  const [customUnit, setCustomUnit] = React.useState("minutes");
  const maxMs = offer ? goDurationMs(offer.max_elevation) : NaN;
  const fits = (h: number) => h * HOUR_MS <= maxMs;
  const longest: Length = [...PRESET_HOURS].reverse().find(fits) ?? "custom";
  const length: Length = choice === null || (typeof choice === "number" && !fits(choice)) ? longest : choice;
  const duration =
    length === "custom"
      ? goDuration(Math.max(0, Number(customCount) || 0) * (customUnit === "hours" ? HOUR_MS : MINUTE_MS))
      : goDuration(length * HOUR_MS);

  const [reason, setReason] = React.useState("");
  const [ticket, setTicket] = React.useState("");
  const ticketRequired = !!offer?.ticket_required;

  const [sending, setSending] = React.useState(false);
  const [sent, setSent] = React.useState<GovernanceRequest | null>(null);
  const [lengthError, setLengthError] = React.useState<string | null>(null);
  const [refusal, setRefusal] = React.useState<Refusal | null>(null);

  const canSend = !!offer && !sending && reason.trim() !== "" && (!ticketRequired || ticket.trim() !== "");

  function refuse(e: unknown, o: ElevationOffer) {
    if (!(e instanceof HttpError)) return setRefusal({ text: getErrorMessage(e), mono: false });
    if (e.reason === "elevation_too_long" || e.reason === "elevation_duration_invalid") return setLengthError(e.message);
    if (e.reason === "governance_change_pending") return setRefusal({ text: ELEV.ALREADY_WAITING(o.unit_name), mono: false });
    if (e.reason === "elevation_not_offered") return setRefusal({ text: ELEV.NOT_OFFERED(o.unit_name), mono: false });
    setRefusal({ text: e.message, mono: true });
  }

  async function send() {
    if (!offer) return;
    busy.current = true;
    setSending(true);
    setLengthError(null);
    setRefusal(null);
    const note = ticket.trim();
    try {
      const r = await requests.create({
        kind: "elevation",
        unit_id: offer.unit_id,
        duration,
        reason: reason.trim(),
        ...(note ? { ticket: note } : {}),
      });
      setSent(r);
      onSent?.(r);
    } catch (e) {
      refuse(e, offer);
    } finally {
      busy.current = false;
      setSending(false);
    }
  }

  function retry() {
    setLoaded({ status: "loading" });
    setTries((n) => n + 1);
  }

  const presets = [
    ...PRESET_HOURS.map((h, i) => ({
      value: h as Length,
      label: ELEV.PRESETS[i],
      over: offer && !fits(h) ? ELEV.PRESET_OVER(offer.unit_name, lengthWords(offer.max_elevation)) : "",
    })),
    { value: "custom" as Length, label: ELEV.PRESETS[3], over: "" },
  ];
  const lengthHint = offer ? ELEV.CUSTOM_HINT(lengthWords(offer.max_elevation), offer.unit_name) : undefined;

  return (
    <>
      <DialogHeader>
        <DialogTitle>{ELEV.TITLE}</DialogTitle>
        <DialogDescription className={sent ? "sr-only" : undefined}>{ELEV.LEAD}</DialogDescription>
      </DialogHeader>

      {/* Picking the other kind while a request is out would close this form before its answer shows. */}
      {!sent && kindChoice && (
        <fieldset disabled={sending} className="m-0 min-w-0 border-0 p-0">
          {kindChoice}
        </fieldset>
      )}

      {sent ? (
        <div className="flex items-start gap-3">
          <Chip tone="success" className="size-9 shrink-0 justify-center rounded-lg p-0">
            <Check className="size-4" aria-hidden="true" />
          </Chip>
          <div className="space-y-1">
            <p className="text-sm font-medium text-foreground">{ELEV.SENT_TITLE}</p>
            <p className="text-sm text-muted-foreground">
              {ELEV.SENT_BODY(decidersPhrase(sent.deciders ?? []), absoluteTime(sent.expires_at))}
            </p>
          </div>
        </div>
      ) : units.status === "failed" || (units.status === "ready" && !offer) ? (
        <Note tone="red" role="alert">
          <span className="font-semibold">{ELEV.UNITS_FAILED_TITLE}</span>
          <span>{units.status === "failed" ? ELEV.UNITS_FAILED_BODY : ELEV.NOT_OFFERED_ANY}</span>
          {!given && (
            <Button type="button" variant="outline" size="sm" className="mt-1 w-fit" onClick={retry}>
              {ELEV.RETRY}
            </Button>
          )}
        </Note>
      ) : (
        <div className="space-y-4">
          <Field label={ELEV.UNIT_LABEL} htmlFor={unitFieldId} hint={ELEV.UNIT_HINT} required>
            <Select value={offer?.unit_id} onValueChange={setPickedUnit} disabled={!offer || sending}>
              <SelectTrigger id={unitFieldId} aria-describedby={fieldHintId(unitFieldId)}>
                <SelectValue placeholder={ELEV.LOADING_UNITS} />
              </SelectTrigger>
              <SelectContent>
                {offerList.map((o) => (
                  <SelectItem key={o.unit_id} value={o.unit_id}>
                    {o.unit_name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </Field>

          <Field
            label={ELEV.LENGTH_LABEL}
            htmlFor={lengthFieldId}
            required
            hint={lengthError ? <span className="text-danger">{lengthError}</span> : lengthHint}
          >
            <div className="space-y-2">
              <div role="radiogroup" aria-label={ELEV.LENGTH_LABEL} className="inline-flex overflow-hidden rounded-md border border-border text-xs">
                {presets.map((p) => (
                  <React.Fragment key={String(p.value)}>
                    <label className={p.over ? "cursor-not-allowed" : "cursor-pointer"} title={p.over || undefined}>
                      <input
                        type="radio"
                        className="peer sr-only"
                        name={`${uid}-length`}
                        checked={length === p.value}
                        disabled={!!p.over || sending}
                        aria-describedby={p.over ? `${uid}-over-${p.value}` : undefined}
                        onChange={() => setChoice(p.value)}
                      />
                      <span className="block px-3 py-1.5 text-muted-foreground peer-checked:bg-primary peer-checked:text-primary-foreground peer-focus-visible:ring-2 peer-focus-visible:ring-ring peer-disabled:cursor-not-allowed peer-disabled:opacity-60">
                        {p.label}
                      </span>
                    </label>
                    {p.over && (
                      <span id={`${uid}-over-${p.value}`} className="sr-only">
                        {p.over}
                      </span>
                    )}
                  </React.Fragment>
                ))}
              </div>
              {length === "custom" && (
                <DurationField
                  id={lengthFieldId}
                  count={customCount}
                  unit={customUnit}
                  units={CUSTOM_UNITS}
                  onCount={setCustomCount}
                  onUnit={setCustomUnit}
                  countLabel={ELEV.CUSTOM_COUNT_LABEL}
                  unitLabel={ELEV.CUSTOM_UNIT_LABEL}
                  disabled={sending}
                  invalid={!!lengthError}
                  describedBy={fieldHintId(lengthFieldId)}
                  inputClassName="max-w-[6rem]"
                />
              )}
            </div>
          </Field>

          <Field
            label={ELEV.REASON_LABEL}
            htmlFor={reasonId}
            required
            hint={
              <span className="flex justify-between gap-2">
                <span>{ELEV.REASON_HINT}</span>
                <span className="shrink-0">{ELEV.COUNT(reason.length)}</span>
              </span>
            }
          >
            <Textarea
              id={reasonId}
              required
              maxLength={500}
              rows={3}
              placeholder={ELEV.REASON_PLACEHOLDER}
              value={reason}
              disabled={sending}
              onChange={(e) => setReason(e.target.value)}
            />
          </Field>

          <Field
            label={ticketRequired ? ELEV.TICKET_REQUIRED_LABEL : ELEV.TICKET_LABEL}
            htmlFor={ticketId}
            required={ticketRequired}
            hint={ELEV.TICKET_HINT}
          >
            <Input
              id={ticketId}
              required={ticketRequired}
              maxLength={128}
              className="font-mono"
              placeholder={ELEV.TICKET_PLACEHOLDER}
              value={ticket}
              disabled={sending}
              onChange={(e) => setTicket(e.target.value)}
            />
          </Field>

          {refusal && (
            <Note tone="red" role="alert">
              <span className="font-semibold">{ELEV.REFUSED_TITLE}</span>
              <span className={cn(refusal.mono && "font-mono")}>{refusal.text}</span>
            </Note>
          )}
        </div>
      )}

      <DialogFooter>
        {sent ? (
          <Button type="button" onClick={onClose}>
            {ELEV.DONE}
          </Button>
        ) : (
          <>
            <Button type="button" variant="outline" disabled={sending} onClick={onClose}>
              {ELEV.CANCEL}
            </Button>
            <Button type="button" disabled={!canSend} onClick={send}>
              {sending ? ELEV.SENDING : ELEV.SEND}
            </Button>
          </>
        )}
      </DialogFooter>
    </>
  );
}
