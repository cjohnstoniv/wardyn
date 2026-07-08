/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";

// usePoll — a small auto-refresh primitive. Invokes `fn` every `intervalMs`
// while `paused` is false, so a "live" board (the Fleet view) never goes stale.
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
export function usePoll(fn: () => void, intervalMs: number, paused: boolean): void {
  const fnRef = React.useRef(fn);
  React.useEffect(() => {
    fnRef.current = fn;
  });

  const pausedRef = React.useRef(paused);
  React.useEffect(() => {
    pausedRef.current = paused;
  }, [paused]);

  React.useEffect(() => {
    if (!Number.isFinite(intervalMs) || intervalMs <= 0) return;
    const id = setInterval(() => {
      if (!pausedRef.current) fnRef.current();
    }, intervalMs);
    return () => clearInterval(id);
  }, [intervalMs]);
}
