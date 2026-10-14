/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "+ Create user drive": a card inside the Choose step, as the Files prototype draws it. One field, the
// name; the server assigns the mount path. A refusal (a name already in use, the person's limit) shows
// under the field as the server wrote it. Enter creates, Escape closes this card only.

import * as React from "react";
import { Button } from "../../../../ui/button";
import { Input } from "../../../../ui/input";
import { Label } from "../../../../ui/label";
import { Chip } from "../../../../wardyn/primitives";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { getErrorMessage } from "../../../../../lib/format";
import type { MeDrive } from "../../../../../lib/api/me-drives";
import { useEscapeClosesThis } from "./nested-escape";

const NAME_ID = "nr-create-drive-name";

export function CreateDriveCard({ create, onCreated, onClose }: {
  create: (name: string) => Promise<MeDrive>;
  onCreated: (drive: MeDrive) => void;
  onClose: () => void;
}) {
  const [name, setName] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [failure, setFailure] = React.useState("");
  const card = React.useRef<HTMLFormElement>(null);
  const errorId = React.useId();
  // The create cannot be recalled once sent, so nothing here pretends to cancel it: the key is taken and ignored.
  useEscapeClosesThis(React.useCallback(() => !busy && onClose(), [busy, onClose]), card);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!name.trim() || busy) return;
    setBusy(true);
    setFailure("");
    // The buttons are disabled while it runs; focus stays on the field so a key press still lands in the card.
    document.getElementById(NAME_ID)?.focus();
    try {
      onCreated(await create(name.trim()));
    } catch (error) {
      setFailure(getErrorMessage(error));
      setBusy(false);
    }
  };

  return (
    <form ref={card} onSubmit={submit} className="mt-1 flex basis-full flex-col gap-3.5 rounded-[9px] border border-border bg-muted p-3.5">
      <p className="text-sm text-muted-foreground">{NR_FILES.CREATE_INTRO}</p>
      <div className="flex flex-col gap-1.5">
        <div className="flex items-center justify-between gap-2">
          <Label htmlFor={NAME_ID}>{NR_FILES.DRIVE_NAME}</Label>
          <Chip>{NR_SHARED.REQUIRED}</Chip>
        </div>
        <Input
          id={NAME_ID}
          autoFocus
          value={name}
          placeholder={NR_FILES.DRIVE_NAME_PLACEHOLDER}
          aria-required="true"
          aria-invalid={failure ? true : undefined}
          aria-describedby={failure ? errorId : undefined}
          onChange={(e) => setName(e.target.value)}
        />
        {failure && <p id={errorId} role="alert" className="text-sm text-danger">{failure}</p>}
      </div>
      <div className="flex justify-end gap-2">
        <Button type="button" variant="outline" disabled={busy} onClick={onClose}>{NR_SHARED.CANCEL}</Button>
        <Button type="submit" disabled={!name.trim() || busy}>{NR_FILES.CREATE_CONFIRM}</Button>
      </div>
    </form>
  );
}
