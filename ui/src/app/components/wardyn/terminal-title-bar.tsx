/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";

// #3078: the title bar both terminals share: the three window dots, an optional title, then the caller's controls.
// `dark` gives the tokens inside it (border, chips, muted hints) their dark values in both themes, because the bar
// sits on the near-black terminal; the run pane frame does the same (#2910).
export function TerminalTitleBar({ title, children }: { title?: React.ReactNode; children?: React.ReactNode }) {
  return (
    <div data-testid="terminal-title-bar" className="dark flex items-center gap-1.5 border-b border-border bg-terminal-chrome px-3 py-2">
      <span className="size-3 rounded-full bg-terminal-dot-close" />
      <span className="size-3 rounded-full bg-terminal-dot-minimize" />
      <span className="size-3 rounded-full bg-terminal-dot-zoom" />
      {title !== undefined && (
        <span className="ml-3 truncate font-mono text-xs text-terminal-chrome-foreground">{title}</span>
      )}
      {children}
    </div>
  );
}
