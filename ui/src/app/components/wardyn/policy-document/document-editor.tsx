/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// DocumentEditor — the run document's source box: a textarea with a line gutter
// and the caller's diagnostics marked on their lines. It holds no text of its
// own and never repairs what was typed. The format switch is the caller's.
import * as React from "react";
import type { PolicySourceFormat } from "../../../lib/policy-document";
import { Textarea } from "../../ui/textarea";
import { cn } from "../../ui/utils";
import { POLICY_DOCUMENT as D } from "../copy/policy-document";

/** One problem in the source, at a 1-based line and column. */
export interface DocumentDiagnostic {
  line: number;
  column: number;
  text: string;
}

export interface DocumentEditorProps {
  /** The textarea's id; the id of the diagnostics list is derived from it. */
  id: string;
  /** The authored text, exactly as typed. */
  value: string;
  format: PolicySourceFormat;
  onChange: (value: string) => void;
  diagnostics: DocumentDiagnostic[];
  /** The textarea's accessible name. */
  label: string;
  /** Ids of the caller's own descriptions of the editor. */
  describedBy?: string;
  rows?: number;
  className?: string;
}

// Gutter and textarea share one font, line height and vertical padding, and the
// textarea never wraps, so line N of the gutter is line N of the text. The
// gutter is contained in size: its one-row-per-line content must not set the
// height of the row, or the box grows to the whole document and `rows` and the
// scroll sync do nothing.
const GUTTER_CONTAIN = "[contain:size]";
const LINE = "py-2 font-mono text-xs leading-5";

export function DocumentEditor({
  id,
  value,
  format,
  onChange,
  diagnostics,
  label,
  describedBy,
  rows = 12,
  className,
}: DocumentEditorProps) {
  const gutter = React.useRef<HTMLDivElement>(null);
  const diagnosticsId = `${id}-diagnostics`;
  const invalid = diagnostics.length > 0;
  const errorLines = new Set(diagnostics.map((d) => d.line));
  const lineCount = value.split("\n").length;

  return (
    <div className={cn("flex min-w-0 flex-col gap-2", className)} data-testid="document-editor" data-format={format}>
      <div className={cn("flex min-w-0 rounded-md border bg-input-background focus-within:ring-[3px] focus-within:ring-ring", invalid ? "border-destructive" : "border-input")}>
        <div
          ref={gutter}
          aria-hidden="true"
          data-testid="document-gutter"
          className={cn(GUTTER_CONTAIN, "w-10 shrink-0 select-none overflow-hidden rounded-l-md border-r border-border bg-muted text-right text-muted-foreground", LINE)}
        >
          {Array.from({ length: lineCount }, (_, i) => {
            const error = errorLines.has(i + 1);
            return (
              <div
                key={i}
                data-line={i + 1}
                data-error={error || undefined}
                className={cn("border-l-2 border-transparent pr-2", error && "border-danger bg-danger-subtle font-semibold text-danger")}
              >
                {i + 1}
              </div>
            );
          })}
        </div>
        <Textarea
          id={id}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          onScroll={(e) => {
            if (gutter.current) gutter.current.scrollTop = e.currentTarget.scrollTop;
          }}
          rows={rows}
          wrap="off"
          spellCheck={false}
          aria-label={label}
          aria-invalid={invalid || undefined}
          aria-describedby={[describedBy, invalid ? diagnosticsId : undefined].filter(Boolean).join(" ") || undefined}
          className={cn(
            "min-w-0 flex-1 field-sizing-fixed resize-none overflow-auto whitespace-pre rounded-l-none border-0 bg-transparent px-3 text-xs shadow-none focus-visible:ring-0 md:text-xs",
            LINE,
          )}
        />
      </div>

      {invalid && (
        <ul id={diagnosticsId} className="m-0 flex list-none flex-col gap-1 p-0">
          {diagnostics.map((d, i) => (
            <li key={`${d.line}:${d.column}:${i}`} className="select-text whitespace-pre-wrap break-words text-xs text-danger">
              {d.text} <span className="font-mono text-muted-foreground">{D.SOURCE_POSITION(d.line, d.column)}</span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
