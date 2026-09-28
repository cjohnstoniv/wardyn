/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The push_rules editor — a section beside the spec, mirroring ToolRulesSection's
// own pattern: it reads and writes the SAME RunPolicySpec document the textarea
// shows, so there is one source of truth and no sync to get wrong.
// deny_paths/require_review_paths are flat string lists, structurally simpler
// than tool_rules' object rows, so each gets its own add/remove list rather
// than tool_rules' named-vs-default split.
//
// The two reserved PushRulesSpec fields (deny_new_executables,
// max_file_size_mib) are not on the wire type yet — its own Go doc comment
// says so — so this editor never authors them. When they land, they get their
// own mock round rather than a quiet addition here.
import * as React from "react";
import { Plus, Trash2 } from "lucide-react";
import type { GrantSpec, PushRulesSpec, RunPolicySpec } from "../../lib/types";
import { nonNegativeInt } from "../../lib/format";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { cn } from "../ui/utils";
import { Chip, SectionLabel } from "./primitives";
import { Field } from "./form-primitives";

const UTF8 = new TextEncoder();

// Mirrors internal/api/policy.go's maxPushRulesPathBytes — the same ceiling,
// so a row's live error and the server's 400 are about the same number.
export const MAX_PUSH_RULE_PATH_BYTES = 256;

// unicode.IsControl (Go) — what the server's controlCharFree
// (internal/api/permissions.go) checks against directly — covers C0
// (U+0000-U+001F), DEL (U+007F) AND C1 (U+0080-U+009F): \x7f-\x9f is one
// contiguous range spanning DEL through the end of C1.
function hasControlChar(s: string): boolean {
  return /[\x00-\x1f\x7f-\x9f]/.test(s);
}

// stripLeadingSlash mirrors strings.TrimPrefix(pattern, "/") — DenyPathSegments'
// FIRST step, before either its whitespace or its segment check runs. Shared
// by the whitespace check below and hasEmptyPathSegment so the two read the
// exact same string DenyPathSegments does, not independently-drifting copies.
function stripLeadingSlash(pattern: string): string {
  return pattern.startsWith("/") ? pattern.slice(1) : pattern;
}

// trimLikeGo mirrors strings.TrimSpace (unicode.IsSpace). Unlike
// String.prototype.trim(), unicode.IsSpace does NOT treat U+FEFF (BOM/ZWNBSP)
// as whitespace — a pattern with only a leading/trailing BOM is legal on the
// wire (DenyPathSegments requires only valid UTF-8, which the JS string
// already is), so flagging it here would be a false positive the server
// accepts. The literal FEFF exclusion inside a negated class is the same "\S
// plus one exception" idiom on either side of `[^...]`.
function trimLikeGo(s: string): string {
  return s.replace(/^[^\S﻿]+/, "").replace(/[^\S﻿]+$/, "");
}

// hasEmptyPathSegment mirrors types.DenyPathSegments' segment split
// (internal/types/policy.go): a leading "/" is stripped, a trailing one reads
// as "everything beneath" (so it can never itself be the empty final segment),
// and no segment may be "", "." or "..". A bare "/" strips to "", which is
// itself the one empty segment — DenyPathSegments refuses it too.
function hasEmptyPathSegment(pattern: string): boolean {
  const p = stripLeadingSlash(pattern);
  const withTrailing = p.endsWith("/") ? `${p}**` : p;
  return withTrailing.split("/").some((seg) => seg === "" || seg === "." || seg === "..");
}

