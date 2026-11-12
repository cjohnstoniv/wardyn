/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "../ui/alert-dialog";
import { CANCEL_NOT_STARTED as NS } from "./copy/cancel-not-started";
import { toastRunName } from "./copy/run-lifetime";
import { RunRef } from "./run-ref";

// Said under the question when the run holds a Both ways folder: ending the run does not copy its changes back.
const BOTH_WAYS_FOLDER_LOST =
  "This run's Both ways folder does not come back: ending a run discards the sandbox's changes. To keep them, run `wardyn folder bring-back <run>` first.";

// Shared kill-run confirm for the two places Kill is offered (the Runs board's
// row menu and Run Detail's header): an enforcement stop deserves the same
// question, in the same words, wherever it is fired from — the two copies had
// already been written out twice and were only kept in sync by hand.
// Controlled-only (like ConfirmEgressDialog) so a caller can render it as a
// SIBLING of a DropdownMenuContent — nested inside, it would unmount with the
// menu the moment the item is clicked.
export function KillRunDialog({
  runId,
  title,
  onOpenChange,
  onConfirm,
  notStarted = false,
  bothWaysFolder = false,
}: {
  // The run to kill; also doubles as the "is the dialog open" flag (null = closed).
  runId: string | null;
  // What the page calls the run (its title, else its task). Named in the question with the id beneath it;
  // absent, the id stands in so the question still names the run.
  title?: string;
  onOpenChange: (open: boolean) => void;
  // Called with the id the dialog is showing, so the kill can only ever hit
  // the run the title names.
  onConfirm: (runId: string) => void;
  // A run that has not started (scheduled, or waiting for its owner's sign-in) holds no sandbox and no credential,
  // so the question is a plain cancel and says nothing about tearing anything down.
  notStarted?: boolean;
  // The run holds a Both ways computer folder: ending it discards the sandbox's changes to that folder.
  bothWaysFolder?: boolean;
}) {
  // First line, bounded: a run named by its task can be a paragraph.
  const line = (title ?? "").trim().split("\n")[0];
  const name = line.length > 80 ? `${line.slice(0, 79)}…` : line;
  return (
    <AlertDialog open={!!runId} onOpenChange={onOpenChange}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle className="break-words">{notStarted ? NS.TITLE(toastRunName({ title })) : `End ${toastRunName({ title })}?`}</AlertDialogTitle>
          <AlertDialogDescription>
            {name && runId && (
              <>
                <RunRef runId={runId} />.{" "}
              </>
            )}
            {notStarted
              ? NS.BODY
              : "This terminates the agent run, tears down the sandbox, and revokes any brokered credentials. Ending the run is recorded in the audit trail. This cannot be undone."}
            {bothWaysFolder && !notStarted && ` ${BOTH_WAYS_FOLDER_LOST}`}
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>{notStarted ? NS.KEEP : "Cancel"}</AlertDialogCancel>
          <AlertDialogAction
            onClick={() => runId && onConfirm(runId)}
            className="bg-danger text-danger-foreground hover:bg-danger/90"
          >
            {notStarted ? NS.CONFIRM : "End run"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
