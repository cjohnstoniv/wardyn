/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The one confirm the runner doors go through: a revoke (M-RN-1 RA4, unused tokens, M-RN-2 MR6/MR8) and
// the runners switch (RA5b, RA5j). An AlertDialog with no field, so focus opens on Cancel: the action is
// never the Enter target. A refusal stays inline with the server's sentence. Where the caller passes
// `onRaced`, a 404 or 409 means someone else got there first, so the caller re-reads and the dialog
// closes; the switch does not, because there a 409 is the refusal itself.
import * as React from "react";
import { Loader2 } from "lucide-react";
import { HttpError } from "../../../lib/api/core";
import { getErrorMessage } from "../../../lib/format";
import { Button } from "../../ui/button";
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "../../ui/alert-dialog";

export function ConfirmDialog({
  open,
  onOpenChange,
  title,
  body,
  confirmLabel,
  destructive = true,
  errorTitle,
  onConfirm,
  onDone,
  onRaced,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  /** Paragraphs under the title; the last one may be a count sentence the caller already read. */
  body: string[];
  confirmLabel: string;
  destructive?: boolean;
  /** A heading over the server's refusal sentence. */
  errorTitle?: string;
  onConfirm: () => Promise<unknown>;
  /** The action happened. */
  onDone: () => void;
  /** Someone else got there first (404 or 409): re-read; the dialog closes. */
  onRaced?: () => void;
}) {
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");
  React.useEffect(() => {
    if (open) setError("");
  }, [open]);

  const run = async () => {
    setBusy(true);
    setError("");
    try {
      await onConfirm();
      onOpenChange(false);
      onDone();
    } catch (e) {
      if (onRaced && e instanceof HttpError && (e.status === 404 || e.status === 409)) {
        onOpenChange(false);
        onRaced();
      } else {
        setError(getErrorMessage(e));
      }
    } finally {
      setBusy(false);
    }
  };

  return (
    <AlertDialog open={open} onOpenChange={(o) => !busy && onOpenChange(o)}>
      <AlertDialogContent className="sm:max-w-md">
        <AlertDialogHeader>
          <AlertDialogTitle>{title}</AlertDialogTitle>
          <AlertDialogDescription asChild>
            <div className="space-y-2">
              {body.map((p) => (
                <p key={p}>{p}</p>
              ))}
            </div>
          </AlertDialogDescription>
        </AlertDialogHeader>
        {error && (
          <div role="alert" className="space-y-1 text-body text-danger">
            {errorTitle && <p className="font-medium">{errorTitle}</p>}
            <p>{error}</p>
          </div>
        )}
        <AlertDialogFooter>
          <AlertDialogCancel disabled={busy}>Cancel</AlertDialogCancel>
          <Button variant={destructive ? "destructive" : "default"} disabled={busy} onClick={() => void run()}>
            {busy && <Loader2 className="size-3.5 animate-spin" />}
            {confirmLabel}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
