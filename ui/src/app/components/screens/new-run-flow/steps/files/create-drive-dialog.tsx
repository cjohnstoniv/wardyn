/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "+ Create user drive": one field, the name. The server assigns the mount path
// and the drive is attached to this run once it exists. A refusal (a name already
// in use, the person's limit) renders the server's sentence under the field. It
// opens over the Add dialog; Esc closes this one only, and focus goes back to the
// control that opened it.

import * as React from "react";
import { Button } from "../../../../ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogTitle } from "../../../../ui/dialog";
import { Input } from "../../../../ui/input";
import { Label } from "../../../../ui/label";
import { Chip } from "../../../../wardyn/primitives";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { getErrorMessage } from "../../../../../lib/format";
import type { MeDrive } from "../../../../../lib/api/me-drives";
import { useEscapeClosesThis } from "./nested-escape";

const NAME_ID = "nr-create-drive-name";

export function CreateDriveDialog({ open, onOpenChange, create, onCreated, returnFocusTo }: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  create: (name: string) => Promise<MeDrive>;
  onCreated: (drive: MeDrive) => void;
  /** The control that opened the dialog; it gets focus back when the dialog closes. */
  returnFocusTo: React.RefObject<HTMLElement | null>;
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        onOpenAutoFocus={(e) => {
          // Input forwards no ref under React 18, so the first field is found by id.
          e.preventDefault();
          (e.currentTarget as HTMLElement).querySelector<HTMLInputElement>(`#${NAME_ID}`)?.focus();
        }}
        onCloseAutoFocus={(e) => {
          e.preventDefault();
          returnFocusTo.current?.focus();
        }}
      >
        <CreateDriveForm onOpenChange={onOpenChange} create={create} onCreated={onCreated} />
      </DialogContent>
    </Dialog>
  );
}

function CreateDriveForm({ onOpenChange, create, onCreated }: Pick<Parameters<typeof CreateDriveDialog>[0], "onOpenChange" | "create" | "onCreated">) {
  const [name, setName] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [failure, setFailure] = React.useState("");
  const errorId = React.useId();
  useEscapeClosesThis(React.useCallback(() => onOpenChange(false), [onOpenChange]));

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!name.trim() || busy) return;
    setBusy(true);
    setFailure("");
    try {
      const drive = await create(name.trim());
      onCreated(drive);
      onOpenChange(false);
    } catch (error) {
      setFailure(getErrorMessage(error));
      setBusy(false);
    }
  };

  return (
    <form onSubmit={submit} className="flex flex-col gap-4">
      <DialogTitle>{NR_FILES.CREATE_TITLE}</DialogTitle>
      <DialogDescription>{NR_FILES.CREATE_INTRO}</DialogDescription>
      <div className="flex flex-col gap-1.5">
        <div className="flex items-center gap-2">
          <Label htmlFor={NAME_ID}>{NR_FILES.DRIVE_NAME}</Label>
          <Chip>{NR_SHARED.REQUIRED}</Chip>
        </div>
        <Input
          id={NAME_ID}
          value={name}
          placeholder={NR_FILES.DRIVE_NAME_PLACEHOLDER}
          aria-invalid={failure ? true : undefined}
          aria-describedby={failure ? errorId : undefined}
          onChange={(e) => setName(e.target.value)}
        />
        {failure && <p id={errorId} role="alert" className="text-sm text-danger">{failure}</p>}
      </div>
      <DialogFooter>
        <Button type="button" variant="ghost" onClick={() => onOpenChange(false)}>{NR_SHARED.CANCEL}</Button>
        <Button type="submit" disabled={!name.trim() || busy}>{NR_FILES.CREATE_CONFIRM}</Button>
      </DialogFooter>
    </form>
  );
}
