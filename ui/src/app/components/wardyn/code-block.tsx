/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { Check, Copy } from "lucide-react";
import { cn } from "../ui/utils";
import { useCopyToClipboard } from "../../lib/use-copy-to-clipboard";

/** Syntax-tinted pretty-printed JSON in a mono code block. */
export function JsonBlock({ value, className }: { value: unknown; className?: string }) {
  const text = React.useMemo(() => JSON.stringify(value, null, 2), [value]);
  const { copied, copy } = useCopyToClipboard();

  return (
    <div className={cn("group relative rounded-lg border border-border bg-surface-2/60", className)}>
      <button
        onClick={() => copy(text)}
        className="absolute right-2 top-2 z-10 inline-flex size-7 items-center justify-center rounded-md border border-border bg-card text-muted-foreground opacity-0 transition focus-visible:opacity-100 group-hover:opacity-100 hover:text-foreground"
        aria-label="Copy"
      >
        {copied ? <Check className="size-3.5 text-success" /> : <Copy className="size-3.5" />}
      </button>
      <span aria-live="polite" className="sr-only">{copied ? "Copied" : ""}</span>
      <pre className="scroll-thin overflow-x-auto p-3 text-xs leading-relaxed">
        <code className="font-mono">{highlight(text)}</code>
      </pre>
    </div>
  );
}

function highlight(json: string): React.ReactNode {
  const lines = json.split("\n");
  return lines.map((line, i) => {
    const m = line.match(/^(\s*)"([^"]+)":\s*(.*)$/);
    if (m) {
      return (
        <div key={i}>
          {m[1]}
          <span className="text-info">"{m[2]}"</span>
          <span className="text-muted-foreground">: </span>
          {value(m[3])}
        </div>
      );
    }
    // Non key/value line (array element, bracket) — PRESERVE leading indentation.
    const lead = line.match(/^(\s*)/)?.[1] ?? "";
    return (
      <div key={i}>
        {lead}
        <span className="text-muted-foreground">{value(line.slice(lead.length))}</span>
      </div>
    );
  });
}

/** Pretty-printed YAML in a mono code block — the preferred, readable rendering for
 *  policy specs (indented, no noisy braces/quotes). Copy yields the YAML text. */
export function YamlBlock({ value: v, className }: { value: unknown; className?: string }) {
  const text = React.useMemo(() => toYaml(v), [v]);
  const { copied, copy } = useCopyToClipboard();

  return (
    <div className={cn("group relative rounded-lg border border-border bg-surface-2/60", className)}>
      <button
        onClick={() => copy(text)}
        className="absolute right-2 top-2 z-10 inline-flex size-7 items-center justify-center rounded-md border border-border bg-card text-muted-foreground opacity-0 transition focus-visible:opacity-100 group-hover:opacity-100 hover:text-foreground"
        aria-label="Copy"
      >
        {copied ? <Check className="size-3.5 text-success" /> : <Copy className="size-3.5" />}
      </button>
      <span aria-live="polite" className="sr-only">{copied ? "Copied" : ""}</span>
      <pre className="scroll-thin overflow-x-auto p-3 text-xs leading-relaxed">
        <code className="font-mono">{highlightYaml(text)}</code>
      </pre>
    </div>
  );
}

// toYaml renders a JSON-like value (the shapes a RunPolicySpec uses: objects, arrays,
// strings, numbers, booleans, null) as pretty, indented YAML. Kept minimal on purpose
// — no external yaml dep — and scalar-quotes only where YAML requires it.
export function toYaml(value: unknown, indent = 0): string {
  const pad = "  ".repeat(indent);
  if (value === null || value === undefined) return "null";
  if (typeof value === "boolean" || typeof value === "number") return String(value);
  if (typeof value === "string") return yamlScalar(value);
  if (Array.isArray(value)) {
    if (value.length === 0) return "[]";
    return value
      .map((item) => {
        if (isYamlContainer(item)) {
          const lines = toYaml(item, indent + 1).split("\n");
          const first = lines[0].slice((indent + 1) * 2); // hoist first line after "- "
          const rest = lines.slice(1);
          return `${pad}- ${first}${rest.length ? "\n" + rest.join("\n") : ""}`;
        }
        return `${pad}- ${toYaml(item, 0)}`;
      })
      .join("\n");
  }
  const entries = Object.entries(value as Record<string, unknown>).filter(([, v]) => v !== undefined);
  if (entries.length === 0) return "{}";
  return entries
    .map(([k, v]) => (isYamlContainer(v) ? `${pad}${k}:\n${toYaml(v, indent + 1)}` : `${pad}${k}: ${toYaml(v, 0)}`))
    .join("\n");
}

function isYamlContainer(v: unknown): boolean {
  if (Array.isArray(v)) return v.length > 0;
  return v !== null && typeof v === "object" && Object.keys(v as object).length > 0;
}

// yamlScalar quotes a string only when a plain YAML scalar would be ambiguous
// (special indicators, leading/trailing space, or a value that would parse as a
// number/bool/null). Uses JSON string quoting for the quoted form.
function yamlScalar(s: string): string {
  if (s === "") return '""';
  const needsQuote =
    /[:#[\]{}",&*!|>'%@`]/.test(s) ||
    /^[\s?-]/.test(s) ||
    /\s$/.test(s) ||
    /^(true|false|null|yes|no|on|off|~)$/i.test(s) ||
    /^[+-]?[\d.]/.test(s);
  return needsQuote ? JSON.stringify(s) : s;
}

// highlightYaml tints keys/values per line WITHOUT touching indentation (the JSON
// highlighter's old bug). Renders each source line as its own div.
function highlightYaml(yaml: string): React.ReactNode {
  return yaml.split("\n").map((line, i) => {
    const lead = line.match(/^(\s*(?:- )?)/)?.[1] ?? "";
    const rest = line.slice(lead.length);
    const kv = rest.match(/^([\w.*-]+):(\s?)(.*)$/);
    if (kv) {
      return (
        <div key={i}>
          {lead}
          <span className="text-info">{kv[1]}</span>
          <span className="text-muted-foreground">:</span>
          {kv[2]}
          {kv[3] ? value(kv[3]) : null}
        </div>
      );
    }
    return (
      <div key={i}>
        {lead}
        {rest ? value(rest) : null}
      </div>
    );
  });
}

function value(v: string): React.ReactNode {
  const trailingComma = v.endsWith(",");
  const core = trailingComma ? v.slice(0, -1) : v;
  let cls = "text-foreground";
  if (/^".*"$/.test(core)) cls = "text-success";
  else if (/^(true|false)$/.test(core)) cls = "text-warning";
  else if (/^-?\d+(\.\d+)?$/.test(core)) cls = "text-chart-4";
  else if (core === "null") cls = "text-muted-foreground";
  return (
    <>
      <span className={cls}>{core}</span>
      {trailingComma && <span className="text-muted-foreground">,</span>}
    </>
  );
}

/** Inline monospace identity / id token. */
export function Mono({ children, className, title }: { children: React.ReactNode; className?: string; title?: string }) {
  return (
    <span title={title} className={cn("font-mono text-[12.5px] text-muted-foreground", className)}>
      {children}
    </span>
  );
}
