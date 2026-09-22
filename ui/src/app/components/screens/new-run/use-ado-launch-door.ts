/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #386's launch door state machine — split out of new-run-screen.tsx (the
// file's own 1000-line gate), the same seam new-run-primitives.tsx and
// new-run-rail.tsx already took. Mirrors the model-access door's own
// auto-open-on-422 shape (new-run-rail.tsx's `credentialRefused` effect), for
// a connection that is a popup + poll (use-ado-connect.ts) rather than an
// in-page sign-in pane.
//
// One-shot by construction: `notifyLaunchError` opens the dialog IMPERATIVELY,
// from the catch block that just received the 422, rather than reactively off
// a persisted boolean — so there is no "re-arm before the next Launch" guard
// to own here or in the caller.
import * as React from "react";
import { toast } from "sonner";
import { ADO } from "../../../lib/ado-entra-copy";
import { isGitCredentialRefusal } from "../../../lib/api/runs";
import { useAdoConnect } from "../../../lib/hooks/use-ado-connect";

export function useAdoLaunchDoor(relaunch: () => void): {
  /** Call from Launch's catch block with the caught error. */
  notifyLaunchError: (e: unknown) => void;
  dialog: { open: boolean; connecting: boolean; onConfirm: () => void; onCancel: () => void };
} {
  const [open, setOpen] = React.useState(false);
  const { connecting, connect } = useAdoConnect();
  // The latest `relaunch`, read at confirm time — `launch` is redefined every
  // render of the caller.
  const relaunchRef = React.useRef(relaunch);
  relaunchRef.current = relaunch;

  // Connect confirmed: run the popup + poll, and — only on a real connection
  // — close the dialog, toast the relaunch (§7.7's RELAUNCH_TOAST), and press
  // Launch again with the form exactly as it stood. A closed-without-
  // connecting popup just closes the dialog; nothing relaunches.
  const onConfirm = async () => {
    const connected = await connect();
    setOpen(false);
    if (connected) {
      toast.success(ADO.RELAUNCH_TOAST_TITLE, { description: ADO.RELAUNCH_TOAST_BODY });
      relaunchRef.current();
    }
  };

  return {
    notifyLaunchError: (e) => {
      if (isGitCredentialRefusal(e)) setOpen(true);
    },
    dialog: { open, connecting, onConfirm: () => void onConfirm(), onCancel: () => setOpen(false) },
  };
}
