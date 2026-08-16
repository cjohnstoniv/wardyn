/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// useRunLayout — the cockpit canvas's layout state: load the caller's saved
// arrangement for one preset, keep it optimistically local while they drag,
// and write it back on a debounce.
//
// Three rules this hook exists to hold:
//  1. A SAVED layout beats the situational default, always. An empty saved
//     layout is "never saved" (a 200 with layout: []), not a failure, so it
//     falls through to the preset default — and so does any GET error, since a
//     cockpit with no widgets is worse than one arranged by the machine.
//  2. A 501 means this deployment's store cannot persist layouts at all. That
//     is a DEPLOYMENT fact, not a user error: remember it, stop writing, and
//     let the session keep working. No toast, or every drag would fire one.
//  3. Nothing here ever writes on mount. Only an explicit gesture (drag,
//     resize, catalog click, reset, save) schedules a PUT — an auto-save on
//     load would give every human a "saved layout" the instant they opened a
//     run, and they would never see a default improve again.
import * as React from "react";
import { HttpError } from "../../../lib/api/core";
import {
  runLayout as runLayoutApi,
  type RunLayoutPreset,
  type RunLayoutWidget,
} from "../../../lib/api/run-layout";
import { normalizeLayout, presetLayout } from "./widget-registry";

// Long enough that a drag across the canvas is one PUT, short enough that a
// human who drags and immediately closes the tab keeps the arrangement (and
// the unmount flush below covers the rest).
const SAVE_DEBOUNCE_MS = 800;

export type RunLayoutState = {
  layout: RunLayoutWidget[];
  /** The GET has settled. The layout is usable before this (the preset
   *  default), so this only reports whether a saved one is still in flight. */
  loaded: boolean;
  /** False once the server answered 501: in-session only from here on. */
  persistable: boolean;
  /** Optimistic local update + a debounced PUT. */
  apply: (next: RunLayoutWidget[]) => void;
  /** Flush now. Resolves true when the server stored it. */
  save: () => Promise<boolean>;
  /** Back to the preset default, and clear the saved row (PUT []). */
  reset: () => void;
};

export function useRunLayout(preset: RunLayoutPreset): RunLayoutState {
  // Seeded with the preset default rather than empty: the canvas can paint the
  // machine's arrangement immediately and swap once a saved one arrives, which
  // beats one round-trip of empty canvas for the majority who never saved one.
  const [layout, setLayout] = React.useState<RunLayoutWidget[]>(() => presetLayout(preset));
  const [loaded, setLoaded] = React.useState(false);
  const [persistable, setPersistable] = React.useState(true);

  const layoutRef = React.useRef(layout);
  const persistRef = React.useRef(true);
  const timer = React.useRef<ReturnType<typeof setTimeout> | null>(null);

  const commit = (next: RunLayoutWidget[]) => {
    layoutRef.current = next;
    setLayout(next);
  };

  const cancelPending = () => {
    if (timer.current) {
      clearTimeout(timer.current);
      timer.current = null;
    }
  };

  // Bound to THIS preset on purpose — see the flush effect at the bottom, which
  // uses that binding to finish a pending write against the preset it was made
  // under when the caller switches presets mid-edit.
  const put = React.useCallback(
    async (body: RunLayoutWidget[]): Promise<boolean> => {
      if (!persistRef.current) return false;
      try {
        await runLayoutApi.putLayout(preset, body);
        return true;
      } catch (err) {
        if (err instanceof HttpError && err.status === 501) {
          persistRef.current = false;
          setPersistable(false);
        }
        // Any other failure stays retryable: the next gesture writes again.
        return false;
      }
    },
    [preset],
  );

  const apply = React.useCallback(
    (next: RunLayoutWidget[]) => {
      const norm = normalizeLayout(next, preset);
      commit(norm);
      cancelPending();
      timer.current = setTimeout(() => {
        timer.current = null;
        void put(norm);
      }, SAVE_DEBOUNCE_MS);
    },
    [preset, put],
  );

  const save = React.useCallback(async (): Promise<boolean> => {
    cancelPending();
    return put(layoutRef.current);
  }, [put]);

  const reset = React.useCallback(() => {
    cancelPending();
    commit(presetLayout(preset));
    // PUT [] is the reset: there is no DELETE route, and storing today's
    // default as a personal choice would freeze this human out of every future
    // improvement to it. An empty saved layout reads as "never saved" on load.
    void put([]);
  }, [preset, put]);

  React.useEffect(() => {
    let alive = true;
    setLoaded(false);
    runLayoutApi
      .getLayout(preset)
      .then((res) => {
        if (!alive) return;
        commit(res.layout.length > 0 ? normalizeLayout(res.layout, preset) : presetLayout(preset));
        setLoaded(true);
      })
      .catch(() => {
        if (!alive) return;
        commit(presetLayout(preset));
        setLoaded(true);
      });
    return () => {
      alive = false;
    };
  }, [preset]);

  // Finish a debounced write the human never saw land — on unmount (they
  // navigated away mid-debounce) and on a preset switch, where the cleanup
  // still holds the PREVIOUS preset's `put` and the layout made under it.
  React.useEffect(
    () => () => {
      if (timer.current) {
        clearTimeout(timer.current);
        timer.current = null;
        void put(layoutRef.current);
      }
    },
    [put],
  );

  return { layout, loaded, persistable, apply, save, reset };
}
