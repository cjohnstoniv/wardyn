/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/**
 * TerminalConnectionStatus — #216. Connection state (a live reconnect attempt,
 * or the bounded reconnect budget spent) is not program output, so it never
 * goes into xterm's own scrollback the way `[closed]` and friends used to
 * (attach-terminal.tsx's onclose handler). It renders here instead, in a strip
 * below the grid that persists for as long as the state does — printed output
 * can push it off the BOTTOM of the terminal's own scroll region, but it can
 * never scroll this strip out of view, and a spent budget always leaves a
 * Reconnect button behind rather than a dead end.
 *
 * Split out of attach-terminal.tsx purely to keep that file under its line cap
 * (see CONTRIBUTING.md "Large files"); it owns no state of its own.
 */
import { CircleAlert, Loader2, RotateCw } from "lucide-react";
import { Button } from "./ui/button";
import { TERMINAL } from "./wardyn/copy";

export interface TerminalConnectionStatusProps {
  /**
   * "reconnecting" — a retry is already in flight; nothing to press.
   * "closed" — the bounded reconnect budget (MAX_RECONNECT_ATTEMPTS) is spent
   * and nothing further will happen on its own; offers Reconnect.
   */
  state: "reconnecting" | "closed";
  /** The reconnect attempt currently in flight (1-based), for "reconnecting". */
  attempt: number;
  /** The reconnect budget (attach-terminal.tsx's MAX_RECONNECT_ATTEMPTS). */
  maxAttempts: number;
  /** Drops the current socket, resets the budget, and attaches again. */
  onReconnect: () => void;
}

export function TerminalConnectionStatus({
  state,
  attempt,
  maxAttempts,
  onReconnect,
}: TerminalConnectionStatusProps) {
  const reconnecting = state === "reconnecting";
  return (
    <div
      role="status"
      className="flex flex-wrap items-center gap-3 border-t border-border bg-card/60 px-3 py-2.5"
    >
      {reconnecting ? (
        <Loader2 className="size-4 shrink-0 animate-spin text-muted-foreground" />
      ) : (
        <CircleAlert className="size-4 shrink-0 text-muted-foreground" />
      )}
      <div className="min-w-0 flex-1">
        <p className="text-sm font-medium text-foreground">
          {reconnecting ? TERMINAL.RECONNECTING_LINE(attempt, maxAttempts) : TERMINAL.CLOSED_TITLE}
        </p>
        <p className="mt-0.5 text-xs text-muted-foreground">
          {reconnecting ? TERMINAL.RECONNECTING_HINT : TERMINAL.CLOSED_BODY(maxAttempts)}
        </p>
      </div>
      {!reconnecting && (
        <Button variant="outline" size="sm" onClick={onReconnect}>
          <RotateCw className="size-3.5" />
          {TERMINAL.RECONNECT}
        </Button>
      )}
    </div>
  );
}
