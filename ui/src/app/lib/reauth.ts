/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #483 — a session that ends mid-page. App.tsx owns this state (its 401
// handler calls lapse()); the shell's reauth layer reads it to draw the
// "Sign in to continue" dialog or the signed-out bar; a saving screen reads
// useWriteDropped() to say its last save never went through. Deliberately
// free of copy and UI: it sits on the eager graph, and the layer that draws
// it is a lazy chunk (bundle-split.test.ts).
import * as React from "react";

// none: signed in. dialog: signed out, asking. bar: signed out, "Not now".
export type ReauthPhase = "none" | "dialog" | "bar";

export interface Reauth {
  phase: ReauthPhase;
  /** A write was among the requests refused in this lapse. It is never re-sent. */
  writeDropped: boolean;
  open: () => void;
  notNow: () => void;
  /** The same person is back and the page carries on. */
  resume: () => void;
  /** Someone else signed in: drop the page and load `path` fresh. */
  reloadAs: (path: string) => void;
  clearWriteDropped: () => void;
  /** Whether a mounted screen will show writeDropped beside its own Save. */
  writeDroppedClaimed: () => boolean;
  claimWriteDropped: () => () => void;
}

const noop = () => {};
export const ReauthContext = React.createContext<Reauth>({
  phase: "none",
  writeDropped: false,
  open: noop,
  notNow: noop,
  resume: noop,
  reloadAs: noop,
  clearWriteDropped: noop,
  writeDroppedClaimed: () => false,
  claimWriteDropped: () => noop,
});

export function useReauth(): Reauth {
  return React.useContext(ReauthContext);
}

/** For a screen with its own Save: true once the person is back and a save of
 *  the lapse was refused; call clear() when that screen saves again. */
export function useWriteDropped(): [boolean, () => void] {
  const r = React.useContext(ReauthContext);
  const claim = r.claimWriteDropped;
  React.useEffect(() => claim(), [claim]);
  return [r.writeDropped && r.phase === "none", r.clearWriteDropped];
}

interface State {
  phase: ReauthPhase;
  writeDropped: boolean;
}

export function useReauthController(reloadAs: (path: string) => void): {
  reauth: Reauth;
  lapse: (write: boolean) => void;
  reset: () => void;
} {
  const [state, setState] = React.useState<State>({ phase: "none", writeDropped: false });
  const claims = React.useRef(0);

  // Functional updates throughout: several requests can 401 in one tick, and
  // each must see the phase the one before it set.
  const lapse = React.useCallback((write: boolean) => {
    setState((s) => {
      if (s.phase === "none") return { phase: "dialog", writeDropped: write };
      return write && !s.writeDropped ? { ...s, writeDropped: true } : s;
    });
  }, []);
  const reset = React.useCallback(() => setState({ phase: "none", writeDropped: false }), []);
  // Stable identity: useWriteDropped's effect depends on it.
  const claimWriteDropped = React.useCallback(() => {
    claims.current += 1;
    return () => {
      claims.current -= 1;
    };
  }, []);

  const reauth = React.useMemo<Reauth>(
    () => ({
      ...state,
      open: () => setState((s) => ({ ...s, phase: "dialog" })),
      notNow: () => setState((s) => ({ ...s, phase: "bar" })),
      resume: () => setState((s) => ({ ...s, phase: "none" })),
      reloadAs,
      clearWriteDropped: () => setState((s) => (s.writeDropped ? { ...s, writeDropped: false } : s)),
      writeDroppedClaimed: () => claims.current > 0,
      claimWriteDropped,
    }),
    [state, reloadAs, claimWriteDropped],
  );
  return { reauth, lapse, reset };
}

