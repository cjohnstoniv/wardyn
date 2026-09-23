/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the mounted screens have typed and not saved, as readable text — read
// by "Copy my changes" wherever the console offers it outside the screen that
// owns the draft (the signed-out bar and dialog, #483). Module state, not
// context: the reader sits above the screens, and it only ever reads on a
// click or a render it already caused. Memory only — never browser storage.
import * as React from "react";

const registry = new Map<string, () => string>();

/** Registers a draft's text under `id`; returns the unregister function. */
export function registerUnsaved(id: string, getText: () => string): () => void {
  registry.set(id, getText);
  return () => {
    if (registry.get(id) === getText) registry.delete(id);
  };
}

/** Every registered draft's text, joined by a blank line; null when none is registered. */
export function unsavedSnapshot(): string | null {
  if (registry.size === 0) return null;
  return [...registry.values()].map((getText) => getText()).join("\n\n");
}

/** Registers `getText` under `id` while `dirty`; the latest `getText` is always the one read. */
export function useRegisterUnsaved(id: string, dirty: boolean, getText: () => string): void {
  const latest = React.useRef(getText);
  React.useEffect(() => {
    latest.current = getText;
  });
  React.useEffect(() => {
    if (!dirty) return;
    return registerUnsaved(id, () => latest.current());
  }, [id, dirty]);
}
