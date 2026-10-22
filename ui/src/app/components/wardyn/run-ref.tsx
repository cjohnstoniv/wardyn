/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Link } from "react-router-dom";
import { Check } from "lucide-react";
import { cn } from "../ui/utils";
import { useCopyToClipboard } from "../../lib/use-copy-to-clipboard";
import { RUN_REF } from "./copy/run-ref";

/** The 8-character id a run is shown by. Audit rows carry a `run_` prefix some callers keep. */
export const shortRunId = (runId: string): string => runId.replace(/^run_/, "").slice(0, 8);

// A title can be a task paragraph: the first line, and the CSS truncates it to the room it has.
const firstLine = (title: string) => title.trim().split("\n")[0];

// #3036: the one way a run is named. The title leads (a link when `to` or `onOpen` is given, plain text
// on the run's own page), then a muted mono 8-character id. Hover or focus shows the whole id, a click copies it.
// With no title the id stands alone after "Run".
export function RunRef({
  runId,
  title,
  to,
  onOpen,
  className,
}: {
  runId: string;
  title?: string;
  /** Makes the title a router link. */
  to?: string;
  /** Makes the title a button, for a caller that drills in place (the Audit rows). */
  onOpen?: () => void;
  className?: string;
}) {
  const { copied, copy } = useCopyToClipboard();
  const name = firstLine(title ?? "");
  // With no title the lead word is still the way in, so a row never loses its link.
  const label = name || RUN_REF.ID_ONLY;
  const labelClass = cn("min-w-0 truncate", name ? "text-foreground" : "shrink-0 text-muted-foreground");
  return (
    <span data-testid="run-ref" className={cn("inline-flex min-w-0 max-w-full items-baseline gap-1.5", className)}>
      {to ? (
        <Link to={to} className={cn(labelClass, "hover:underline")} onClick={(e) => e.stopPropagation()}>
          {label}
        </Link>
      ) : onOpen ? (
        <button
          type="button"
          className={cn(labelClass, "text-left hover:underline")}
          onClick={(e) => {
            e.stopPropagation();
            onOpen();
          }}
        >
          {label}
        </button>
      ) : (
        <span className={labelClass}>{label}</span>
      )}
      <button
        type="button"
        title={`${runId}\n${RUN_REF.CLICK_TO_COPY}`}
        className="inline-flex shrink-0 items-center gap-1 rounded-sm font-mono text-meta text-muted-foreground hover:text-foreground focus-visible:text-foreground"
        onClick={(e) => {
          e.stopPropagation();
          void copy(runId);
        }}
      >
        {shortRunId(runId)}
        {copied && <Check className="size-3 text-success" aria-hidden />}
        <span aria-live="polite" className="sr-only">
          {copied ? RUN_REF.COPIED : ""}
        </span>
      </button>
    </span>
  );
}
