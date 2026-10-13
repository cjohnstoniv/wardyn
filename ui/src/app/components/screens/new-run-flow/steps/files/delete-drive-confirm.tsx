/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The confirm before a person deletes one of their own drives. It opens over the
// Add dialog, so Esc closes this one only. Focus lands on Cancel: the safe answer
// is the one a stray Enter gives. The server's sentence for a refusal (a run still
// using the drive, a delete that failed) shows beside the buttons, and focus goes
// back to the control that opened the confirm.

import * as React from "react";
import { Button } from "../../../../ui/button";
import { Dialog, DialogContent, DialogFooter, DialogTitle } from "../../../../ui/dialog";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { getErrorMessage } from "../../../../../lib/format";
import { useEscapeClosesThis } from "./nested-escape";

export function DeleteDriveConfirm({ name, remove, onDeleted, onClose, returnFocusTo }: {
  name: string;
  /** Destroys the drive; rejects with the server's refusal. */
  remove: () => Promise<void>;
  onDeleted: () => void;
  onClose: () => void;
  /** Where focus goes when the confirm closes: the control that opened it, or whatever stands in once that is gone. */
  returnFocusTo: () => HTMLElement | null;
}) {
  const cancelRef = React.useRef<HTMLButtonElement>(null);
  const [busy, setBusy] = React.useState(false);
  const [failure, setFailure] = React.useState("");
  useEscapeClosesThis(React.useCallback(() => !busy && onClose(), [busy, onClose]));

  const confirm = async () => {
    setBusy(true);
    setFailure("");
    try {
      await remove();
      onDeleted();
      onClose();
    } catch (error) {
      setFailure(getErrorMessage(error));
      setBusy(false);
    }
  };

  return (
    <Dialog open onOpenChange={(open) => !open && !busy && onClose()}>
      <DialogContent
        aria-describedby={undefined}
        onOpenAutoFocus={(e) => {
          e.preventDefault();
          cancelRef.current?.focus();
        }}
        onCloseAutoFocus={(e) => {
          e.preventDefault();
          returnFocusTo()?.focus();
        }}
      >
        <DialogTitle className="text-base font-normal">{NR_FILES.DELETE_CONFIRM(name)}</DialogTitle>
        {failure && <p role="alert" className="text-sm text-danger">{failure}</p>}
        <DialogFooter>
          <Button ref={cancelRef} type="button" variant="ghost" disabled={busy} onClick={onClose}>{NR_SHARED.CANCEL}</Button>
          <Button type="button" variant="outline" className="text-danger" disabled={busy} onClick={confirm}>{NR_FILES.DELETE_DRIVE}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