// pushRulePatternProblem mirrors validatePushRulePaths + DenyPathSegments
// (internal/api/policy.go, internal/types/policy.go) for ONE pattern, in the
// same order the server checks them — advisory only, the server stays the
// gate. An empty pattern is not flagged: a freshly added, unwritten row is not
// yet a mistake, and PushRulesSection never lets one reach the wire anyway
// (see withPushRules).
//
// The empty/./.. segment check renders live only for require_review_paths —
// deny_paths gets the identical check, just server-side only, at Save. That
// asymmetry is deliberate: the two lists share a pattern language, but this
// packet only asked for the review side's live check.
export function pushRulePatternProblem(pattern: string, isReview: boolean): string | null {
  const bytes = UTF8.encode(pattern).length;
  if (bytes > MAX_PUSH_RULE_PATH_BYTES) {
    return `This pattern is ${bytes} bytes. Patterns are at most ${MAX_PUSH_RULE_PATH_BYTES} bytes.`;
  }
  if (hasControlChar(pattern)) {
    return "This pattern has a control character in it, which no push path can contain.";
  }
  // DenyPathSegments strips the leading "/" BEFORE checking whitespace, so
  // "/ a" is a whitespace violation server-side even though the raw string's
  // first character is "/", not a space.
  const stripped = stripLeadingSlash(pattern);
  if (stripped.length > 0 && trimLikeGo(stripped) !== stripped) {
    return "Remove the leading or trailing space — it can never match a real path.";
  }
  // Guarded on the RAW pattern's length, not the slash-stripped one: "/" is
  // non-empty before stripping but strips to "", which is itself the empty
  // segment DenyPathSegments refuses — it must still reach this check.
  if (isReview && pattern.length > 0 && hasEmptyPathSegment(pattern)) {
    return "A path segment can't be empty, \".\", or \"..\".";
  }
  return null;
}

// pushRulesUnenforceableBySSHOnly mirrors composer/risk.go's
// pushRulesUnenforceable EXACTLY: push_rules needs the git BROKER to read the
// pushed pack, and the SSH transport has no broker seam, so a run whose only
// git-capable grant is ssh_key can set push_rules but nothing will enforce it.
export function pushRulesUnenforceableBySSHOnly(grants: readonly GrantSpec[] | undefined): boolean {
  let sawSSH = false;
  let sawBrokered = false;
  for (const g of grants ?? []) {
    if (g.kind === "ssh_key") sawSSH = true;
    else if (g.kind === "github_token" || g.kind === "git_pat") sawBrokered = true;
  }
  return sawSSH && !sawBrokered;
}

// nonGitHubPushRuleHosts mirrors composer/risk.go's nonGitHubPATHosts: the
// hosts of this run's git_pat grants other than github.com, where the
// broker's forge reader (GitHub-only) cannot check what a push left
// unchanged — so a deny_paths match there refuses every push, not just an
// offending one.
export function nonGitHubPushRuleHosts(grants: readonly GrantSpec[] | undefined): string[] {
  const hosts = new Set<string>();
  for (const g of grants ?? []) {
    if (g.kind !== "git_pat") continue;
    const host = g.scope?.host;
    if (typeof host !== "string") continue;
    const h = host.trim().toLowerCase().replace(/\.+$/, "");
    if (h && h !== "github.com") hosts.add(h);
  }
  return [...hosts].sort();
}

// The exact risk.go sentence (composer/risk.go), reused verbatim per the
// packet's Strings table (PUSH_RULES.WARN_SSH_ONLY) rather than re-authored.
export const PUSH_RULES_WARN_SSH_ONLY =
  "push_rules is set, but this run's only git-capable grant is ssh_key — the SSH transport has no broker seam, so these content rules cannot be enforced.";

// The exact risk.go sentence with the host filled in (PUSH_RULES.WARN_NON_GITHUB).
export function pushRulesWarnNonGithub(host: string): string {
  return (
    `Content rules on ${host} refuse any push whose tree still holds a path a deny pattern reaches, even one the push ` +
    "leaves untouched: Wardyn can check what a push left unchanged on github.com only. On this forge, deny " +
    "only paths the repository does not hold yet."
  );
}

// Read the four push_rules fields defensively: the panel's parseSpec is a bare
// cast over a free-form textarea, so a hand-edited document can hand this
// anything JSON allows. A non-array/non-string/non-number shape reads as
// empty/absent rather than throwing — the same "refuse to read, never crash"
// discipline toolRulesProblem's malformed-document tests pin.
function readPushRules(pr: PushRulesSpec | undefined): {
  deny: string[];
  review: string[];
  maxPack: number;
  hold: number;
} {
  const strings = (v: unknown): string[] => (Array.isArray(v) ? v.map((s) => (typeof s === "string" ? s : String(s))) : []);
  const num = (v: unknown): number => (typeof v === "number" && Number.isFinite(v) && v > 0 ? v : 0);
  return {
    deny: strings(pr?.deny_paths),
    review: strings(pr?.require_review_paths),
    maxPack: num(pr?.max_inspect_pack_mib),
    hold: num(pr?.hold_seconds),
  };
}

