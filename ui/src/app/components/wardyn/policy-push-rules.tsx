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
import { Chip, SectionLabel } from "./primitives";
import { Field } from "./form-primitives";

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

// One path-pattern list (Deny or Hold for review) — add/remove rows. Live
// per-row validation lands in a follow-up (#57 packet, PR-2).
function PathListRows({
  listLabel,
  paths,
  onChange,
}: {
  listLabel: string;
  paths: readonly string[];
  onChange: (next: string[]) => void;
}) {
  return (
    <>
      {paths.map((p, i) => (
        <div key={i} className="mt-1.5 flex items-start gap-2">
          <Input
            aria-label={`${listLabel} path ${i + 1}`}
            value={p}
            spellCheck={false}
            className="h-8 flex-1 font-mono text-body"
            onChange={(e) => onChange(paths.map((row, n) => (n === i ? e.target.value : row)))}
          />
          <Button
            type="button"
            variant="ghost"
            size="icon"
            className="size-8"
            aria-label={`Remove ${listLabel} path ${i + 1}`}
            onClick={() => onChange(paths.filter((_, n) => n !== i))}
          >
            <Trash2 className="size-4" />
          </Button>
        </div>
      ))}
      <Button
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
        <PathListRows listLabel="Deny" paths={deny} onChange={(next) => write(next, review, maxPack, hold)} />
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
          listLabel="Hold for review"
          paths={review}
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
