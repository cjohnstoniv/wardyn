/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Only lazy policy consumers may import this module; code-block's display
// emitter is reachable from the eager Runs screen and must stay dependency-free.
import { Composer, isAlias, isMap, isNode, isPair, isScalar, LineCounter, Parser, visit, type CST, type Document } from "yaml";

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

function located(source: string, offset: number, message: string): PolicySourceError {
  const before = source.slice(0, offset).split("\n");
  return { ok: false, line: before.length, column: before[before.length - 1].length + 1, message };
}

// yaml here breaks lines only at LF and CRLF. The CLI's --policy-file reader
// (gopkg.in/yaml.v3) also breaks at a bare CR, U+0085, U+2028 and U+2029, so
// the same text would read as a different policy there. Escapes are fine.
const STRAY_BREAK = /\r(?!\n)|[\x85\u2028\u2029]/;

function strayBreak(source: string): PolicySourceError | undefined {
  const at = source.search(STRAY_BREAK);
  if (at < 0) return undefined;
  const code = source.charCodeAt(at).toString(16).padStart(4, "0");
  return located(source, at, source[at] === "\r"
    ? "Bare carriage return: use LF or CRLF line endings."
    : `Unescaped U+${code.toUpperCase()}: write it as \\u${code} in a quoted string.`);
}

// The server refuses a request body over 1 MiB, so no larger policy can launch.
const MAX_SOURCE_LENGTH = 1 << 20;
// Far above any real policy; composing, converting and the JSON locator recurse
// once per level, and a stack overflow there left V8 unable to compile the next
// regular expression, aborting the page on a later parse.
const MAX_DEPTH = 64;
const TOO_DEEP = `Policy source is nested more than ${MAX_DEPTH} levels deep: flatten it.`;

function unreadable(source: string): PolicySourceError | undefined {
  if (source.length > MAX_SOURCE_LENGTH) return { ok: false, line: 1, column: 1, message: "Policy source is larger than 1 MiB: shorten it." };
  // gopkg.in/yaml.v3 strips a byte order mark only at byte 0, yaml here only
  // before the first content line; anywhere else they read the text differently.
  const bom = source.indexOf("\ufeff", 1);
  if (bom > 0) return located(source, bom, "Byte order mark inside the text: remove it, or write it as \\ufeff in a quoted string.");
  return strayBreak(source);
}

// The tokenizer does not recurse, so its output is checked before anything
// that does sees it: nesting depth, and two flow shapes gopkg.in/yaml.v3 reads
// differently from YAML 1.2. A ':' straight before ',', ']' or '}' ends an
// unquoted key here ([x:] is [{x: null}]) but stays in the scalar there
// (["x:"]); a '?' starting an item is a plain scalar here ([?x] is ["?x"]) but
// an explicit-key indicator there ([{x: null}]).
function structureProblem(tokens: readonly CST.Token[], source: string): [number, string] | undefined {
  const pending: [CST.Token | null | undefined, number][] = tokens.map((token) => [token, 0]);
  for (let next = pending.pop(); next; next = pending.pop()) {
    const [token, depth] = next;
    if (!token) continue;
    const level = "items" in token ? depth + 1 : depth;
    if (level > MAX_DEPTH) return [token.offset, TOO_DEEP];
    // With an explicit indentation indicator yaml.v3 keeps a trailing
    // whitespace-only line as content (as YAML 1.2 says); yaml here drops it.
    const header = token.type === "block-scalar" ? token.props.find((prop) => prop.type === "block-scalar-header") : undefined;
    if (header && "source" in header && /\d/.test(header.source)) {
      return [header.offset, "Indentation indicators are not allowed: remove the digit after | or >."];
    }
    // An escaped line break before an empty line keeps it as "\n" in yaml.v3
    // (as YAML 1.2 says); yaml here folds it to a space.
    const escaped = token.type === "double-quoted-scalar" ? /(?:^|[^\\])(?:\\\\)*\\\r?\n/.exec(token.source) : null;
    if (escaped) {
      return [token.offset + escaped.index + escaped[0].lastIndexOf("\\"), "Escaped line break in a quoted string: write the string on one line."];
    }
    if ("value" in token) pending.push([token.value, level]);
    if (!("items" in token)) continue;
    for (const item of token.items) {
      if (token.type === "flow-collection") {
        const colon = item.sep?.find((sep) => sep.type === "map-value-ind");
        if (colon && item.key?.type === "scalar" && /[,\]}]/.test(source[colon.offset + 1] ?? "")) {
          return [colon.offset, "Ambiguous ':' after an unquoted key: add a space after it, or quote the key."];
        }
        const question = [item.key, item.value].find((part) => part?.type === "scalar" && part.source.startsWith("?"));
        if (question) return [question.offset, "Ambiguous '?' starting a flow item: quote the item."];
      }
      pending.push([item.key, level], [item.value, level]);
    }
  }
  return undefined;
}

