/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The push_rules editor — a section beside the spec, mirroring ToolRulesSection's
// own pattern (#57 packet, PR-1): it reads and writes the SAME RunPolicySpec
// document the textarea shows, so there is one source of truth and no sync to
// get wrong. deny_paths/require_review_paths are flat string lists, structurally
// simpler than tool_rules' object rows, so each gets its own add/remove list
// rather than tool_rules' named-vs-default split.
import * as React from "react";
import { Plus, Trash2 } from "lucide-react";
import type { GrantSpec, PushRulesSpec, RunPolicySpec } from "../../lib/types";
import { nonNegativeInt } from "../../lib/format";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { cn } from "../ui/utils";
import { Chip, SectionLabel } from "./primitives";
import { Field } from "./form-primitives";

// Scope check, recorded (#57 packet, PR-3): PushRulesSpec's own doc comment
// (internal/types/policy.go) says DenyNewExecutables and MaxFileSizeMiB are
// "reserved for a later change" — they are not on the wire type yet, so this
// editor deliberately never authors them. Designing UI for a field the server
// cannot accept is exactly what CONSOLE-RULES §12's mock-first rule exists to
// catch; when they land, they get their own mock round, not a quiet addition
// here.

const UTF8 = new TextEncoder();

// Mirrors internal/api/policy.go's maxPushRulesPathBytes — the same ceiling,
// so a row's live error and the server's 400 are about the same number.
export const MAX_PUSH_RULE_PATH_BYTES = 256;

// unicode.IsControl (Go) — what the server's controlCharFree
// (internal/api/permissions.go) checks against directly — covers C0
// (U+0000-U+001F), DEL (U+007F) AND C1 (U+0080-U+009F). \x7f-\x9f alone (not
// \x7f, then a gap, then \x9f) is what actually spans DEL through the C1
// block; missing that range let a C1 control (e.g. U+0085 NEL) through live
// while the server refused it (review finding F1).
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
// accepts (review finding F3). The literal FEFF exclusion inside a negated
// class is the same "\S plus one exception" idiom either side of `[^...]`.
function trimLikeGo(s: string): string {
  return s.replace(/^[^\S\uFEFF]+/, "").replace(/[^\S\uFEFF]+$/, "");
}

// hasEmptyPathSegment mirrors types.DenyPathSegments' segment split
// (internal/types/policy.go): a leading "/" is stripped, a trailing one reads
// as "everything beneath" (so it can never itself be the empty final segment),
// and no segment may be "", "." or "..".
function hasEmptyPathSegment(pattern: string): boolean {
  const p = stripLeadingSlash(pattern);
  const withTrailing = p.endsWith("/") ? `${p}**` : p;
  return withTrailing.split("/").some((seg) => seg === "" || seg === "." || seg === "..");
}

