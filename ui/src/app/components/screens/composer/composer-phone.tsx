/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The composer under 760 px: the chat takes the screen, the draft folds into one bar, and "Show draft" opens it as a
// full-height sheet. Nothing is accepted or saved from the chat.
import * as React from "react";
import { Button } from "../../ui/button";
import { ASSISTANT as T } from "../../wardyn/copy/assistant";

const PHONE_QUERY = "(max-width: 759px)";

/** True under 760 px. The console's own breakpoint; jsdom has no matchMedia, so the window width answers there. */
export function useIsPhone(): boolean {
  const read = () =>
    typeof window.matchMedia === "function" ? window.matchMedia(PHONE_QUERY).matches : window.innerWidth < 760;
  const [phone, setPhone] = React.useState(read);
  React.useEffect(() => {
    const update = () => setPhone(read());
    update();
    window.addEventListener("resize", update);
    const mq = typeof window.matchMedia === "function" ? window.matchMedia(PHONE_QUERY) : null;
    mq?.addEventListener?.("change", update);
    return () => {
      window.removeEventListener("resize", update);
      mq?.removeEventListener?.("change", update);
    };
  }, []);
  return phone;
}

/** The one bar the draft folds into: how many lines it holds and how many hosts were left out. */
export function PhoneDraftBar({ lines, left, onShow }: { lines: number; left: number; onShow: () => void }) {
  return (
    <div
      data-testid="composer-draft-bar"
      className="flex items-center justify-between gap-2 rounded-lg border border-warning/35 bg-warning-subtle px-2.5 py-2"
    >
      <span className="min-w-0 text-body text-foreground">{T.DRAFT_BAR(lines, left)}</span>
      <Button type="button" variant="outline" size="sm" onClick={onShow}>
        {T.SHOW_DRAFT}
      </Button>
    </div>
  );
}

/** The draft as a full-height sheet over the chat. */
export function PhoneDraftSheet({ open, onClose, children }: { open: boolean; onClose: () => void; children: React.ReactNode }) {
  if (!open) return null;
  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-label={T.SHOW_DRAFT}
      data-testid="composer-draft-sheet"
      className="fixed inset-0 z-50 flex flex-col gap-3 overflow-y-auto bg-background p-4"
    >
      <div>
        <Button type="button" variant="outline" size="sm" onClick={onClose}>
          {T.HIDE_DRAFT}
        </Button>
      </div>
      {children}
    </div>
  );
}