// Written text must never hold those characters, or a byte order mark past
// byte 0, raw; only quoted strings can escape them.
function escapeBreaks(text: string): string {
  return text.replace(/[\x85\u2028\u2029\ufeff]/g, (char, at: number) => (at ? `\\u${char.charCodeAt(0).toString(16).padStart(4, "0")}` : char));
}

function safeNumber(value: number): boolean {
  return Number.isFinite(value) && (!Number.isInteger(value) || Number.isSafeInteger(value));
}

// gopkg.in/yaml.v3, the CLI's --policy-file reader, resolves some plain scalars
// differently from YAML 1.2 core here: 017 is octal 15 there, 1_000, 0b1 and
// 0X1F are numbers, and a date is a timestamp. Refused rather than guessed.
// yaml.v3 tries numbers only for text starting with a sign, digit or dot, and
// drops every underscore first.
const NUMBER_LIKE = /^[-+]?(?:0[box][\da-f]+|(?:\.\d+|\d+(?:\.\d*)?)(?:e[-+]?\d+)?)$/i;
const DATE_LIKE = /^\d{4}-\d\d?-\d\d?(?:[Tt\s]|$)/;

function ambiguousString(text: string): boolean {
  return (/^[-+.\d]/.test(text) && NUMBER_LIKE.test(text.replace(/_/g, ""))) || DATE_LIKE.test(text);
}

function documentValue(document: Document, lines: LineCounter, source: string): PolicySourceResult {
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
    else if (isNode(node) && node.anchor) {
      // Aliases are refused, so an anchor is never needed; and gopkg.in/yaml.v3
      // ends a name at ':' or '?', reading the rest as the next scalar.
      message = "Anchors are not allowed.";
      offset = source.lastIndexOf(`&${node.anchor}`, offset);
    } else if (isNode(node) && node.tag) message = "Explicit tags are not allowed.";
    else if (isScalar(node)) {
      const value = node.value;
      const plain = node.type === "PLAIN" ? node.source ?? "" : undefined;
      // JSON.stringify sends an unpaired surrogate as a \u escape that the
      // server's encoding/json stores as U+FFFD.
      if (typeof value === "string" && /[\ud800-\udbff](?![\udc00-\udfff])|(?:^|[^\ud800-\udbff])[\udc00-\udfff]/.test(value)) {
        message = "Unpaired UTF-16 surrogate: remove it.";
      } else if (plain !== undefined && typeof value === "string" && ambiguousString(plain)) {
        message = "Ambiguous unquoted value: quote it.";
      } else if (plain !== undefined && typeof value !== "string" && /^[-+]?0\d/.test(plain)) {
        message = "Leading zeros are ambiguous: remove them, or quote the value.";
      } else if (typeof value === "bigint") {
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
    const problem = structureProblem(tokens, source);
    if (problem) return failure(lines, ...problem);
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
    const parsed = documentValue(document, lines, source);
    return parsed.ok ? { ...parsed, document } : parsed;
  } catch (error) {
    return caught(error);
  }
}

const JSON_SPACE = /[ \t\n\r]*/y;
// A string up to, not including, its closing quote.
const JSON_STRING = /"(?:[ !#-[\]-\uffff]|\\(?:["\\/bfnrt]|u[0-9a-fA-F]{4}))*/y;
const JSON_ATOM = /-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?|true|false|null/y;

