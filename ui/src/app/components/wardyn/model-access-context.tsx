/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// THE DOOR — one model-access answer, one dialog instance, one refresh, shared
// by every surface that can offer a sign-in or a key. Since #544 that includes
// Settings, the Agents tab and Getting started, which each mounted their own
// pane before: their buttons stay, and open this door keyed by provider.
//
// A context rather than AppShell props, for three reasons that are each a diff:
// the shell spends two lines, `new-run-screen.tsx` (995 lines, at the gate)
// spends ZERO, and a surface nested deep inside a screen — the New Run rail, a
// failed run's failure block, a held run's approval row — reaches the same
// answer without prop-drilling through either.
//
// It holds THREE things the strip alone could not:
//
//  1. `target` + openDoor/closeDoor. Reusing a dialog COMPONENT is not sharing an
//     instance: four callers each mounting their own would give four dialogs,
//     four login runs and four sets of cleanup (Codex #15). The state lives
//     here; the one mount rides <ModelAccessBanner/>, which the shell renders
//     exactly once.
//  2. `claim()` — door ownership, ref-counted (round-2 UX B1). One PRIMARY
//     recovery action per state per screen: a page surface that renders its own
//     sign-in control claims the door on mount and releases on unmount, and the
//     strip renders its button only while the count is zero. Its SENTENCE
//     stays — the state is still true, it is the second button that is the
//     defect.
//  3. `refresh` — the shell's own /setup/status read, so a sign-in completed in
//     the dialog updates every surface at once instead of each one re-fetching.
//
// The VIEWER is resolved in the consumer hook, not here: OperatorProvider lives
// inside AppShell (app-shell.tsx), below the provider App.tsx mounts around
// <Routes>, so a door graded here would read the fail-open operator default for
// every caller. Grading at the point of consumption is also what makes the
// audience-aware `shared_expired` arm correct for a surface the shell does not
// own.

import * as React from "react";

import {
  modelAccessDoor,
  NO_MODEL_ACCESS_DOOR,
  resolveDoor,
  type DoorRequest,
  type DoorTarget,
  type ModelAccessDoor,
} from "../../lib/model-access";
import type { SetupStatus } from "../../lib/types";
import { viewOfPath } from "./console-view";
import { useOperator, useOperatorResolved, usePrincipal } from "./operator-context";

/** How an entrance opens the door. */
export interface OpenDoorOptions {
  /** Which door (#544): a provider id, or today's login lane. Default
   *  `{ login: "aws" }` — the one door every existing caller opened. */
  for?: DoorRequest;
  /** The element focus should return to when the door closes and no better
   *  target exists. Callers whose OWN trigger unmounts before the door closes
   *  (the New Run rail's sign-in control, gone the moment the state it
   *  described clears) pass their neighbour (Launch) here:
   *  document.activeElement at call time is a DETACHED node by the time
   *  onCloseAutoFocus runs, and focusOpener() on a detached node fails.
   *  Omitted, it falls back to document.activeElement (S1 fix, review-1). */
  returnTo?: HTMLElement | null;
  /** What to do when the sign-in COMPLETES, for the one caller whose next step
   *  is not "read the strip" but "launch again": the New Run rail, on the
   *  server's model-credential refusal. Reported through signedIn() (the
   *  dialog's onDone), never inferred from `open` falling — it falls the same
   *  way on Escape. A cancellation drops it. It may resolve to the server's
   *  refusal of that relaunch when no screen is left to show it (#146): the
   *  strip then shows it (B9). */
  onSignedIn?: () => void | Promise<string | void>;
  /** Runs on EVERY close, signed in or not — for a screen that keeps its own
   *  /setup/status read (Settings): a capture can land even when the pane
   *  ends on "couldn't verify", and a cancellation costs one GET. */
  onClosed?: () => void;
}

/** What a caller gets: the graded door, plus the three things only the shared
 *  instance can offer. */
export interface ModelAccessDoorHandle extends ModelAccessDoor {
  /** Re-read /setup/status (the shell's own read). Returns its promise so a
   *  caller can await the new answer. */
  refresh: () => void | Promise<unknown>;
  /** Take ownership of the door for this screen; call the returned function to
   *  release it. Written to be used as an effect body:
   *  `React.useEffect(() => door.claim(), [door.claim])`. */
  claim: () => () => void;
  /** Whether ANY surface currently owns the door. */
  claimed: boolean;
  /** THIS viewer's resolved role — false until /me has answered, which is also
   *  when every field above reads as "nothing to say". Consumers read it here
   *  rather than calling useOperator() again: a second read would answer the
   *  fail-open default in exactly the window the door refuses to grade. */
  operator: boolean;
  /** THIS viewer's resolved subject (GET /me's `principal`), "" until /me has
   *  answered — the same window `operator` above reads as false. Consumers that
   *  compare a row's owner against the viewer read it here rather than calling
   *  usePrincipal() again, so the ownership answer and the tier answer can
   *  never come from two different moments. */
  principal: string;
  open: boolean;
  /** The open door's key, fixed when it opened; null while closed. */
  target: DoorTarget | null;
  /** Bumped when an entrance asks for the door while it is already open: the
   *  one dialog takes focus instead of a second door opening (#544). */
  focusSeq: number;
  /** Open THE door. A no-op but for focus while one is open — a second door
   *  would be a second sign-in sandbox, and replacing the first would orphan
   *  the run it started. A request with no door (resolveDoor's null) opens
   *  nothing. */
  openDoor: (options?: OpenDoorOptions) => void;
  /** B9: a relaunch refused after its screen was gone — the server's
   *  sentence, "" when there is none — and the strip's Dismiss. */
  refusal: string;
  dismissRefusal: () => void;
  /** The cancel path (Escape, the overlay, the pane's own Cancel). */
  closeDoor: () => void;
  /** The completion path: close, then run the opener's `onSignedIn` once. */
  signedIn: () => void;
  /** Put focus back on the control that opened the door, and say whether it
   *  could: false once a completed sign-in has taken that surface away, which
   *  is exactly when restoring to it would drop focus on <body>. */
  focusOpener: () => boolean;
}

