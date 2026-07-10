/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// HostStatusBar — one persistent host-status strip replacing per-step re-check
// buttons: a single aria-live in-flight state driven purely by props
// (checking / lastCheckedLabel / onRecheck), which the orchestrator wires to its
// re-check so a host re-probe anywhere surfaces "Checking…" here.
import { RotateCw, Loader2, Server } from "lucide-react";
import { Button } from "../../ui/button";
import { cn } from "../../ui/utils";
import { BTN } from "../../wardyn/copy";

export function HostStatusBar({
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
    <div
      className={cn(
        "flex items-center justify-between gap-3 rounded-lg border bg-muted/50 px-3 py-2",
        className,
      )}
    >
      <div className="flex items-center gap-2 text-sm text-muted-foreground" aria-live="polite">
        <Server className="size-4 shrink-0" aria-hidden />
        {checking ? (
          <span className="inline-flex items-center gap-1.5 text-info">
            <Loader2 className="size-3.5 animate-spin" aria-hidden />
            Checking Wardyn's setup…
          </span>
        ) : (
          <span>
            Host status ·{" "}
            <span className="text-foreground">{lastCheckedLabel}</span>
          </span>
        )}
      </div>
      <Button variant="outline" size="sm" onClick={onRecheck} disabled={checking}>
        <RotateCw className={cn("size-3.5", checking && "animate-spin")} aria-hidden />
        {BTN.recheck}
      </Button>
    </div>
  );
}
