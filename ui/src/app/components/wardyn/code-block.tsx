/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { cn } from "../ui/utils";
import { CopyButton } from "./copy-button";

/** A mono code block with a copy button. `children` is the tinted rendering of
 *  `text`; without it the raw text is shown (arbitrary emitted-file contents). */
export function CodeBlock({
  text,
  className,
  children,
  copyLabel,
}: {
  text: string;
  className?: string;
  children?: React.ReactNode;
  /** Accessible name of the Copy button; says WHAT is copied when a card holds several blocks. */
  copyLabel?: string;
}) {
  return (
    <div className={cn("group relative rounded-lg border border-border bg-surface-2/60", className)}>
      <CopyButton
        text={text}
        label={copyLabel}
        className="absolute right-2 top-2 z-10 size-7 justify-center rounded-md border border-border bg-card text-muted-foreground opacity-0 transition focus-visible:opacity-100 group-hover:opacity-100 hover:text-foreground"
      />
      <pre className="scroll-thin overflow-x-auto p-3 text-xs leading-relaxed">
        <code className="font-mono">{children ?? text}</code>
      </pre>
    </div>
  );
}

/** Syntax-tinted pretty-printed JSON in a mono code block. */
export function JsonBlock({ value, className }: { value: unknown; className?: string }) {
  const text = React.useMemo(() => JSON.stringify(value, null, 2), [value]);
  return (
    <CodeBlock text={text} className={className}>
      {tintLines(text, /^("[^"]+"):(\s?)(.*)$/)}
    </CodeBlock>
  );
}

/** Pretty-printed YAML in a mono code block — the preferred, readable rendering for
 *  policy specs (indented, no noisy braces/quotes). Copy yields the YAML text. */
export function YamlBlock({ value: v, className }: { value: unknown; className?: string }) {
  const text = React.useMemo(() => toYaml(v), [v]);
  return (
    <CodeBlock text={text} className={className}>
      {tintLines(text, /^([\w.*-]+):(\s?)(.*)$/)}
    </CodeBlock>
  );
}

// toYaml renders a JSON-like value (the shapes a RunPolicySpec uses: objects, arrays,
// strings, numbers, booleans, null) as pretty, indented YAML. Kept minimal on purpose
// — no external yaml dep — and scalar-quotes only where YAML requires it. The strict
// policy parser (lib/policy-document) must read its output back as the same value.
export function toYaml(value: unknown, indent = 0): string {
  const pad = "  ".repeat(indent);
  if (value === null || value === undefined) return "null";
  if (typeof value === "boolean" || typeof value === "number") return String(value);
  if (typeof value === "string") return yamlScalar(value);
  if (Array.isArray(value)) {
    if (!value.length) return "[]";
    return value
      // A nested block opens on the dash's own line: drop its first indent.
      .map((item) => `${pad}- ${isYamlContainer(item) ? toYaml(item, indent + 1).slice(pad.length + 2) : toYaml(item, 0)}`)
      .join("\n");
  }
  const entries = Object.entries(value as Record<string, unknown>).filter(([, v]) => v !== undefined);
  if (!entries.length) return "{}";
  return entries
    .map(([k, v]) => {
      const key = yamlScalar(k);
      // YAML allows an implicit key at most 1024 characters before its colon.
      return `${pad}${key.length > 1024 ? `? ${key}\n${pad}` : key}:${isYamlContainer(v) ? `\n${toYaml(v, indent + 1)}` : ` ${toYaml(v, 0)}`}`;
    })
    .join("\n");
}

// Arrays too: a JSON array has no keys beyond its indices.
function isYamlContainer(v: unknown): boolean {
  return v !== null && typeof v === "object" && Object.keys(v as object).length > 0;
}

// yamlScalar quotes a string — a value or a mapping key — only when a plain YAML
// scalar would be ambiguous: empty, a control character other than tab (a raw line
// break ends the scalar), special indicators, leading/trailing space, or text that
// would parse as a number/bool/null (a leading - is caught above; the CLI's reader
// drops underscores first, so +_1 is a number there). Uses JSON string quoting for
// the quoted form, plus YAML's escapes for U+0085, U+2028 and U+2029, which JSON
// leaves raw and the CLI's YAML reader (gopkg.in/yaml.v3) takes as line breaks even
// inside quotes. One expression on purpose: this module ships in the size-budgeted
// entry chunk.
function yamlScalar(s: string): string {
  return /^$|[\0-\b\n-\x1f\x85\u2028\u2029:#[\]{}",&*!|>'%@`]|^[\s?-]|\s$|^(true|false|null|yes|no|on|off|~)$|^\+?[\d._]/i.test(s)
    ? JSON.stringify(s).replace(/\x85/g, "\\N").replace(/\u2028/g, "\\L").replace(/\u2029/g, "\\P")
    : s;
}

// tintLines renders each source line as its own div, tinting a `key: value` pair
// matched by `kv` (group 1 = key, 2 = separator space, 3 = value) WITHOUT touching
// indentation. Shared by both blocks — JSON and YAML differ only in the key regex.
function tintLines(text: string, kvRe: RegExp): React.ReactNode {
  return text.split("\n").map((line, i) => {
    const lead = line.match(/^(\s*(?:- )?)/)?.[1] ?? "";
    const rest = line.slice(lead.length);
    const kv = rest.match(kvRe);
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
    <span title={title} className={cn("font-mono text-xs leading-[inherit] text-muted-foreground", className)}>
      {children}
    </span>
  );
}

/**
 * makeMono builds the backtick-mono splitter four surfaces need: a frozen copy
 * string carries its literals as PLAIN TEXT (each copy module's own header
 * rule), and the CONSUMING component decides which substrings render mono.
 * Give it that surface's term list; it returns the renderer.
 *
 * Terms are regex-escaped, so a term carrying `.`, `-` or `/` matches itself
 * and nothing else — the drives list (`/home/agent/drive`, `disk_mib`, `. _ -`)
 * is the one that needs it, and escaping is a no-op for the env-var lists.
 * Longest-first so a term that is a prefix of another cannot win the split.
 */
export function makeMono(terms: string[]): (text: string) => React.ReactNode {
  const ordered = [...terms].sort((a, b) => b.length - a.length);
  const escape = (s: string) => s.replace(/[.*+?^${}()|[\]\\-]/g, "\\$&");
  const re = new RegExp(`(${ordered.map(escape).join("|")})`, "g");
  return function withMono(text: string): React.ReactNode {
    return text.split(re).map((part, i) =>
      terms.includes(part) ? (
        <Mono key={i} className="text-inherit">
          {part}
        </Mono>
      ) : (
        <React.Fragment key={i}>{part}</React.Fragment>
      ),
    );
  };
}
