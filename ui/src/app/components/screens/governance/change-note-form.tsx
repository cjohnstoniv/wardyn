/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The note form of a held change: its reason and its ticket. The change's drawer and the held-save note open
// this one form. The PUT replaces the note, and both fields empty clears it. The console only caps the
// fields; the server's sentence says why a note it refuses is refused.
import * as React from "react";
import { Loader2 } from "lucide-react";
import { toast } from "sonner";
import { HttpError } from "../../../lib/api/core";
import { governance as api } from "../../../lib/api/governance";
import { getErrorMessage } from "../../../lib/format";
import type { GovernanceChange } from "../../../lib/types";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";
import { Textarea } from "../../ui/textarea";
import { Mono } from "../../wardyn/code-block";
import { CHANGE_NOTE } from "../../wardyn/copy/change-note";
import { Field } from "../../wardyn/form-primitives";
import { Note } from "./display";

export function ChangeNoteForm({
  change,
  onSaved,
  onCancel,
  onGone,
}: {
  change: GovernanceChange;
  onSaved: (c: GovernanceChange) => void;
  onCancel: () => void;
  /** The change was decided or expired meanwhile (409), or can no longer be seen (404). */
  onGone: (status: 404 | 409) => void;
}) {
  const id = React.useId();
  const [why, setWhy] = React.useState(change.note?.reason ?? "");
  const [ticket, setTicket] = React.useState(change.note?.ticket ?? "");
  const [busy, setBusy] = React.useState(false);
  const [refused, setRefused] = React.useState<string | null>(null);

  const hasNote = !!(change.note?.reason || change.note?.ticket);
  const empty = !why.trim() && !ticket.trim();

  const save = async () => {
    setBusy(true);
    setRefused(null);
    try {
      const saved = await api.noteChange(change.id, { reason: why.trim(), ticket: ticket.trim() });
      toast.success(CHANGE_NOTE.SAVED);
      onSaved(saved);
    } catch (e) {
      setRefused(e instanceof HttpError ? e.message : getErrorMessage(e));
      if (e instanceof HttpError && (e.status === 404 || e.status === 409)) onGone(e.status);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-3 rounded-xl border border-border p-4">
      <Field label={CHANGE_NOTE.WHY_LABEL} htmlFor={`${id}-why`} hint={CHANGE_NOTE.WHY_HINT}>
        <Textarea
          id={`${id}-why`}
          value={why}
          onChange={(e) => setWhy(e.target.value)}
          placeholder={CHANGE_NOTE.WHY_PLACEHOLDER}
          maxLength={500}
          rows={3}
          disabled={busy}
        />
      </Field>
      <Field label={CHANGE_NOTE.TICKET_LABEL} htmlFor={`${id}-ticket`} hint={CHANGE_NOTE.TICKET_HINT}>
        <Input
          id={`${id}-ticket`}
          className="font-mono"
          value={ticket}
          onChange={(e) => setTicket(e.target.value)}
          placeholder={CHANGE_NOTE.TICKET_PLACEHOLDER}
          maxLength={128}
          autoComplete="off"
          disabled={busy}
        />
      </Field>
      {refused && (
        <Note tone="red" role="alert">
          <b className="font-semibold">{CHANGE_NOTE.REFUSED_TITLE}</b>
          <Mono className="text-inherit">{refused}</Mono>
        </Note>
      )}
      <div className="flex justify-end gap-2">
        <Button variant="ghost" disabled={busy} onClick={onCancel}>
          {CHANGE_NOTE.CANCEL}
        </Button>
        <Button disabled={busy || (empty && !hasNote)} onClick={() => void save()}>
          {busy && <Loader2 className="size-4 animate-spin" />}
          {empty && hasNote && !busy ? CHANGE_NOTE.REMOVE : CHANGE_NOTE.SAVE}
        </Button>
      </div>
    </div>
  );
}