// Put the four fields back on the wire. Omitted/zero/empty drops the whole
// key: `push_rules: {}` and no key at all read identically everywhere else
// this field is consulted (types.PushRulesSpec.IsSet), so the shorter form is
// what a policy authored before this editor existed looks like.
//
// deny/review are filtered to non-blank entries here, on the way to the wire:
// PathListRows hands this every row's raw text, including a freshly added or
// still-blank one (so it can stay visible while the operator edits it), and a
// blank pattern is refused server-side with no row-level hint ("empty entry",
// validatePushRulePaths) and no canon string for that refusal to show live.
// This is the one place that text becomes the wire document, so it is the one
// place that filter needs to run.
function withPushRules(
  spec: RunPolicySpec,
  deny: readonly string[],
  review: readonly string[],
  maxPack: number,
  hold: number,
): RunPolicySpec {
  const nonBlank = (list: readonly string[]) => list.filter((p) => p.trim() !== "");
  const pr: PushRulesSpec = {};
  const cleanDeny = nonBlank(deny);
  const cleanReview = nonBlank(review);
  if (cleanDeny.length) pr.deny_paths = cleanDeny;
  if (cleanReview.length) pr.require_review_paths = cleanReview;
  if (maxPack > 0) pr.max_inspect_pack_mib = maxPack;
  if (hold > 0) pr.hold_seconds = hold;
  const next = { ...spec };
  if (Object.keys(pr).length === 0) delete next.push_rules;
  else next.push_rules = pr;
  return next;
}

// One path-pattern list's rows (Deny or Hold for review) — add/remove, each
// row with its own live error line directly under it.
//
// Rows carry a stable id, assigned once per row and independent of its
// position in the list — never the array index. An id-keyed row survives a
// resize correctly: removing one row leaves every OTHER row's identity (and
// DOM node, and focus) exactly where it was, instead of every row AFTER the
// removed one silently shifting down onto a neighbor's DOM node.
//
// Local `rows` state, not `paths` directly, is what gets rendered — this
// component's OWN edits never put a blank or whitespace-only entry on the
// wire (withPushRules filters those out before they reach the spec), but the
// operator still needs a visible, editable blank row right after clicking
// "Add path". Every local edit writes the FILTERED projection of `rows` back
// to the wire via `onChange`, so the two stay in lockstep without ever
// putting a blank on the wire.
//
// `paths` itself is NOT guaranteed blank-free, though: it is a prop, and a
// hand-edited JSON textarea, a pasted document, or a template can hand this
// component a document that already carries `""` or a whitespace-only entry
// — withPushRules only ever cleans up what THIS component writes, not what
// arrives from outside it. Reconciling `rows` against `paths` therefore
// happens in an effect below, keyed on the wire's own (normalized) content —
// never directly during render — specifically so a wire value this component
// can never fully match (a blank entry, which `rows`' own non-blank
// projection can never equal) settles once and stays settled, instead of
// re-triggering a state update on every render forever.
let nextRowId = 0;
interface Row {
  id: number;
  value: string;
}

function seedRows(paths: readonly string[]): Row[] {
  return paths.map((value) => ({ id: ++nextRowId, value }));
}

function sameContents(a: readonly string[], b: readonly string[]): boolean {
  return a.length === b.length && a.every((v, i) => v === b[i]);
}

