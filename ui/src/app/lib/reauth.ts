/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #483 — a session that ends mid-page. App.tsx owns this state (its 401
// handler calls lapse()); the shell's reauth layer reads it to draw the
// "Sign in to continue" dialog or the signed-out bar; a saving screen reads
// useWriteDropped() (use-write-dropped.ts) to say its last save never went
// through. Deliberately
// free of copy and UI: it sits on the eager graph, and the layer that draws
// it is a lazy chunk (bundle-split.test.ts).
import * as React from "react";
import type { Refused } from "./api/core";

// none: signed in. dialog: signed out, asking. bar: signed out, "Not now".
export type ReauthPhase = "none" | "dialog" | "bar";

export interface Reauth {
  phase: ReauthPhase;
  /** The owner id (WfetchInit.save) of a Save refused in this lapse — never
   *  re-sent — or null. Only that owner's screen ever shows it. */
  writeDropped: string | null;
  /** "dialog": ask (again). "bar": Not now. "none": the same person is back
   *  and the page carries on. */
  setPhase: (phase: ReauthPhase) => void;
  /** Someone else signed in: drop the page and load `path` fresh. */
  reloadAs: (path: string) => void;
  clearWriteDropped: () => void;
  /** Whether writeDropped's owner is mounted to show it beside its own Save. */
  writeDroppedClaimed: () => boolean;
  claimWriteDropped: (owner: string) => () => void;
}

const noop = () => {};
export const ReauthContext = React.createContext<Reauth>({
  phase: "none",
  writeDropped: null,
  setPhase: noop,
  reloadAs: noop,
  clearWriteDropped: noop,
  writeDroppedClaimed: () => false,
  claimWriteDropped: () => noop,
});

export function useReauth(): Reauth {
  return React.useContext(ReauthContext);
}

interface State {
  phase: ReauthPhase;
  writeDropped: string | null;
}

export function useReauthController(reloadAs: (path: string) => void): {
  reauth: Reauth;
  lapse: (refused: Refused) => void;
  reset: () => void;
} {
  const [state, setState] = React.useState<State>({ phase: "none", writeDropped: null });
  // One mounted screen per Save owner (an owner is a screen's own resource).
  const claims = React.useRef(new Set<string>());

  // Functional updates throughout: several requests can 401 in one tick, and
  // each must see the phase the one before it set. A write refused under the
  // bar asks again — the person just tried to change something.
  const lapse = React.useCallback(({ write, save }: Refused) => {
    setState((s) => {
      if (s.phase === "none") return { phase: "dialog", writeDropped: save ?? null };
      const phase = write && s.phase === "bar" ? "dialog" : s.phase;
      return { phase, writeDropped: save ?? s.writeDropped };
    });
  }, []);
  const reset = React.useCallback(() => setState({ phase: "none", writeDropped: null }), []);
  // Stable identity: useWriteDropped's effect depends on it. The owner going
  // away takes its dropped save with it — the draft it named is gone too.
  const claimWriteDropped = React.useCallback((owner: string) => {
    claims.current.add(owner);
    return () => {
      claims.current.delete(owner);
      setState((s) => (s.writeDropped === owner ? { ...s, writeDropped: null } : s));
    };
  }, []);

  const reauth = React.useMemo<Reauth>(
    () => ({
      ...state,
      setPhase: (phase) => setState((s) => ({ ...s, phase })),
      reloadAs,
      clearWriteDropped: () => setState((s) => (s.writeDropped ? { ...s, writeDropped: null } : s)),
      writeDroppedClaimed: () => claims.current.has(state.writeDropped ?? ""),
      claimWriteDropped,
    }),
    [state, reloadAs, claimWriteDropped],
  );
  return { reauth, lapse, reset };
}
