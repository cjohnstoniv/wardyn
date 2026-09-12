/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The take-over confirm stop, split out of attach-terminal.tsx.
//
// A seam and not a shuffle: this is the only part of the panel that is a
// DIALOG rather than the terminal itself — it owns no socket, no PTY geometry
// and no attach state, just a question and the two answers to it. Everything
// it needs is the holder's name and a callback, which is why the whole of it
// moves without a prop drilled through anything.
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "./ui/alert-dialog";
import { RUN_COCKPIT } from "./wardyn/copy";

// Take-over ends another human's live session, so it gets the same confirm
// stop as the deny confirm in live-approvals.tsx. (NOT "irreversible" — a deny
// can re-raise at `once` scope and can always be undone in the workspace's
// egress settings at `always` scope; this is a consequential-action stop, not
// a claim about undoability.)
export function TakeoverConfirmDialog({
  open,
  holderPrincipal,
  onOpenChange,
  onConfirm,
}: {
  open: boolean;
  /** Whose session this ends. The panel never opens this without one — a
   *  confirm that cannot say whose session it ends is worse than no button. */
  holderPrincipal: string;
  onOpenChange: (open: boolean) => void;
  onConfirm: () => void;
}) {
  return (
    <AlertDialog open={open} onOpenChange={onOpenChange}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{RUN_COCKPIT.takeOver}</AlertDialogTitle>
          <AlertDialogDescription>{RUN_COCKPIT.takeOverConfirm(holderPrincipal)}</AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            onClick={(e) => {
              e.preventDefault();
              onConfirm();
            }}
          >
            {RUN_COCKPIT.takeOver}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