// pushRulePatternProblem mirrors validatePushRulePaths + DenyPathSegments
// (internal/api/policy.go, internal/types/policy.go) for ONE pattern, in the
// same order the server checks them — advisory only, the server stays the
// gate. An empty pattern is not flagged: a freshly added, unwritten row is not
// yet a mistake, and the server itself only refuses a truly empty SAVED entry
// (PushRulesSection drops one on blur instead — see PathListRows).
//
// isReview is the packet's own scope call (#57, PR-2 "the three questions"):
// the empty/./.. segment check renders live only for require_review_paths —
// deny_paths gets the identical check, just server-side only, at Save.
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
  // first character is "/", not a space (review finding F2).
  const stripped = stripLeadingSlash(pattern);
  if (stripped.length > 0 && trimLikeGo(stripped) !== stripped) {
    return "Remove the leading or trailing space — it can never match a real path.";
  }
  if (isReview && stripped.length > 0 && hasEmptyPathSegment(pattern)) {
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
function withPushRules(
  spec: RunPolicySpec,
  deny: readonly string[],
  review: readonly string[],
  maxPack: number,
  hold: number,
): RunPolicySpec {
  const pr: PushRulesSpec = {};
  if (deny.length) pr.deny_paths = [...deny];
  if (review.length) pr.require_review_paths = [...review];
  if (maxPack > 0) pr.max_inspect_pack_mib = maxPack;
  if (hold > 0) pr.hold_seconds = hold;
  const next = { ...spec };
  if (Object.keys(pr).length === 0) delete next.push_rules;
  else next.push_rules = pr;
  return next;
}

// One path-pattern list (Deny or Hold for review) — add/remove rows, each with
// its own live error line directly under it (#57 packet, PR-2).
//
// idBase namespaces every row/error id under this section's useId() plus
// which list this is ("deny"/"review"), so aria-describedby always points at
// THIS list's row, never the other list's same-index one.
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
  const rowId = (i: number) => `${idBase}-row-${i}`;
  const errId = (i: number) => `${idBase}-err-${i}`;
  const addRef = React.useRef<HTMLButtonElement>(null);
  // Tracks the list length across renders purely to detect "a row was just
  // added" (always appended, so it's the new last row) vs. "a row was just
  // removed" (removedIndexRef says which one) — an effect, not the click
  // handler itself, because the new/shifted DOM node the focus call needs
  // doesn't exist until the parent re-renders with the changed spec.
  const prevLength = React.useRef(paths.length);
  const removedIndexRef = React.useRef<number | null>(null);

  React.useEffect(() => {
    if (paths.length > prevLength.current) {
      document.getElementById(rowId(paths.length - 1))?.focus();
    } else if (paths.length < prevLength.current && removedIndexRef.current !== null) {
      // The row now AT the removed index (the next one shifted up), or the
      // new last row if the removed one was last, or the Add-path button if
      // the list is now empty.
      const target = paths.length > 0 ? Math.min(removedIndexRef.current, paths.length - 1) : -1;
      if (target >= 0) document.getElementById(rowId(target))?.focus();
      else addRef.current?.focus();
    }
    removedIndexRef.current = null;
    prevLength.current = paths.length;
  }, [paths.length]);

  return (
    <>
      {paths.map((p, i) => {
        const problem = pushRulePatternProblem(p, isReview);
        return (
          <div key={i}>
            <div className="mt-1.5 flex items-start gap-2">
              <Input
                id={rowId(i)}
                aria-label={`${listLabel} path ${i + 1}`}
                aria-invalid={problem ? true : undefined}
                aria-describedby={problem ? errId(i) : undefined}
                value={p}
                spellCheck={false}
                className={cn("h-8 flex-1 font-mono text-body", problem && "border-danger")}
                onChange={(e) =>
                  onChange(paths.map((row, n) => (n === i ? e.target.value : row)))
                }
                // F10: a row added and left blank must not reach Save (the
                // server 400s "empty entry" with no row-level hint, since
                // empty is deliberately not flagged live — see
                // pushRulePatternProblem's own doc comment). Dropping it here,
                // on blur, needs no new canon error string and keeps every
                // other row's live validation exactly as authored above.
                onBlur={() => {
                  if (paths[i] === "") onChange(paths.filter((_, n) => n !== i));
                }}
              />
              <Button
                type="button"
                variant="ghost"
                size="icon"
                className="size-8"
                aria-label={`Remove ${listLabel} path ${i + 1}`}
                onClick={() => {
                  removedIndexRef.current = i;
                  onChange(paths.filter((_, n) => n !== i));
                }}
              >
                <Trash2 className="size-4" />
              </Button>
            </div>
            {problem && (
              <p id={errId(i)} role="alert" className="mt-1 text-xs text-danger">
                {problem}
              </p>
            )}
          </div>
        );
      })}
      <Button
        ref={addRef}
        type="button"
        variant="ghost"
        size="sm"
        className="mt-1.5 px-1"
        onClick={() => onChange([...paths, ""])}
      >
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
            count next to the title in every one of its five scenarios — not a
            canon string (no Strings-table row), but not ambiguous either. */}
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