interface ModelAccessContextValue {
  status: SetupStatus | null;
  refresh: () => void | Promise<unknown>;
  claim: () => () => void;
  claimed: boolean;
  target: DoorTarget | null;
  focusSeq: number;
  openDoor: (options?: OpenDoorOptions) => void;
  refusal: string;
  dismissRefusal: () => void;
  closeDoor: () => void;
  signedIn: () => void;
  focusOpener: () => boolean;
}

// FAIL-OPEN DEFAULT — no provider above means no status, which grades to
// NO_MODEL_ACCESS_DOOR (needsAttention false): every screen and every suite
// that mounts a component directly renders exactly what it renders today. Never
// "harden" this into a thrown error; a missing provider must not be able to
// paint a warning nobody can act on.
const ModelAccessContext = React.createContext<ModelAccessContextValue>({
  status: null,
  refresh: () => {},
  claim: () => () => {},
  claimed: false,
  target: null,
  focusSeq: 0,
  openDoor: () => {},
  refusal: "",
  dismissRefusal: () => {},
  closeDoor: () => {},
  signedIn: () => {},
  focusOpener: () => false,
});

export function ModelAccessProvider({
  status,
  onRefresh,
  children,
}: {
  status: SetupStatus | null;
  onRefresh: () => void | Promise<unknown>;
  children: React.ReactNode;
}) {
  // A COUNT, not a boolean: two surfaces can legitimately overlap for a frame
  // during a route change (the old screen's cleanup runs after the new screen's
  // effect), and a boolean would leave the strip's button suppressed forever
  // the first time that happened.
  const claims = React.useRef(0);
  const [claimed, setClaimed] = React.useState(false);

  // Stable identities: every one of these is an effect dependency somewhere
  // (claim especially), and a new function each render would re-run those
  // effects on every status poll.
  const claim = React.useCallback(() => {
    claims.current += 1;
    setClaimed(true);
    let released = false;
    return () => {
      if (released) return;
      released = true;
      claims.current -= 1;
      setClaimed(claims.current > 0);
    };
  }, []);
  // WHO opened it, captured here because this is the one point every caller
  // passes through (the strip's button, the rail's link, the failure block, a
  // held-approval row). Radix returns focus to that element when the dialog
  // closes — right after a cancellation, and wrong after a sign-in that took
  // the control away with the state that justified it.
  const opener = React.useRef<Element | null>(null);
  // WHAT to do when the sign-in completes — the rail's relaunch. A ref, not
  // state: it is consumed exactly once, on signedIn(), and must never survive a
  // cancellation (a door closed on Escape and reopened from the strip would
  // otherwise launch a run the person walked away from).
  const onSignedIn = React.useRef<OpenDoorOptions["onSignedIn"] | null>(null);
  const onClosed = React.useRef<(() => void) | null>(null);
  // Refs beside the state: openDoor reads the CURRENT answer without taking
  // `status` as a dependency, which would give every caller a new openDoor on
  // every status poll.
  const [target, setTarget] = React.useState<DoorTarget | null>(null);
  const targetRef = React.useRef<DoorTarget | null>(null);
  const statusRef = React.useRef(status);
  React.useEffect(() => {
    statusRef.current = status;
  }, [status]);
  const [focusSeq, setFocusSeq] = React.useState(0);
  const openDoor = React.useCallback((options: OpenDoorOptions = {}) => {
    if (targetRef.current) {
      setFocusSeq((n) => n + 1);
      return;
    }
    // Keyed ONCE, at the click: a status poll that lands mid-sign-in must not
    // swap the door (and its sandbox) out from under the person. The path, not
    // useLocation(): this provider sits above <Routes> and is mounted without a
    // router by half the suites that use it; main.tsx's BrowserRouter makes the
    // two the same answer.
    const next = resolveDoor(
      statusRef.current,
      options.for ?? { login: "aws" },
      viewOfPath(typeof window === "undefined" ? "/" : window.location.pathname),
    );
    if (!next) return;
    opener.current = options.returnTo ?? (typeof document === "undefined" ? null : document.activeElement);
    onSignedIn.current = options.onSignedIn ?? null;
    onClosed.current = options.onClosed ?? null;
    targetRef.current = next;
    setTarget(next);
  }, []);
  const close = React.useCallback(() => {
    const closed = onClosed.current;
    onSignedIn.current = null;
    onClosed.current = null;
    targetRef.current = null;
    setTarget(null);
    closed?.();
  }, []);
  // B9 lives here because this provider outlives the route that asked for the
  // relaunch; the strip, mounted outside the routes, renders it.
  const [refusal, setRefusal] = React.useState("");
  const dismissRefusal = React.useCallback(() => setRefusal(""), []);
  const signedIn = React.useCallback(() => {
    const cb = onSignedIn.current;
    close();
    void Promise.resolve(cb?.()).then((sentence) => {
      if (sentence) setRefusal(sentence);
    });
  }, [close]);
  const focusOpener = React.useCallback(() => {
    const el = opener.current;
    if (!el || !el.isConnected || typeof (el as HTMLElement).focus !== "function") return false;
    (el as HTMLElement).focus();
    return true;
  }, []);

  const refreshRef = React.useRef(onRefresh);
  React.useEffect(() => {
    refreshRef.current = onRefresh;
  });
  const refresh = React.useCallback(() => refreshRef.current(), []);

  const value = React.useMemo<ModelAccessContextValue>(
    () => ({
      status,
      refresh,
      claim,
      claimed,
      target,
      focusSeq,
      openDoor,
      refusal,
      dismissRefusal,
      closeDoor: close,
      signedIn,
      focusOpener,
    }),
    [status, refresh, claim, claimed, target, focusSeq, openDoor, refusal, dismissRefusal, close, signedIn, focusOpener],
  );
  return <ModelAccessContext.Provider value={value}>{children}</ModelAccessContext.Provider>;
}