function PathListRows({
  idBase,
  listLabel,
  paths,
  isReview,
  onChange,
}: {
  idBase: string;
  listLabel: string;
  paths: readonly string[];
  isReview: boolean;
  onChange: (next: string[]) => void;
}) {
  const [rows, setRows] = React.useState<Row[]>(() => seedRows(paths));
  const addRef = React.useRef<HTMLButtonElement>(null);
  // Set right before a commit that should move focus once its row exists in
  // the DOM (an add, or a remove whose neighbor should pick up focus); read
  // and cleared by the effect below, which runs after that render commits.
  const focusTargetRef = React.useRef<number | "add" | null>(null);

  // The wire value this component itself last produced (via commit, below) —
  // compared against the CURRENT `paths` prop to tell "this is what I just
  // wrote" from "something else changed the document since". A plain ref
  // assignment during render (not inside an effect) is the standard way to
  // read the latest prop from inside a later effect without adding it to a
  // dependency array.
  const pathsRef = React.useRef(paths);
  pathsRef.current = paths;
  const ownWireRef = React.useRef<readonly string[]>(paths);

  // Reconcile on an EXTERNAL change to the wire — a hand-edited JSON textarea,
  // a pasted document, a template, switching policies. This runs in an
  // effect, keyed on the wire's own content (not the `paths` array reference,
  // which is a new object every render regardless of whether anything
  // changed), so it fires exactly once per distinct wire value rather than on
  // every render: a wire value this component could never produce itself —
  // one that still carries a blank or whitespace-only entry — settles once
  // here and stays settled, instead of re-triggering on every subsequent
  // render forever.
  const wireKey = JSON.stringify(paths);
  React.useLayoutEffect(() => {
    const current = pathsRef.current;
    if (!sameContents(current, ownWireRef.current)) {
      setRows(seedRows(current));
      ownWireRef.current = current;
    }
    // wireKey is `paths` normalized to a primitive the effect can depend on
    // without re-running on every render; pathsRef (a ref, exempt from the
    // dependency list) always holds the SAME array wireKey was derived from.
  }, [wireKey]);

  const rowDomId = (id: number) => `${idBase}-row-${id}`;
  const errDomId = (id: number) => `${idBase}-err-${id}`;

  React.useEffect(() => {
    const target = focusTargetRef.current;
    focusTargetRef.current = null;
    if (target === null) return;
    if (target === "add") {
      addRef.current?.focus();
    } else {
      document.getElementById(rowDomId(target))?.focus();
    }
  });

  // Every row's raw text goes to `rows` (so a blank one stays visible) and to
  // `onChange` (so a mid-edit value is graded live and reflected everywhere
  // else that reads the document) — withPushRules is what keeps a blank or
  // whitespace-only entry off the wire, not this function. `ownWireRef` is
  // updated to match what withPushRules will actually write, so the
  // reconciliation effect recognizes the wire's NEXT render as this
  // component's own edit rather than an external one.
  function commit(next: Row[]) {
    setRows(next);
    const wire = next.map((r) => r.value);
    onChange(wire);
    ownWireRef.current = wire.filter((v) => v.trim() !== "");
  }

  function addRow() {
    const row: Row = { id: ++nextRowId, value: "" };
    focusTargetRef.current = row.id;
    commit([...rows, row]);
  }

  function editRow(id: number, value: string) {
    commit(rows.map((r) => (r.id === id ? { ...r, value } : r)));
  }

  function removeRow(id: number) {
    const i = rows.findIndex((r) => r.id === id);
    const remaining = rows.filter((r) => r.id !== id);
    // The row that shifted into the removed one's place, or the new last row
    // if the removed one was last, or the Add-path button if the list is now
    // empty.
    const next = remaining[i] ?? remaining[remaining.length - 1];
    focusTargetRef.current = next ? next.id : "add";
    commit(remaining);
  }

  return (
    <>
      {rows.map((row, i) => {
        const problem = pushRulePatternProblem(row.value, isReview);
        return (
          <div key={row.id}>
            <div className="mt-1.5 flex items-start gap-2">
              <Input
                id={rowDomId(row.id)}
                aria-label={`${listLabel} path ${i + 1}`}
                aria-invalid={problem ? true : undefined}
                aria-describedby={problem ? errDomId(row.id) : undefined}
                value={row.value}
                spellCheck={false}
                className={cn("h-8 flex-1 font-mono text-body", problem && "border-danger")}
                onChange={(e) => editRow(row.id, e.target.value)}
              />
              <Button
                type="button"
                variant="ghost"
                size="icon"
                className="size-8"
                aria-label={`Remove ${listLabel} path ${i + 1}`}
                onClick={() => removeRow(row.id)}
              >
                <Trash2 className="size-4" />
              </Button>
            </div>
            {problem && (
              <p id={errDomId(row.id)} role="alert" className="mt-1 text-xs text-danger">
                {problem}
              </p>
            )}
          </div>
        );
      })}
      <Button ref={addRef} type="button" variant="ghost" size="sm" className="mt-1.5 px-1" onClick={addRow}>
        <Plus className="size-4" /> Add path
      </Button>
    </>
  );
}

