/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { AlertTriangle, RotateCw } from "lucide-react";
import { Button } from "../ui/button";
import { cn } from "../ui/utils";

export function EmptyState({
  icon: Icon,
  title,
  description,
  action,
  className,
}: {
  icon: React.ElementType;
  title: string;
  description?: string;
  action?: React.ReactNode;
  className?: string;
}) {
  return (
    <div className={cn("flex flex-col items-center justify-center gap-3 px-6 py-16 text-center", className)}>
      <div className="flex size-12 items-center justify-center rounded-xl border border-border bg-surface-2 text-muted-foreground">
        <Icon className="size-5" />
      </div>
      <div className="space-y-1">
        <h3 className="text-foreground">{title}</h3>
        {description && <p className="max-w-sm text-sm text-muted-foreground">{description}</p>}
      </div>
      {action}
    </div>
  );
}

export function ErrorState({ message, onRetry }: { message?: string; onRetry?: () => void }) {
  return (
    <div className="flex flex-col items-center justify-center gap-3 px-6 py-16 text-center">
      <div className="flex size-12 items-center justify-center rounded-xl border border-danger/30 bg-danger-subtle text-danger">
        <AlertTriangle className="size-5" />
      </div>
      <div className="space-y-1">
        <h3 className="text-foreground">Something went wrong</h3>
        <p className="max-w-sm text-sm text-muted-foreground">
          {message ?? "We couldn't reach the Wardyn control plane. Please try again."}
        </p>
      </div>
      {onRetry && (
        <Button variant="outline" size="sm" onClick={onRetry}>
          <RotateCw className="size-3.5" /> Retry
        </Button>
      )}
    </div>
  );
}

export function TableSkeleton({ rows = 6, cols = 6 }: { rows?: number; cols?: number }) {
  return (
    <div className="divide-y divide-border">
      {Array.from({ length: rows }).map((_, r) => (
        <div key={r} className="flex items-center gap-4 px-4 py-3.5">
          {Array.from({ length: cols }).map((_, c) => (
            <div
              key={c}
              className="h-3.5 animate-pulse rounded bg-muted"
              style={{ width: c === 0 ? 90 : c === cols - 1 ? 40 : `${30 + ((r + c) % 4) * 14}%` }}
            />
          ))}
        </div>
      ))}
    </div>
  );
}
