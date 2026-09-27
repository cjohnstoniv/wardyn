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
//
// REVIEW FINDING F8: connecting does NOT relaunch. RELAUNCH_TOAST_BODY's own
// words are "launch when you're ready" — the person presses Launch
// themselves, with the form exactly as it stood; this hook never calls it
// for them.
import * as React from "react";
import { toast } from "sonner";
import { ADO } from "../../../lib/ado-entra-copy";
import { HttpError } from "../../../lib/api/core";
import { isGitCredentialRefusal } from "../../../lib/api/runs";
import { useAdoConnect } from "../../../lib/hooks/use-ado-connect";

export function useAdoLaunchDoor(): {
  /** Call from Launch's catch block with the caught error. */
  notifyLaunchError: (e: unknown) => void;
  dialog: {
    open: boolean;
    connecting: boolean;
    /** The Azure DevOps org the 422 body named (review finding F1) — read
     *  from the error itself, never from a preflight fact: a 422 can be the
     *  very first thing this caller hears about the row. */
    org: string;
    blockedUrl: string | null;
    onConfirm: () => void;
    /** Fires the SAME connect outcome as onConfirm, off connectFallback()'s
     *  bounded poll (review follow-up N1) — call when the blockedUrl link
     *  itself is clicked, alongside its normal href navigation. */
    onFallbackClick: () => void;
    onCancel: () => void;
  };
} {
  const [open, setOpen] = React.useState(false);
  const [org, setOrg] = React.useState("");
  const { connecting, connect, connectFallback, cancel, blockedUrl } = useAdoConnect();
  // Never toast into an unmounted screen (review finding F9) — a person who
  // navigated away while the popup was open must not see a stray "Connected"
  // toast land on whatever page they are on now.
  const mountedRef = React.useRef(true);
  React.useEffect(() => () => {
    mountedRef.current = false;
  }, []);

  // Connect confirmed: run the popup + poll (or connectFallback()'s bounded
  // one, off the fallback link), close the dialog either way, and — only on
  // a real connection — toast the fact (§7.7's RELAUNCH_TOAST). Nothing
  // relaunches (F8, above).
  //
  // connected === null means the popup was blocked (review finding F1):
  // useAdoConnect already set blockedUrl, so the dialog must stay OPEN,
  // showing the fallback link, instead of closing as if the person had
  // answered.
  const settle = async (connected: boolean | null) => {
    if (connected === null) return;
    if (!mountedRef.current) return;
    setOpen(false);
    if (connected) {
      toast.success(ADO.RELAUNCH_TOAST_TITLE, { description: ADO.RELAUNCH_TOAST_BODY });
    }
  };

  return {
    notifyLaunchError: (e) => {
      if (!isGitCredentialRefusal(e)) return;
      setOrg(e instanceof HttpError ? e.org : "");
      setOpen(true);
    },
    dialog: {
      open,
      connecting,
      org,
      blockedUrl,
      onConfirm: () => void connect().then(settle),
      onFallbackClick: () => void connectFallback().then(settle),
      // Cancel does not just close the dialog (regression finding 2) — it
      // stops the in-flight poll too. Without cancel(), the fallback link's
      // bounded poll ran on for the full FALLBACK_POLL_TIMEOUT_MS after
      // Cancel, and a late `true` fired the RELAUNCH_TOAST on whatever
      // screen the person had moved to by then.
      onCancel: () => {
        cancel();
        setOpen(false);
      },
    },
  };
}
