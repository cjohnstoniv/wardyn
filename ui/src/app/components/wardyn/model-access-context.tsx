/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// THE DOOR — one model-access answer, one dialog instance, one refresh, shared
// by every surface that can offer the AWS sign-in.
//
// A context rather than AppShell props, for three reasons that are each a diff:
// the shell spends two lines, `new-run-screen.tsx` (995 lines, at the gate)
// spends ZERO, and a surface nested deep inside a screen — the New Run rail, a
// failed run's failure block, a held run's approval row — reaches the same
// answer without prop-drilling through either.
//
// It holds THREE things the strip alone could not:
//
//  1. `open` + openDoor/closeDoor. Reusing a dialog COMPONENT is not sharing an
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

import { modelAccessDoor, NO_MODEL_ACCESS_DOOR, type ModelAccessDoor } from "../../lib/model-access";
import type { SetupStatus } from "../../lib/types";
import { useOperator } from "./operator-context";

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
  open: boolean;
  openDoor: () => void;
  closeDoor: () => void;
}

interface ModelAccessContextValue {
  status: SetupStatus | null;
  refresh: () => void | Promise<unknown>;
  claim: () => () => void;
  claimed: boolean;
  open: boolean;
  openDoor: () => void;
  closeDoor: () => void;
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
  open: false,
  openDoor: () => {},
  closeDoor: () => {},
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
  const [open, setOpen] = React.useState(false);
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
  const openDoor = React.useCallback(() => setOpen(true), []);
  const closeDoor = React.useCallback(() => setOpen(false), []);

  const refreshRef = React.useRef(onRefresh);
  React.useEffect(() => {
    refreshRef.current = onRefresh;
  });
  const refresh = React.useCallback(() => refreshRef.current(), []);

  const value = React.useMemo<ModelAccessContextValue>(
    () => ({ status, refresh, claim, claimed, open, openDoor, closeDoor }),
    [status, refresh, claim, claimed, open, openDoor, closeDoor],
  );
  return <ModelAccessContext.Provider value={value}>{children}</ModelAccessContext.Provider>;
}

/**
 * useModelAccessDoor grades the shell's last /setup/status for THIS viewer and
 * hands back the shared door controls.
 */
export function useModelAccessDoor(): ModelAccessDoorHandle {
  const ctx = React.useContext(ModelAccessContext);
  const operator = useOperator();
  const door = React.useMemo(
    () => (ctx.status ? modelAccessDoor(ctx.status, { operator }) : NO_MODEL_ACCESS_DOOR),
    [ctx.status, operator],
  );
  return React.useMemo(
    () => ({
      ...door,
      refresh: ctx.refresh,
      claim: ctx.claim,
      claimed: ctx.claimed,
      open: ctx.open,
      openDoor: ctx.openDoor,
      closeDoor: ctx.closeDoor,
    }),
    [door, ctx.refresh, ctx.claim, ctx.claimed, ctx.open, ctx.openDoor, ctx.closeDoor],
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
