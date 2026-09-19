/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/**
 * useTerminalFullscreen drives AttachTerminal's fullscreen toggle. Split out of
 * attach-terminal.tsx purely to keep that file under its 1000-line cap
 * (scripts/check-file-size.sh, split-by-seam, never allowlist) — this hook has
 * exactly one caller and is not meant to grow a second.
 */
import * as React from "react";
import { modalLayerOpen } from "../lib/modal-layer-open";

export function useTerminalFullscreen(panelRef: React.RefObject<HTMLDivElement | null>, refit: () => void) {
  const [fullscreen, setFullscreen] = React.useState(false);

  // Refit shortly after entering/leaving fullscreen (the box just changed).
  React.useEffect(() => {
    const id = requestAnimationFrame(() => refit());
    const t = setTimeout(() => refit(), 60);
    return () => {
      cancelAnimationFrame(id);
      clearTimeout(t);
    };
  }, [fullscreen, refit]);

  // Fullscreen uses the NATIVE Fullscreen API, not a `fixed inset-0` overlay.
  //
  // The CSS approach cannot be made reliable: `position: fixed` is resolved
  // against the nearest ancestor that establishes a containing block, and the
  // list of things that do is long and growing — transform, filter,
  // backdrop-filter, perspective, contain, will-change, and (Tailwind v4's
  // default for translate-x-*) the INDIVIDUAL `translate` property, which
  // `transform: none` does not reset. Measured on the login dialog: computed
  // `transform: none` yet `translate: -50% -50%`, and a `fixed inset-0` child
  // still sized to the dialog rather than the viewport.
  //
  // requestFullscreen promotes the element to the browser's TOP LAYER, which
  // sits outside the whole containing-block question, so this works identically
  // inside a dialog, a card, or a page. It also gives real fullscreen — over the
  // browser chrome, not just the page — and the browser handles Escape itself
  // (WCAG 2.1.2), so there is no key handler to fight xterm's textarea for.
  const toggleFullscreen = React.useCallback(() => {
    const el = panelRef.current;
    if (!el) return;
    if (document.fullscreenElement === el) {
      void document.exitFullscreen().catch(() => {});
      return;
    }
    const req = el.requestFullscreen?.bind(el);
    if (!req) {
      // No API (very old browser, or a sandboxed iframe without
      // allow-fullscreen): fall back to the in-page overlay. It is still
      // subject to the containing-block rules above, so it may only fill an
      // ancestor — degraded, never broken.
      setFullscreen((f) => !f);
      return;
    }
    void req().catch(() => setFullscreen((f) => !f));
  }, [panelRef]);

  // The browser owns the truth: Escape, F11 and the OS window chrome can all
  // leave fullscreen without going through our button.
  React.useEffect(() => {
    const sync = () => setFullscreen(document.fullscreenElement === panelRef.current);
    document.addEventListener("fullscreenchange", sync);
    return () => document.removeEventListener("fullscreenchange", sync);
  }, [panelRef]);

  // Escape exits the FALLBACK overlay (WCAG 2.1.2, no keyboard trap). Native
  // fullscreen needs no help — the browser exits on Escape before the page sees
  // the key — so this only binds when we are overlaying rather than promoted.
  // Capture phase, so it runs before xterm's textarea swallows the key and
  // sends it to the PTY as literal input.
  React.useEffect(() => {
    if (!fullscreen || document.fullscreenElement) return;
    const onKeyDown = (e: KeyboardEvent) => {
      // R-4: yield to a dialog's own Escape-dismiss (F1-F3's sibling here).
      if (e.key === "Escape" && !e.ctrlKey && !e.metaKey && !e.shiftKey && !e.altKey && !modalLayerOpen()) {
        e.preventDefault();
        e.stopPropagation();
        setFullscreen(false);
      }
    };
    document.addEventListener("keydown", onKeyDown, true);
    return () => document.removeEventListener("keydown", onKeyDown, true);
  }, [fullscreen]);

  return { fullscreen, toggleFullscreen };
}
