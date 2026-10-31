/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The confirm before a person deletes one of their own drives. It opens over the
// Add dialog, so Esc closes this one only. Focus lands on Cancel: the safe answer
// is the one a stray Enter gives. The server's sentence for a refusal (a run still
// using the drive, a delete that failed) shows beside the buttons, and focus goes
// back to the control that opened the confirm. A disk drive whose Mac is offline or gone is deleted only in
// part: the server's sentence for that stays on the confirm, above a single close button.

import * as React from "react";
import { Button } from "../../../../ui/button";
import { Dialog, DialogContent, DialogFooter, DialogTitle } from "../../../../ui/dialog";
import { Notice } from "../../../../wardyn/notice";
import { NR_FILES } from "../../../../wardyn/copy/new-run/files";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { getErrorMessage } from "../../../../../lib/format";
import type { MyDriveDeleteResult } from "../../../../../lib/api/me-drives";
import { useEscapeClosesThis } from "./nested-escape";

export function DeleteDriveConfirm({ name, remove, onDeleted, onClose, returnFocusTo }: {
  name: string;
  /** Destroys the drive; rejects with the server's refusal, and answers with a result when a disk drive was not removed in full. */
  remove: () => Promise<MyDriveDeleteResult | null>;
  /** Called once the delete was accepted, with the result when it was not a full delete (the Notice then speaks for itself). */
  onDeleted: (result: MyDriveDeleteResult | null) => void;
  onClose: () => void;
  /** Where focus goes when the confirm closes: the control that opened it, or whatever stands in once that is gone. */
  returnFocusTo: () => HTMLElement | null;
}) {
  const cancelRef = React.useRef<HTMLButtonElement>(null);
  const [busy, setBusy] = React.useState(false);
  const [failure, setFailure] = React.useState("");
  const [partial, setPartial] = React.useState<MyDriveDeleteResult | null>(null);
  const content = React.useRef<HTMLDivElement>(null);
  // Only a key pressed inside the confirm: a dialog opened above it (a sign-in prompt) keeps its own Escape.
  // While the delete is in flight the key is taken and does nothing, as the buttons do.
  useEscapeClosesThis(React.useCallback(() => !busy && onClose(), [busy, onClose]), content);

  const confirm = async () => {
    setBusy(true);
    setFailure("");
    try {
      const result = await remove();
      onDeleted(result);
      if (result) {
        setPartial(result);
        setBusy(false);
      } else {
        onClose();
      }
    } catch (error) {
      setFailure(getErrorMessage(error));
      setBusy(false);
    }
  };

  return (
    <Dialog open onOpenChange={(open) => !open && !busy && onClose()}>
      <DialogContent
        ref={content}
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
        {partial ? (
          <>
            <DialogTitle className="sr-only">{NR_FILES.DELETE_DRIVE}</DialogTitle>
            <Notice tone={partial.reason === "member_drive_record_only" ? "warning" : "info"} live>{partial.message}</Notice>
            <DialogFooter>
              <Button ref={cancelRef} type="button" variant="outline" onClick={onClose}>{NR_SHARED.CLOSE_DIALOG}</Button>
            </DialogFooter>
          </>
        ) : (
          <>
            <DialogTitle className="text-base font-normal">{NR_FILES.DELETE_CONFIRM(name)}</DialogTitle>
            {failure && <p role="alert" className="text-sm text-danger">{failure}</p>}
            <DialogFooter>
              <Button ref={cancelRef} type="button" variant="ghost" disabled={busy} onClick={onClose}>{NR_SHARED.CANCEL}</Button>
              <Button type="button" variant="outline" className="text-danger" disabled={busy} onClick={confirm}>{NR_FILES.DELETE_DRIVE}</Button>
            </DialogFooter>
          </>
        )}
      </DialogContent>
    </Dialog>
  );
}
