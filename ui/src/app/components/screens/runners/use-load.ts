/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The read loop the Runners page and its Pools tab share: one sequence-guarded read, an initial load, and a
// silent re-read every `pollMs` through usePoll (paused while the tab is hidden or the console is signed out).
// A newer read, or leaving the page, discards a read still in flight, so a late answer never sets state.
// `fetch` does the request; `ok` and `fail` apply its outcome and are called only for the read that is
// still current. `start` runs before a loud (non-silent) read, to show "loading".
import * as React from "react";
import { usePoll } from "../../../lib/use-poll";

export interface LoadHandlers<T> {
  fetch: (silent: boolean) => Promise<T>;
  start: () => void;
  ok: (result: T) => void;
  fail: (e: unknown, silent: boolean) => void;
}

export function useLoad<T>(handlers: LoadHandlers<T>, pollMs: number): (silent: boolean) => Promise<void> {
  const seq = React.useRef(0);
  // Read through a ref: `read` is built once, and callers pass new closures each render.
  const h = React.useRef(handlers);
  h.current = handlers;

  const read = React.useCallback((silent: boolean) => {
    const mine = ++seq.current;
    if (!silent) h.current.start();
    return h.current.fetch(silent).then(
      (r) => {
        if (mine === seq.current) h.current.ok(r);
      },
      (e: unknown) => {
        if (mine === seq.current) h.current.fail(e, silent);
      },
    );
  }, []);

  const abandon = React.useCallback(() => {
    seq.current++;
  }, []);
  React.useEffect(() => {
    void read(false);
    return abandon;
  }, [read, abandon]);
  usePoll(() => read(true), pollMs, false);
  return read;
}