// JSON.parse judges whether source is JSON; engines disagree on whether their
// error says where, so this walks the same grammar only to find that place.
function jsonSyntaxError(source: string): PolicySourceError {
  let at = 0;
  const skip = (pattern: RegExp): boolean => {
    pattern.lastIndex = at;
    if (!pattern.test(source)) return false;
    at = pattern.lastIndex;
    return true;
  };
  const take = (char: string): boolean => {
    if (source[at] !== char) return false;
    at++;
    return true;
  };
  const eat = (char: string): boolean => skip(JSON_SPACE) && take(char);
  const string = (): boolean => skip(JSON_SPACE) && skip(JSON_STRING) && take('"');
  const open = (char: string, depth: number): boolean => {
    if (!eat(char)) return false;
    if (depth < MAX_DEPTH) return true;
    at--;
    throw new RangeError(TOO_DEEP);
  };
  const value = (depth: number): boolean => {
    if (open("{", depth)) {
      if (eat("}")) return true;
      do {
        if (!(string() && eat(":") && value(depth + 1))) return false;
      } while (eat(","));
      return eat("}");
    }
    if (open("[", depth)) {
      if (eat("]")) return true;
      do {
        if (!value(depth + 1)) return false;
      } while (eat(","));
      return eat("]");
    }
    return source[at] === '"' ? string() : skip(JSON_ATOM);
  };
  let message = "Policy source is not valid JSON.";
  try {
    if (value(0) && skip(JSON_SPACE) && at === source.length) at = 0;
    else {
      message = at < source.length
        ? `Unexpected ${JSON.stringify(String.fromCodePoint(source.codePointAt(at) ?? 0))} in JSON.`
        : "Unexpected end of JSON.";
    }
  } catch {
    message = TOO_DEEP; // the only throw: `at` is the bracket past the limit
  }
  return located(source, at, message);
}

// The AST walk supplies the value in both formats: JSON.parse would silently
// keep the last duplicate key and round an unsafe integer. Explicit JSON must
// also be JSON by JSON.parse's grammar and read the same both ways. No failed
// parse exposes a previously valid value or Document.
export function parsePolicySource(source: string, format: PolicySourceFormat = "yaml"): PolicySourceResult {
  const refused = unreadable(source);
  if (refused) return refused;
  let json: unknown;
  if (format === "json") {
    try {
      json = JSON.parse(source);
    } catch {
      return jsonSyntaxError(source);
    }
  }
  const parsed = readSource(source);
  if (!parsed.ok) return parsed;
  try {
    if (format === "json" && JSON.stringify(parsed.value) !== JSON.stringify(json)) {
      return caught(new Error("Policy source reads differently as JSON and as YAML."));
    }
  } catch (error) {
    return caught(error);
  }
  return { ok: true, value: parsed.value };
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
  const parsed = unreadable(source) ?? readSource(source);
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
    // Authored text already passed readSource, so only new strings can hold a
    // CR, a byte order mark or a character that must be escaped, or be
    // ambiguous when plain; only
    // double quotes keep them exact (a block scalar would turn CRLF into LF).
    visit(parsed.document, {
      Scalar(_, node) {
        if (typeof node.value !== "string") return;
        if (/[\r\x85\u2028\u2029\ufeff]/.test(node.value) || ((node.type ?? "PLAIN") === "PLAIN" && ambiguousString(node.value))) {
          node.type = "QUOTE_DOUBLE";
        }
      },
    });
    const edited = escapeBreaks(parsed.document.toString());
    const checked = parsePolicySource(edited);
    if (!checked.ok) return checked;
    // JSON is the wire/storage representation, so its conversion drops comments.
    return { ok: true, source: format === "json" ? escapeBreaks(JSON.stringify(checked.value, null, 2)) : edited };
  } catch (error) {
    return caught(error);
  }
}
