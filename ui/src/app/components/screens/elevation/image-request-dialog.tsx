/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The image request dialog: a person asks to launch one image, named by its digest, and the request waits for
// the deciders of their default profile. The console checks nothing about the reference: the server's refusal
// shows under the field. Closing drops everything typed, so it always opens fresh.
import * as React from "react";
import { Check } from "lucide-react";
import { HttpError } from "../../../lib/api/core";
import { requests } from "../../../lib/api/governance-requests";
import { absoluteTime, getErrorMessage } from "../../../lib/format";
import type { GovernanceRequest } from "../../../lib/types/governance-requests";
import { Button } from "../../ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../../ui/dialog";
import { Input } from "../../ui/input";
import { Textarea } from "../../ui/textarea";
import { ELEV } from "../../wardyn/copy/elevation";
import { IMG } from "../../wardyn/copy/image-request";
import { Field } from "../../wardyn/form-primitives";
import { Chip } from "../../wardyn/primitives";
import { Note } from "../governance/display";
import { decidersPhrase } from "./request-phrases";

// A full reference inside a sentence breaks anywhere; the rest of the sentence wraps at words.
const REFERENCE = /(\S+@sha256:[0-9a-f]{64})/;
function sentence(text: string): React.ReactNode {
  return text.split(REFERENCE).map((part, i) =>
    i % 2 ? (
      <span key={i} className="break-all">
        {part}
      </span>
    ) : (
      part
    ),
  );
}

export function ImageRequestDialog({
  open,
  onOpenChange,
  kindChoice,
  onSent,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  kindChoice?: React.ReactNode;
  onSent?: (r: GovernanceRequest) => void;
}) {
  // A request that is out cannot be walked away from: its answer would have nowhere to show.
  const busy = React.useRef(false);
  return (
    <Dialog open={open} onOpenChange={(o) => !(busy.current && !o) && onOpenChange(o)}>
      <DialogContent className="sm:max-w-lg">
        {/* Mounted only while the dialog is open, so every opening starts from the first state. */}
        <RequestForm busy={busy} kindChoice={kindChoice} onClose={() => onOpenChange(false)} onSent={onSent} />
      </DialogContent>
    </Dialog>
  );
}

function RequestForm({
  busy,
  kindChoice,
  onClose,
  onSent,
}: {
  busy: React.RefObject<boolean>;
  kindChoice?: React.ReactNode;
  onClose: () => void;
  onSent?: (r: GovernanceRequest) => void;
}) {
  const uid = React.useId();
  const imageId = `${uid}-image`;
  const reasonId = `${uid}-reason`;
  const ticketId = `${uid}-ticket`;

  const [image, setImage] = React.useState("");
  const [reason, setReason] = React.useState("");
  const [ticket, setTicket] = React.useState("");
  const [sending, setSending] = React.useState(false);
  const [sent, setSent] = React.useState<GovernanceRequest | null>(null);
  const [imageError, setImageError] = React.useState<string | null>(null);
  const [refusal, setRefusal] = React.useState<string | null>(null);

  const canSend = !sending && image.trim() !== "" && reason.trim() !== "";

  function refuse(e: unknown) {
    if (!(e instanceof HttpError)) return setRefusal(getErrorMessage(e));
    if (e.reason === "image_request_not_pinned") return setImageError(e.message);
    if (e.reason === "governance_change_pending") return setRefusal(IMG.ALREADY_WAITING);
    setRefusal(e.message);
  }

  async function send() {
    busy.current = true;
    setSending(true);
    setImageError(null);
    setRefusal(null);
    const note = ticket.trim();
    try {
      const r = await requests.create({
        kind: "image",
        image: image.trim(),
        reason: reason.trim(),
        ...(note ? { ticket: note } : {}),
      });
      setSent(r);
      onSent?.(r);
    } catch (e) {
      refuse(e);
    } finally {
      busy.current = false;
      setSending(false);
    }
  }

  return (
    <>
      <DialogHeader>
        <DialogTitle>{IMG.TITLE}</DialogTitle>
        <DialogDescription className={sent ? "sr-only" : undefined}>{IMG.LEAD}</DialogDescription>
      </DialogHeader>

      {!sent && kindChoice}

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
      ) : (
        <div className="space-y-4">
          <Field
            label={IMG.IMAGE_LABEL}
            htmlFor={imageId}
            required
            hint={imageError ? <span className="text-danger">{imageError}</span> : IMG.IMAGE_HINT}
          >
            {/* A digest is long, so the field wraps it onto a second line rather than clipping it. */}
            <Textarea
              id={imageId}
              required
              rows={2}
              spellCheck={false}
              autoCapitalize="off"
              autoCorrect="off"
              className="min-h-0 resize-none break-all font-mono"
              placeholder={IMG.IMAGE_PLACEHOLDER}
              value={image}
              disabled={sending}
              aria-invalid={imageError ? true : undefined}
              onKeyDown={(e) => e.key === "Enter" && e.preventDefault()}
              onChange={(e) => setImage(e.target.value.replace(/\s+/g, ""))}
            />
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
              placeholder={IMG.REASON_PLACEHOLDER}
              value={reason}
              disabled={sending}
              onChange={(e) => setReason(e.target.value)}
            />
          </Field>

          <Field label={ELEV.TICKET_LABEL} htmlFor={ticketId} hint={ELEV.TICKET_HINT}>
            <Input
              id={ticketId}
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
              <span>{sentence(refusal)}</span>
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
              {sending ? ELEV.SENDING : IMG.SEND}
            </Button>
          </>
        )}
      </DialogFooter>
    </>
  );
}
