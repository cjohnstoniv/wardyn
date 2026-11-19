/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run document as text: the draft's intent written out, an example, and the
// server's diagnostics placed on lines.
import { isMap, isScalar, isSeq, LineCounter, parseDocument, type Pair, type YAMLMap } from "yaml";
import { runDocumentErrorText, RUN_END_CONFLICT_REASON } from "../../../../lib/run-document-canon";
import { TEMPLATE_API_VERSION, RUN_DOCUMENT_KIND, type TemplateDiagnostic } from "../../../../lib/types/templates";
import { toYaml } from "../../../wardyn/yaml-block";
import type { DocDiagnostic, RunIntent } from "../draft/run-intent";

export type DocumentFormat = "yaml" | "json";

/** The server refuses a document over this many bytes; the console refuses first. */
export const MAX_DOCUMENT_BYTES = 1 << 20;
export const tooLarge = (text: string): boolean => new TextEncoder().encode(text).length > MAX_DOCUMENT_BYTES;

/** The draft's intent as a kind Run document, in the chosen format. */
export function runDocumentText(intent: RunIntent, format: DocumentFormat): string {
  const doc = { api_version: TEMPLATE_API_VERSION, kind: RUN_DOCUMENT_KIND, intent };
  return format === "json" ? JSON.stringify(doc, null, 2) : `${toYaml(doc)}\n`;
}

/** An example run of the given harness: an interactive agent task. */
export const exampleDocumentText = (harness: string, format: DocumentFormat): string =>
  runDocumentText({ shape: "agent_task", experience: "interactive", tools: [{ id: harness, kind: "harness" }] }, format);

type Segment = string | number;

/** A server diagnostic path (`intent.tools[0].id`) as keys and indexes. */
function segments(path: string): Segment[] {
  return [...path.matchAll(/([^.[\]]+)|\[(\d+)\]/g)].map((m) => (m[2] !== undefined ? Number(m[2]) : m[1]));
}

/**
 * The 1-based line and column of the key a path names, or of its deepest
 * ancestor that is in the text; the start of the text when none is. JSON is
 * read as YAML, which it is a subset of.
 */
export const locatePath = (text: string, path: string): { line: number; column: number } => locator(text)(path);

/**
 * locatePath over one text, parsed once: a document can draw thousands of
 * diagnostics, and parsing it again for each, or scanning a map's keys again for
 * each, stalls the console. A map's keys are indexed on first use, the first of
 * a repeated key kept. The parser's repeated-key check is off: it compares each
 * key with every earlier one, and a document with a repeated key is refused
 * before the server is asked.
 */
function locator(text: string): (path: string) => { line: number; column: number } {
  const lines = new LineCounter();
  const doc = parseDocument(text, { lineCounter: lines, prettyErrors: false, uniqueKeys: false });
  const keys = new Map<YAMLMap, Map<string, Pair>>();
  const keyed = (map: YAMLMap): Map<string, Pair> => {
    let index = keys.get(map);
    if (!index) {
      index = new Map();
      for (const pair of map.items) {
        if (isScalar(pair.key) && !index.has(String(pair.key.value))) index.set(String(pair.key.value), pair);
      }
      keys.set(map, index);
    }
    return index;
  };
  return (path) => {
    let at = 0;
    let node: unknown = doc.contents;
    for (const seg of segments(path)) {
      if (isMap(node) && typeof seg === "string") {
        const pair = keyed(node).get(seg);
        if (!pair) break;
        at = (isScalar(pair.key) && pair.key.range?.[0]) || at;
        node = pair.value;
      } else if (isSeq(node) && typeof seg === "number") {
        const item = node.items[seg] as { range?: [number, number, number] | null } | undefined;
        if (!item) break;
        at = item.range?.[0] ?? at;
        node = item;
      } else break;
    }
    const pos = lines.linePos(at);
    return { line: Math.max(1, pos.line), column: Math.max(1, pos.col) };
  };
}

/**
 * The editor's diagnostics for the server's: the canon sentence when there is
 * one, else the server's own, on the line of the key it names. The end conflict
 * is reported at `intent`; it marks both keys.
 */
export function placeDiagnostics(text: string, format: DocumentFormat, diagnostics: readonly TemplateDiagnostic[]): DocDiagnostic[] {
  const locate = locator(text);
  return diagnostics.flatMap((d) => {
    const sentence = runDocumentErrorText(d, format);
    const paths = d.reason === RUN_END_CONFLICT_REASON ? [`${d.path}.ends_after_sec`, `${d.path}.no_end`] : [d.path];
    const seen = new Set<number>();
    return paths.flatMap((path) => {
      const at = locate(path);
      if (seen.has(at.line)) return [];
      seen.add(at.line);
      return [{ path: d.path, reason: d.reason, text: sentence, ...at }];
    });
  });
}
