/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Only lazy policy consumers may import this module; code-block's display
// emitter is reachable from the eager Runs screen and must stay dependency-free.
import { Composer, isAlias, isMap, isNode, isPair, isScalar, LineCounter, Parser, visit, type Document } from "yaml";

export type PolicySourceValue = null | boolean | number | string | PolicySourceValue[] | PolicySourceMapping;
export type PolicySourceMapping = { [key: string]: PolicySourceValue };
export type PolicySourceFormat = "yaml" | "json";
export type PolicySourceError = { ok: false; line: number; column: number; message: string };
export type PolicySourceResult = { ok: true; value: PolicySourceMapping } | PolicySourceError;
export type PolicySourceEditResult = { ok: true; source: string } | PolicySourceError;

function failure(lines: LineCounter, offset: number, message: string): PolicySourceError {
  const pos = lines.linePos(offset);
  return { ok: false, line: Math.max(1, pos.line), column: Math.max(1, pos.col), message };
}

function caught(error: unknown): PolicySourceError {
  return { ok: false, line: 1, column: 1, message: error instanceof Error ? error.message : "Policy source could not be read." };
}

function safeNumber(value: number): boolean {
  return Number.isFinite(value) && (!Number.isInteger(value) || Number.isSafeInteger(value));
}

function documentValue(document: Document, lines: LineCounter): PolicySourceResult {
  if (!isMap(document.contents)) {
    return failure(lines, document.contents?.range?.[0] ?? 0, "Policy source must be a mapping.");
  }
  let error: PolicySourceError | undefined;
  visit(document, (_, node) => {
    let message: string | undefined;
    let offset = isNode(node) ? node.range?.[0] ?? 0 : 0;
    if (isPair(node)) {
      offset = isNode(node.key) ? node.key.range?.[0] ?? 0 : 0;
      if (!isScalar(node.key) || typeof node.key.value !== "string") message = "Mapping keys must be strings.";
      else if (node.key.value === "<<") message = "Merge keys (<<) are not allowed.";
    } else if (isAlias(node)) message = "Aliases are not allowed.";
    else if (isNode(node) && node.tag) message = "Explicit tags are not allowed.";
    else if (isScalar(node)) {
      const value = node.value;
      if (typeof value === "bigint") {
        // Checking before Number() is essential: rounding can hide an unsafe integer.
        if (value < BigInt(Number.MIN_SAFE_INTEGER) || value > BigInt(Number.MAX_SAFE_INTEGER)) {
          message = "Numbers must be finite and within the safe integer range.";
        } else node.value = Number(value);
      } else if (typeof value === "number") {
        if (!safeNumber(value)) message = "Numbers must be finite and within the safe integer range.";
      } else if (value !== null && typeof value !== "string" && typeof value !== "boolean") {
        message = "Only JSON-compatible values are allowed.";
      }
    }
    if (message) {
      error = failure(lines, offset, message);
      return visit.BREAK;
    }
  });
  if (error) return error;
  return { ok: true, value: document.toJS({ mapAsMap: false, maxAliasCount: 0 }) as PolicySourceMapping };
}

function readSource(source: string):
  | { ok: true; document: Document; value: PolicySourceMapping }
  | PolicySourceError {
  const lines = new LineCounter();
  try {
    const tokens = [...new Parser(lines.addNewLine).parse(source)];
    // Inspect tokens, not lines: %YAML and %TAG inside strings are ordinary text.
    const directive = tokens.find((token) => token.type === "directive");
    if (directive) return failure(lines, directive.offset, "Directives are not allowed.");
    const documents = [...new Composer({
      version: "1.2",
      schema: "core",
      customTags: [],
      uniqueKeys: true,
      merge: false,
      intAsBigInt: true,
      keepSourceTokens: true,
      strict: true,
    }).compose(tokens, true, source.length)];
    if (documents.length !== 1) {
      const second = tokens.filter((token) => token.type === "document")[1];
      return failure(lines, second?.offset ?? 0, "MULTIPLE_DOCS: Policy source must contain one document.");
    }
    const document = documents[0];
    const issue = document.errors[0] ?? document.warnings[0];
    if (issue) return failure(lines, issue.pos[0], `${issue.code}: ${issue.message}`);
    const parsed = documentValue(document, lines);
    return parsed.ok ? { ...parsed, document } : parsed;
  } catch (error) {
    return caught(error);
  }
}

// JSON goes through this same boundary: JSON.parse would silently keep the last
// duplicate key. No failed parse exposes a previously valid value or Document.
export function parsePolicySource(source: string): PolicySourceResult {
  const parsed = readSource(source);
  return parsed.ok ? { ok: true, value: parsed.value } : parsed;
}

function jsonEditValue(value: unknown): boolean {
  if (value === null || typeof value === "string" || typeof value === "boolean") return true;
  if (typeof value === "number") return safeNumber(value);
  if (Array.isArray(value)) return Array.from(value).every(jsonEditValue);
  if (typeof value !== "object" || value === null) return false;
  const prototype = Object.getPrototypeOf(value);
  return (prototype === Object.prototype || prototype === null) &&
    Object.entries(value).every(([key, item]) => key !== "<<" && jsonEditValue(item));
}

function validPath(value: PolicySourceValue | undefined, path: readonly (string | number)[]): boolean {
  for (const key of path) {
    if (Array.isArray(value)) {
      if (typeof key !== "number" || !Number.isSafeInteger(key) || key < 0 || key > value.length) return false;
      value = value[key];
    } else if (value === undefined) {
      // A new sequence may start at zero, but an edit must never create holes.
      if (typeof key === "number" && key !== 0) return false;
    } else if (value !== null && typeof value === "object") {
      if (typeof key !== "string" || key === "<<") return false;
      value = Object.prototype.hasOwnProperty.call(value, key) ? value[key] : undefined;
    } else return false;
  }
  return path.length > 0 && !path.includes("<<");
}

// undefined removes a field; null and [] remain explicit authored values. The
// Document is ephemeral and only the returned string belongs in draft state.
export function editPolicySource(
  source: string,
  path: readonly (string | number)[],
  value: PolicySourceValue | undefined,
  format: PolicySourceFormat,
): PolicySourceEditResult {
  const parsed = readSource(source);
  if (!parsed.ok) return parsed;
  try {
    if (!validPath(parsed.value, path)) return caught(new Error("The edit must name a mapping field or a sequence item without gaps."));
    if (value !== undefined && !jsonEditValue(value)) return caught(new Error("The edit must contain only JSON-compatible values and safe numbers."));
    if (value === undefined) {
      if (!parsed.document.hasIn(path)) return { ok: true, source };
      parsed.document.deleteIn(path);
    } else {
      const next = value !== null && typeof value === "object"
        ? parsed.document.createNode(value, { aliasDuplicateObjects: false })
        : value;
      parsed.document.setIn(path, next);
    }
    const edited = parsed.document.toString();
    const checked = parsePolicySource(edited);
    if (!checked.ok) return checked;
    // JSON is the wire/storage representation, so its conversion drops comments.
    return { ok: true, source: format === "json" ? JSON.stringify(checked.value, null, 2) : edited };
  } catch (error) {
    return caught(error);
  }
}