/** The shell's last /setup/status, ungraded, and its re-read — for a shell
 *  band that reads a different row of the same answer (the everyone-is-an-
 *  admin banner) instead of fetching the expensive endpoint a second time, and
 *  for the one screen whose writes change that row (the People step). */
export function useShellSetupStatus(): { status: SetupStatus | null; refresh: () => void | Promise<unknown> } {
  const { status, refresh } = React.useContext(ModelAccessContext);
  return React.useMemo(() => ({ status, refresh }), [status, refresh]);
}

/**
 * useModelAccessDoor grades the shell's last /setup/status for THIS viewer and
 * hands back the shared door controls.
 */
export function useModelAccessDoor(): ModelAccessDoorHandle {
  const ctx = React.useContext(ModelAccessContext);
  // useOperator()'s default is fail-OPEN (true) — right for a console that must
  // never lock an admin out of their own controls, and wrong for this: while
  // /me is in flight (or after it failed) a MEMBER under a dead shared row
  // would read the ADMIN's sentence, "The shared AWS sign-in no longer works …
  // sign in again", with a button the server then refuses — and usePrincipal()
  // is "" in the same window, so a "Not now" there would write an unkeyed flag
  // for whoever uses the tab next.
  //
  // So the door says NOTHING until the identity is known: the same rule the
  // shell's own role chip follows (useOperatorResolved), and the honest one —
  // the answer is audience-dependent and the audience is not known yet.
  const operator = useOperator();
  const resolved = useOperatorResolved();
  const principal = usePrincipal();
  const door = React.useMemo(
    () => (ctx.status && resolved ? modelAccessDoor(ctx.status, { operator }) : NO_MODEL_ACCESS_DOOR),
    [ctx.status, operator, resolved],
  );
  const viewerOperator = resolved && operator;
  const viewerPrincipal = resolved ? principal : "";
  return React.useMemo(
    () => ({
      ...door,
      refresh: ctx.refresh,
      claim: ctx.claim,
      claimed: ctx.claimed,
      operator: viewerOperator,
      principal: viewerPrincipal,
      open: ctx.target !== null,
      target: ctx.target,
      focusSeq: ctx.focusSeq,
      openDoor: ctx.openDoor,
      refusal: ctx.refusal,
      dismissRefusal: ctx.dismissRefusal,
      closeDoor: ctx.closeDoor,
      signedIn: ctx.signedIn,
      focusOpener: ctx.focusOpener,
    }),
    [
      door,
      viewerOperator,
      viewerPrincipal,
      ctx.refresh,
      ctx.claim,
      ctx.claimed,
      ctx.target,
      ctx.focusSeq,
      ctx.openDoor,
      ctx.refusal,
      ctx.dismissRefusal,
      ctx.closeDoor,
      ctx.signedIn,
      ctx.focusOpener,
    ],
  );
}

/**
 * useClaimModelAccessDoor — the caller's half of rule 2, as one line.
 *
 * `active` is the caller's own "am I rendering a sign-in control right now":
 * the rail only claims while it actually paints one, so a screen that decides
 * NOT to offer the door does not silently suppress the strip's button.
 */
export function useClaimModelAccessDoor(active: boolean): void {
  const { claim } = useModelAccessDoor();
  React.useEffect(() => {
    if (!active) return;
    return claim();
  }, [active, claim]);
}