export function PushRulesSection({
  spec,
  onSpecChange,
}: {
  spec: RunPolicySpec;
  onSpecChange: (next: RunPolicySpec) => void;
}) {
  const uid = React.useId();
  const { deny, review, maxPack, hold } = readPushRules(spec.push_rules);
  const write = (nextDeny: string[], nextReview: string[], nextMaxPack: number, nextHold: number) =>
    onSpecChange(withPushRules(spec, nextDeny, nextReview, nextMaxPack, nextHold));

  const grants = Array.isArray(spec.eligible_grants) ? spec.eligible_grants : [];
  const sshWarn = (deny.length > 0 || review.length > 0 || maxPack > 0) && pushRulesUnenforceableBySSHOnly(grants);
  const nonGithubHosts = deny.length > 0 ? nonGitHubPushRuleHosts(grants) : [];

  return (
    <div className="rounded-lg border border-border p-3">
      <div className="mb-2 flex items-center gap-2">
        <SectionLabel>Push rules</SectionLabel>
        {/* The mock's own header (packet script, "Try it") carries this exact
            count next to the title in every one of its five scenarios. */}
        <span className="text-xs text-muted-foreground">
          {deny.length} deny · {review.length} held for review
        </span>
        <Chip tone="neutral" mono className="ml-auto">
          push_rules
        </Chip>
      </div>
      <p className="mb-3 text-xs leading-snug text-muted-foreground">
        Content rules for this run&apos;s brokered git pushes — WHAT a push may touch, alongside{" "}
        <span className="font-mono">git_push_any_branch</span>&apos;s WHERE.
      </p>

      {sshWarn && (
        <p role="alert" className="mb-3 text-xs text-warning">
          {PUSH_RULES_WARN_SSH_ONLY}
        </p>
      )}
      {nonGithubHosts.map((h) => (
        <p key={h} role="alert" className="mb-3 text-xs text-warning">
          {pushRulesWarnNonGithub(h)}
        </p>
      ))}

      <div className="mt-2.5">
        <SectionLabel>Deny</SectionLabel>
        <p className="mt-0.5 text-xs leading-snug text-muted-foreground">
          Refuses the push outright. Anchored at the repository root; ** crosses path segments.
        </p>
        <PathListRows
          idBase={`${uid}-deny`}
          listLabel="Deny"
          paths={deny}
          isReview={false}
          onChange={(next) => write(next, review, maxPack, hold)}
        />
      </div>

      <div className="mt-3.5">
        <SectionLabel>Hold for review</SectionLabel>
        <p className="mt-0.5 text-xs leading-snug text-muted-foreground">
          Pauses the push for an admin&apos;s decision instead of refusing it. A deny match always wins.
        </p>
        {review.length === 0 && (
          <p className="mt-1.5 text-xs italic text-muted-foreground">No paths held for review.</p>
        )}
        <PathListRows
          idBase={`${uid}-review`}
          listLabel="Hold for review"
          paths={review}
          isReview
          onChange={(next) => write(deny, next, maxPack, hold)}
        />
      </div>

      <div className="mt-3.5 grid gap-3 sm:grid-cols-2">
        <Field
          label="Inspection ceiling (MiB)"
          htmlFor={`${uid}-max-pack`}
          hint="How much of an incoming push is buffered before it's refused as too large. 0 or blank means 32."
        >
          <Input
            id={`${uid}-max-pack`}
            type="number"
            min={0}
            className="max-w-[10rem] font-mono"
            value={maxPack || ""}
            onChange={(e) => write(deny, review, nonNegativeInt(e.target.value), hold)}
          />
        </Field>
        <Field
          label="Hold time (seconds)"
          htmlFor={`${uid}-hold`}
          hint="How long a held push waits for a decision before it's refused. 0 or blank means 120."
        >
          <Input
            id={`${uid}-hold`}
            type="number"
            min={0}
            className="max-w-[10rem] font-mono"
            value={hold || ""}
            onChange={(e) => write(deny, review, maxPack, nonNegativeInt(e.target.value))}
          />
        </Field>
      </div>
    </div>
  );
}
