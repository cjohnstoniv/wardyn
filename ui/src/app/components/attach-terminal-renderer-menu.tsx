/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/**
 * The renderer menu in the terminal title bar (M11 S4) and the strip shown when
 * the GPU renderer stopped (M11 S5). Wording is canon: TERMINAL_RENDERER.
 */
import { CircleAlert, MonitorCog } from "lucide-react";
import { Button } from "./ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuTrigger,
} from "./ui/dropdown-menu";
import { webgl2Available, type ActiveRenderer, type RendererPref } from "./attach-terminal-renderer";
import { TERMINAL_COPY, TERMINAL_RENDERER } from "./wardyn/copy";

function Item({ value, label, hint, disabled }: { value: RendererPref; label: string; hint: string; disabled?: boolean }) {
  return (
    <DropdownMenuRadioItem value={value} disabled={disabled} className="items-start">
      <span className="flex flex-col">
        <span>{label}</span>
        <span className="text-xs text-muted-foreground">{hint}</span>
      </span>
    </DropdownMenuRadioItem>
  );
}

export function RendererMenu({
  pref,
  active,
  onPick,
}: {
  pref: RendererPref;
  active: ActiveRenderer;
  onPick: (pref: RendererPref) => void;
}) {
  const gpuOk = webgl2Available();
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <button
          type="button"
          onMouseDown={(e) => e.preventDefault()}
          title={TERMINAL_RENDERER.LABEL}
          aria-label={TERMINAL_RENDERER.LABEL}
          data-testid="terminal-renderer-menu"
          className="rounded p-1 text-muted-foreground hover:bg-muted hover:text-foreground"
        >
          <MonitorCog className="size-3.5" />
        </button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-80">
        <DropdownMenuLabel className="text-meta font-medium uppercase tracking-wide text-muted-foreground">
          {TERMINAL_RENDERER.LABEL}
        </DropdownMenuLabel>
        <DropdownMenuRadioGroup value={pref} onValueChange={(v) => onPick(v as RendererPref)}>
          <Item value="auto" label={TERMINAL_RENDERER.AUTO} hint={TERMINAL_RENDERER.AUTO_HINT} />
          <Item
            value="gpu"
            label={TERMINAL_RENDERER.GPU}
            hint={gpuOk ? TERMINAL_RENDERER.GPU_HINT : TERMINAL_RENDERER.GPU_UNAVAILABLE}
            disabled={!gpuOk}
          />
          <Item value="compatible" label={TERMINAL_RENDERER.COMPATIBLE} hint={TERMINAL_RENDERER.COMPATIBLE_HINT} />
        </DropdownMenuRadioGroup>
        <p data-testid="terminal-renderer-footer" className="px-2 py-1.5 text-xs text-muted-foreground">
          {TERMINAL_RENDERER.FOOTER(active === "gpu" ? TERMINAL_RENDERER.GPU : TERMINAL_RENDERER.COMPATIBLE)}
        </p>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

/** The fell-back strip, in the connection-status slot below the grid. */
export function RendererFellBackNotice({ onDismiss }: { onDismiss: () => void }) {
  return (
    <div
      role="status"
      data-testid="terminal-renderer-notice"
      className="flex flex-wrap items-center gap-3 border-t border-border bg-card/60 px-3 py-2.5"
    >
      <CircleAlert className="size-4 shrink-0 text-muted-foreground" />
      <p className="min-w-0 flex-1 text-sm font-medium text-foreground">{TERMINAL_RENDERER.FELL_BACK}</p>
      <Button variant="ghost" size="sm" onClick={onDismiss}>
        {TERMINAL_COPY.DISMISS}
      </Button>
    </div>
  );
}
