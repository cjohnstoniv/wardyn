/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";

// usePoll — a small auto-refresh primitive. Invokes `fn` every `intervalMs`
// while `paused` is false, so a "live" view (the Runs board, ~3s) never goes stale.
//
// Design notes:
//  - The latest `fn` is read through a ref, so the interval doesn't reset (and
//    miss a tick) every render when the caller passes an inline closure.
//  - `paused` is read through a ref for the same reason: pausing/resuming flips
//    the ref without tearing down and rebuilding the timer. Callers pause polling
//    while a modal / drawer is open so an in-flight edit isn't yanked out from
//    under the user (and so the detail panel's own loads don't race the poll).
//  - This drives BACKGROUND refreshes only; the caller still does its own initial
//    load so it can show a loading skeleton. Pass intervalMs <= 0 to disable.
//
// TWO THINGS EVERY CALLER USED TO PAY FOR, fixed here rather than in eleven
// call sites (R4-F073/F074):
//
//  (1) NO IN-FLIGHT GUARD. A bare setInterval has no notion of whether the
//      previous invocation is still outstanding, so when the daemon is slower
//      than the interval each tick stacked another full set of requests on the
//      previous ones — measured at 34 concurrent in-flight on /runs/:id with a
//      12s backend, and 97 with a 40s one. Browsers cap ~6 connections per
//      origin on HTTP/1.1, so the surplus queued in the browser and delayed
//      every foreground action the operator took on that tab: the console added
//      the most load exactly when the daemon was least able to serve it. A tick
//      that arrives while the previous one is unsettled is now DROPPED, not
//      queued — a poll is "show me the current state", and a stale duplicate of
//      a request already in flight has no value to catch up on.
//
//  (2) A HIDDEN TAB POLLED AT FULL CADENCE. Every open cockpit tab kept its
//      pollers running while backgrounded; the human cannot see any of it. Ticks
//      are skipped while document.hidden, and returning to the tab fires one
//      immediately so the view is fresh on the first frame the human sees rather
//      than up to intervalMs later.
//
// `fn` may return a promise; when it does, that promise is what the in-flight
// guard waits on. A caller whose fn returns void keeps exactly today's
// behaviour (nothing to wait for, so nothing is ever skipped) — which is why
// the cockpit's load() returns its Promise.all chain.
export function usePoll(fn: () => void | Promise<unknown>, intervalMs: number, paused: boolean): void {
  const fnRef = React.useRef(fn);
  React.useEffect(() => {
    fnRef.current = fn;
  });

  const pausedRef = React.useRef(paused);
  React.useEffect(() => {
    pausedRef.current = paused;
  }, [paused]);

  const inFlight = React.useRef(false);

  // One tick. Kept in a ref so the visibility listener and the interval invoke
  // the SAME guarded call rather than two copies of the rule.
  const tick = React.useRef(() => {});
  tick.current = () => {
    if (pausedRef.current || inFlight.current) return;
    // A hidden tab shows nobody anything; the visibilitychange handler below
    // catches the view up the moment it is looked at again.
    if (typeof document !== "undefined" && document.hidden) return;
    let result: void | Promise<unknown>;
    try {
      result = fnRef.current();
    } catch {
      // A synchronous throw is the caller's business, not a reason to wedge the
      // guard shut for the life of the mount.
      return;
    }
    if (!result || typeof (result as Promise<unknown>).then !== "function") return;
    inFlight.current = true;
    void (result as Promise<unknown>).then(
      () => {
        inFlight.current = false;
      },
      () => {
        // A REJECTED poll clears the guard too: a failing endpoint must not
        // freeze this view on its last-good data forever.
        inFlight.current = false;
      },
    );
  };

  React.useEffect(() => {
    if (!Number.isFinite(intervalMs) || intervalMs <= 0) return;
    const id = setInterval(() => tick.current(), intervalMs);
    if (typeof document === "undefined") return () => clearInterval(id);
    // Coming back to the tab refreshes NOW: the alternative is a human staring
    // at up to intervalMs of state that was frozen while they were away.
    const onVisible = () => {
      if (!document.hidden) tick.current();
    };
    document.addEventListener("visibilitychange", onVisible);
    return () => {
      clearInterval(id);
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, [intervalMs]);
}
