/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The window on the Mac a macOS VM run may ask for as it starts (`desktop_window: at_start`). The option shows only
// while the site switch is on (setup status `features.mac_window`); absent reads off, so an untouched install shows
// nothing. A server refusal that names the field clears the choice, says so, and the draft is checked again without
// it, so Launch is never held by a window the person can't have. A refusal is matched by its field only: its reasons
// (governance_unit, desktop_sign_ins_off) are shared with refusals that have nothing to do with the window.

import * as React from "react";
import type { DraftAction } from "../../draft/draft-reducer";
import { DESKTOP_WINDOW_FIELD } from "../../draft/field-homes";
import type { RunIntent } from "../../draft/run-intent";
import { withKeys } from "../../lenses/lens-keys";
import { useNewRunDispatch, useNewRunDraft } from "../../new-run-route";

/** The site switch, from the setup status; absent or false is off. */
export const macWindowOn = (status: { features?: { mac_window?: boolean } } | null | undefined): boolean =>
  status?.features?.mac_window === true;

/** Ticks or unticks the window. Unticked is the absent key. */
export const chooseWindow = (intent: RunIntent, on: boolean): RunIntent => withKeys(intent, { desktop_window: on ? "at_start" : undefined });

/** The refusal's sentence when it is about the window and the draft still asks for one; undefined otherwise. */
export function windowRefusal(intent: RunIntent, refusal: { field: string; text: string | null } | null): string | undefined {
  if (!refusal || refusal.field !== DESKTOP_WINDOW_FIELD || refusal.text === null || !intent.desktop_window) return undefined;
  return refusal.text;
}

/** Clears a window the server refused, once per refusal, and keeps its sentence for the notice (W2-06). */
export function useClearRefusedWindow(refusal: { field: string; text: string | null } | null) {
  const { intent } = useNewRunDraft();
  const dispatch = useNewRunDispatch();
  const text = windowRefusal(intent, refusal);
  React.useEffect(() => {
    if (text === undefined) return;
    dispatch({ type: "edit", apply: (i) => chooseWindow(i, false) });
    dispatch({ type: "guided", patch: { windowCleared: text } });
  }, [text, dispatch]);
}

/** Ticks or unticks the window from a step or Review: a new choice takes the old notice away. */
export function chooseWindowIn(dispatch: (action: DraftAction) => void, on: boolean) {
  dispatch({ type: "edit", apply: (i) => chooseWindow(i, on) });
  dispatch({ type: "guided", patch: { windowCleared: undefined } });
}
