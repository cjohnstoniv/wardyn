/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// HostRecheck — the setup funnel's one Re-check, at the right end of every step's eyebrow row (#2847). It replaces the
// full-width "Host status" bar, which never showed a status: only a time, and only after a forced re-check had moved
// the answer. Driven purely by props (checking / lastCheckedLabel / onRecheck), which the orchestrator wires to its
// forced re-check, so a re-probe anywhere surfaces "Checking Wardyn's setup…" here.
import { RotateCw, Loader2 } from "lucide-react";
import { Button } from "../../ui/button";
import { cn } from "../../ui/utils";
import { BTN } from "../../wardyn/copy";

export function HostRecheck({
  checking,
  lastCheckedLabel,
  onRecheck,
  className,
}: {
  checking: boolean;
  lastCheckedLabel: string;
  onRecheck: () => void;
  className?: string;
}) {
  return (
    <div className={cn("flex shrink-0 items-center gap-3", className)}>
      {/* The live region is always mounted, so a text that appears later is announced. An empty label renders nothing:
          the mount read is answered from the daemon's memo, so no time is stamped until a forced re-check lands (U2-02). */}
      <span className="text-sm text-muted-foreground" aria-live="polite">
        {checking ? (
          <span className="inline-flex items-center gap-1.5 text-info">
            <Loader2 className="size-3.5 animate-spin" aria-hidden />
            Checking Wardyn's setup…
          </span>
        ) : (
          lastCheckedLabel && <span className="text-foreground">{lastCheckedLabel}</span>
        )}
      </span>
      <Button variant="outline" size="sm" onClick={onRecheck} disabled={checking}>
        <RotateCw className={cn("size-3.5", checking && "animate-spin")} aria-hidden />
        {BTN.recheck}
      </Button>
    </div>
  );
}
