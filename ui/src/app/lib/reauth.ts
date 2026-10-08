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
//
// It also holds a renewal: the expiry banner's "Sign in again", started while
// the session is still live (lib/use-session-renew.ts), which the same layer
// draws as a strip in the banner's place — and a watch: a renewal the person
// backed out of, whose sign-in can still complete.
import * as React from "react";
import { notifyAuthChange, type Refused } from "./api/core";

// none: signed in. dialog: signed out, asking. bar: signed out, "Not now".
// renew: still signed in, signing in again from the expiry banner.
export type ReauthPhase = "none" | "dialog" | "bar" | "renew";

/** What a renewal started from: who was signed in, with what authority and
 *  until when. Whoever answers /me while it waits is measured against this. */
export interface Renewal {
  principal: string;
  role: string;
  operator: boolean;
  securityOperator: boolean;
  /** session_expires_at at the click, in epoch milliseconds. */
  expiresAt: number;
  /** The sign-in window the click opened; null when the browser refused it. */
  popup: Window | null;
}

/** A cancelled renewal whose sign-in can still complete: who it started from,
 *  and until when (epoch milliseconds) the server would still honour it. */
export interface Watch {
  from: Renewal;
  until: number;
}

export interface Reauth {
  phase: ReauthPhase;
  /** Signed out mid-page: the dialog, the bar, or a renewal under which a
   *  request was refused. A renewal alone leaves the page working. */
  signedOut: boolean;
  /** A deliberate logout retires every pending identity confirmation before its next render. */
  endingSession?: () => boolean;
  /** The renewal in progress, or null. */
  renewal: Renewal | null;
  /** Set while a cancelled renewal's sign-in can still land in this browser:
   *  the layer keeps measuring whoever answers /me until the watch ends. */
  watch: Watch | null;
  /** The owner id (WfetchInit.save) of a Save refused in this lapse — never
   *  re-sent — or null. Only that owner's screen ever shows it. */
  writeDropped: string | null;
  /** "dialog": ask (again). "bar": Not now. "none": the same person is back
   *  and the page carries on. */
  setPhase: (phase: Exclude<ReauthPhase, "renew">) => void;
  setWatch: (watch: Watch | null) => void;
  /** The banner's "Sign in again": the click has already opened the window. */
  startRenew: (renewal: Renewal) => void;
  /** Cancel: back to the banner, or to the dialog when the session ended
   *  while the renewal waited. */
  endRenew: () => void;
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
  signedOut: false,
  renewal: null,
  watch: null,
  writeDropped: null,
  setPhase: noop,
  setWatch: noop,
  startRenew: noop,
  endRenew: noop,
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
  renewal: Renewal | null;
  watch: Watch | null;
  /** A request was refused (401) since the person was last known signed in. */
  refused: boolean;
}
const SIGNED_IN: State = { phase: "none", writeDropped: null, renewal: null, watch: null, refused: false };

export function useReauthController(reloadAs: (path: string) => void, endingSession?: () => boolean): {
  reauth: Reauth;
  lapse: (refused: Refused) => void;
  reset: () => void;
} {
  const [state, setState] = React.useState<State>(SIGNED_IN);
  // One mounted screen per Save owner (an owner is a screen's own resource).
  const claims = React.useRef(new Set<string>());

  // Functional updates throughout: several requests can 401 in one tick, and
  // each must see the phase the one before it set. A write refused under the
  // bar asks again — the person just tried to change something. Under a
  // renewal the strip stays, and its sign-in window serves this refusal too.
  const lapse = React.useCallback(({ write, save }: Refused) => {
    setState((s) => {
      if (s.phase === "none") return { ...s, phase: "dialog", writeDropped: save ?? null, refused: true };
      const phase = write && s.phase === "bar" ? "dialog" : s.phase;
      return { ...s, phase, writeDropped: save ?? s.writeDropped, refused: true };
    });
  }, []);
  const reset = React.useCallback(() => setState(SIGNED_IN), []);
  // Stable identity: the layer's watch effect depends on it.
  const setWatch = React.useCallback((watch: Watch | null) => setState((s) => ({ ...s, watch })), []);
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
      phase: state.phase,
      endingSession,
      writeDropped: state.writeDropped,
      renewal: state.renewal,
      watch: state.watch,
      signedOut: state.phase !== "none" && (state.phase !== "renew" || state.refused),
      setPhase: (phase) =>
        setState((s) => ({ ...s, phase, renewal: null, refused: phase === "none" ? false : s.refused })),
      setWatch,
      startRenew: (renewal) => {
        notifyAuthChange();
        setState((s) => ({ ...s, phase: "renew", renewal }));
      },
      endRenew: () => {
        notifyAuthChange();
        setState((s) => (s.phase === "renew" ? { ...s, phase: s.refused ? "dialog" : "none", renewal: null } : s));
      },
      reloadAs,
      clearWriteDropped: () => setState((s) => (s.writeDropped ? { ...s, writeDropped: null } : s)),
      writeDroppedClaimed: () => claims.current.has(state.writeDropped ?? ""),
      claimWriteDropped,
    }),
    [state, reloadAs, setWatch, claimWriteDropped, endingSession],
  );
  return { reauth, lapse, reset };
}
