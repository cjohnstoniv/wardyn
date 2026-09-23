/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #460 — the one registry a dirty editor tells "I have unsaved work, and
// here is its text": use-unsaved-guard.tsx reads unsavedSnapshot() to decide
// whether to block navigation, and a sibling branch (#483, forced reauth)
// reads the same registry to see what would be lost before it happens.
// Plain module state, not React context, on purpose — a caller outside the
// component tree (an axios interceptor, a beforeunload listener) can read
// unsavedSnapshot() with no provider above it. Kept minimal, no other
// exports: a second concern here is a second thing every consumer has to
// agree not to depend on.
import * as React from "react";

const registry = new Map<string, () => string>();

/** Registers `getText` under `id`; a second call with the same `id`
 *  overwrites the first — one editor, one entry. Returns the unregister. */
export function registerUnsaved(id: string, getText: () => string): () => void {
  registry.set(id, getText);
  return () => {
    registry.delete(id);
  };
}

/** null when nothing is registered (nothing unsaved anywhere); otherwise
 *  every registered editor's own text, in registration order, joined by a
 *  blank line. */
export function unsavedSnapshot(): string | null {
  if (registry.size === 0) return null;
  return Array.from(registry.values(), (getText) => getText()).join("\n\n");
}

/** Registers `id` -> `getText` ONLY while `dirty` — a clean editor has
 *  nothing worth losing, so it has no business in the registry. `getText` is
 *  read through a ref rather than a direct effect dependency, so a caller
 *  passing a fresh closure every render (the common case: it closes over the
 *  latest draft) doesn't re-register on every keystroke — only `id`/`dirty`
 *  changing does. */
export function useRegisterUnsaved(id: string, dirty: boolean, getText: () => string): void {
  const getTextRef = React.useRef(getText);
  getTextRef.current = getText;
  React.useEffect(() => {
    if (!dirty) return undefined;
    return registerUnsaved(id, () => getTextRef.current());
  }, [id, dirty]);
}
