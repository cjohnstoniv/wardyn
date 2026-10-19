/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { CircleAlert, CircleCheck, Info, TriangleAlert } from "lucide-react";
import { cn } from "../ui/utils";

type Tone = "info" | "warning" | "danger" | "success";

const TONES: Record<Tone, { box: string; glyph: string; Icon: typeof Info }> = {
  info: { box: "bg-info-subtle border-info/25", glyph: "text-info", Icon: Info },
  warning: { box: "bg-warning-subtle border-warning/25", glyph: "text-warning", Icon: TriangleAlert },
  danger: { box: "bg-danger-subtle border-danger/25", glyph: "text-danger", Icon: CircleAlert },
  success: { box: "bg-success-subtle border-success/25", glyph: "text-success", Icon: CircleCheck },
};

/**
 * An inline notice. The tone is a glyph plus the words, never colour alone; the text stays
 * `--foreground`. `live` makes it a status region for notices that appear after load.
 */
export function Notice({
  tone,
  title,
  live,
  actions,
  className,
  children,
}: {
  tone: Tone;
  title?: string;
  live?: boolean;
  /** Controls for the reader (a command, a button, a link), beside the words or under them. */
  actions?: React.ReactNode;
  className?: string;
  children: React.ReactNode;
}) {
  const { box, glyph, Icon } = TONES[tone];
  return (
    <div role={live ? "status" : undefined} className={cn("flex gap-2.5 rounded-lg border p-3 text-sm", box, className)}>
      <Icon aria-hidden="true" className={cn("mt-0.5 size-4 shrink-0", glyph)} />
      {/* Actions sit beside the words when there is room, and wrap under them when there is not. */}
      <div className="flex min-w-0 flex-1 flex-wrap items-center gap-x-4 gap-y-1.5">
        <div className="min-w-0 flex-1 basis-80 space-y-0.5">
          {title && <p className="font-medium text-foreground">{title}</p>}
          <div className="text-foreground">{children}</div>
        </div>
        {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
      </div>
    </div>
  );
}
