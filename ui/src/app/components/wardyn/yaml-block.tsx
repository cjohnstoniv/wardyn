/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The policy display blocks and the one YAML emitter, moved unchanged out of
// code-block.tsx: every user is a lazy screen, and code-block.tsx is eager, so
// keeping them there put them in the size-budgeted entry chunk. Nothing eager
// may import this module, and code-block.tsx must not re-export it.
import * as React from "react";
import { CodeBlock } from "./code-block";

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
    if (value.length === 0) return "[]";
    return value
      .map((item) =>
        // A nested block opens on the dash's own line: drop its first indent.
        isYamlContainer(item) ? `${pad}- ${toYaml(item, indent + 1).slice(pad.length + 2)}` : `${pad}- ${toYaml(item, 0)}`,
      )
      .join("\n");
  }
  const entries = Object.entries(value as Record<string, unknown>).filter(([, v]) => v !== undefined);
  if (entries.length === 0) return "{}";
  return entries
    .map(([k, v]) => {
      const key = yamlScalar(k);
      // YAML allows an implicit key at most 1024 characters before its colon.
      const head = `${pad}${key.length > 1024 ? `? ${key}\n${pad}` : key}:`;
      return isYamlContainer(v) ? `${head}\n${toYaml(v, indent + 1)}` : `${head} ${toYaml(v, 0)}`;
    })
    .join("\n");
}

function isYamlContainer(v: unknown): boolean {
  if (Array.isArray(v)) return v.length > 0;
  return v !== null && typeof v === "object" && Object.keys(v as object).length > 0;
}

// yamlScalar quotes a string — a value or a mapping key — only when a plain YAML
// scalar would be ambiguous: empty, a control character other than tab (a raw line
// break ends the scalar), special indicators, leading/trailing space, or text that
// would parse as a number/bool/null. Uses JSON string quoting for the quoted form,
// plus YAML's escapes for U+0085, U+2028 and U+2029, which JSON leaves raw and the
// CLI's YAML reader (gopkg.in/yaml.v3) takes as line breaks even inside quotes.
// One expression on purpose: small, and free of any YAML library.
function yamlScalar(s: string): string {
  return /^$|[\0-\b\n-\x1f\x85\u2028\u2029:#[\]{}",&*!|>'%@`]|^[\s?-]|\s$|^(true|false|null|yes|no|on|off|~)$|^[+-]?[\d.]/i.test(s)
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
