/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Types + pure logic shared by the "Add workspace" wizard's four steps
// (wizard.tsx, step-sources/step-base-image/step-requirements/step-done.tsx).
// No React, no fetch, no DOM — component files import from here so the same
// derivation can be unit-tested without rendering anything, matching the
// convention new-run/wizard-types.ts already established for that wizard.
import type { WorkspaceProfile } from "../../../lib/types";
import type {
  RequirementLevel,
  WorkspaceBaseImageInput,
  WorkspaceRequirement,
  WorkspaceRequirementsMap,
  WorkspaceSourceInput,
  WorkspaceSourceKind,
} from "../../../lib/api/workspaces";
import { IMPOSSIBLE, type AiType } from "../../../lib/integrations";

export type {
  RequirementLevel,
  WorkspaceBaseImageInput,
  WorkspaceRequirement,
  WorkspaceRequirementsMap,
  WorkspaceSourceInput,
  WorkspaceSourceKind,
};

// ============================ Rail ============================
export type WizardStepId = "sources" | "image" | "reqs" | "done";
export const WIZARD_STEPS: { id: WizardStepId; label: string }[] = [
  { id: "sources", label: "Sources" },
  { id: "image", label: "Base image" },
  { id: "reqs", label: "Requirements" },
  { id: "done", label: "Done" },
];

// Where the wizard was opened from — changes ONLY Done's primary action
// (wizard.tsx / step-done.tsx); every other step is origin-independent.
export type WizardOrigin = "library" | "setup" | "run";

// ============================ Sources (step ①) ============================
// The composition floor's in-sandbox scratch path — matches the server's own
// default (internal/api/workspaces.go's defaultEphemeralTarget) so a source
// left at its default previews the exact target the server would give it.
export const DEFAULT_TARGET = "/home/agent/work";

let sourceSeq = 0;
export function newSourceId(): string {
  sourceSeq += 1;
  return `src_${sourceSeq}_${Math.random().toString(36).slice(2, 8)}`;
}

// One row in the Sources step — a superset of WorkspaceSourceInput's fields
// (adds the UI-only `id`/`seeded`); toSourceInput() strips it back to the wire
// shape. All string fields are kept as controlled-input strings (never
// undefined) so a row can always round-trip through a text input.
export interface SourceRow {
  id: string;
  type: WorkspaceSourceKind;
  path: string;
  source: string;
  ref: string;
  target: string;
  // True only for the auto-seeded floor row (V2C.FLOOR copy instead of the
  // generic ephemeral description) — never set by newSourceRow() callers.
  seeded?: boolean;
}

export function newSourceRow(type: WorkspaceSourceKind, seeded = false): SourceRow {
  return { id: newSourceId(), type, path: "", source: "", ref: "", target: "", seeded };
}

// A fresh wizard's floor: one seeded ephemeral row. The >=1-source floor is
// STRUCTURAL, not a validation error (V2C.FLOOR) — removeSource() re-seeds
// this the instant the last other source is removed, so `sources` is never empty.
export function seedFloor(): SourceRow[] {
  return [newSourceRow("ephemeral", true)];
}

// Mirrors the approved mock's removeSrc(): "if (!next.length) next = [seedEph()]".
export function removeSource(rows: SourceRow[], id: string): SourceRow[] {
  const next = rows.filter((r) => r.id !== id);
  return next.length > 0 ? next : seedFloor();
}

// The lone ephemeral row can't be removed while it's the ONLY source (the
// floor); every other row — and an ephemeral row once a second source exists
// — is always removable.
export function isRemovable(row: SourceRow, rows: SourceRow[]): boolean {
  return !(row.type === "ephemeral" && rows.length === 1);
}

function baseNameOf(row: SourceRow): string {
  if (row.type === "repo") {
    const cleaned = (row.source || "repo").replace(/\.git$/, "");
    return cleaned.split(/[/:]/).filter(Boolean).pop() || "repo";
  }
  if (row.type === "local_dir") {
    return (row.path || "").replace(/\/+$/, "").split("/").filter(Boolean).pop() || "dir";
  }
  return "scratch";
}

// Auto-derives a distinct sub-path once several sources exist, so two mounted
// sources don't collide on the same in-sandbox target by default. An
// operator-typed target always wins.
export function defaultTargetFor(row: SourceRow, rows: SourceRow[]): string {
  const typed = row.target.trim();
  if (typed) return typed;
  return rows.length <= 1 ? DEFAULT_TARGET : `${DEFAULT_TARGET}/${baseNameOf(row)}`;
}

// ---- Repo source parsing (host + SSH-remote detection) ----
// Ported from the approved mock's parseRepoSource (wardyn-workspaces.js) — a
// light CLIENT-SIDE parse feeding the Access block's host/lane lookup, NOT a
// validator (the server's repoCloneURL, internal/api/workspaces.go, is the
// real gate). Recognizes an SSH remote (git@host:org/repo[.git]), an http(s)
// URL, or a bare "org/repo" slug (assumed github.com, matching the mock).
export interface ParsedRepoSource {
  host: string;
  ssh: boolean;
}
export function parseRepoSource(src: string): ParsedRepoSource | null {
  const s = (src || "").trim();
  if (!s) return null;
  const sshMatch = /^git@([^:]+):(.+)$/.exec(s);
  if (sshMatch) return { host: sshMatch[1], ssh: true };
  const httpMatch = /^https?:\/\/([^/]+)\//.exec(s);
  if (httpMatch) return { host: httpMatch[1], ssh: false };
  if (/^[\w.-]+\/[\w.-]+$/.test(s)) return { host: "github.com", ssh: false };
  return null;
}

// Bare-minimum client-side shape check for the step's Continue gate — mirrors
// the INTENT of runner.ValidateMountSource/repoCloneURL, not their exact
// rules; the server (validateWorkspaceSource, internal/api/workspaces.go)
// remains the real gate.
export function isSourceShapeValid(row: SourceRow): boolean {
  if (row.type === "local_dir") return /^\//.test(row.path.trim());
  if (row.type === "repo") return !!parseRepoSource(row.source);
  return true;
}

// A repo row's SSH gate: its source is an SSH remote but no ssh-key-<slug>
// secret is stored for that host yet. Checked by the caller against the lanes
// deriveProviders() returns for the row's host (scm-provider.ts) — kept here
// only as the "is this row SSH-shaped" half so step-sources.tsx and
// step-base-image.tsx (Phase A's immediate-fail case) agree on one definition.
export function isSshRemote(row: SourceRow): boolean {
  return row.type === "repo" && !!parseRepoSource(row.source)?.ssh;
}

export function toSourceInput(row: SourceRow, rows: SourceRow[]): WorkspaceSourceInput {
  const target = defaultTargetFor(row, rows);
  if (row.type === "local_dir") return { type: "local_dir", path: row.path.trim(), target };
  if (row.type === "repo") {
    const ref = row.ref.trim();
    return { type: "repo", source: row.source.trim(), ref: ref || undefined, target };
  }
  return { type: "ephemeral", target };
}

// ============================ Base image (step ②) ============================
export type ScanStatus = "pending" | "scanning" | "done" | "failed";
export interface SourceScanState {
  status: ScanStatus;
  startedAt?: number;
  secs?: number;
  error?: string;
}

// mm:ss elapsed, clamped at 0 — ported from the mock's fmtElapsed.
export function fmtElapsed(startedAtMs: number, nowMs: number = Date.now()): string {
  const s = Math.max(0, Math.floor((nowMs - startedAtMs) / 1000));
  const m = Math.floor(s / 60);
  const pad = (n: number) => (n < 10 ? `0${n}` : String(n));
  return `${pad(m)}:${pad(s % 60)}`;
}

export type BaseImageChoice = "recommended" | "registry" | "custom" | "byo";

// Step ②'s "Customize the build" state, held by the wizard and passed down —
// mirrors new-run/step-access.tsx's WizardState+patch convention rather than
// exploding into a dozen individual props.
export interface BaseImageState {
  choice: BaseImageChoice;
  customBase: string;
  // Keyed by the detected tool/language chip it toggles (e.g. "Go 1.22") — the
  // real profile's own vocabulary, never a hardcoded Go/Node/TS list.
  customTools: Record<string, boolean>;
  // The agent-tool (Claude Code CLI) toggle — separate from customTools since
  // its very presence in the checklist depends on harnessAvailable, unlike an
  // ordinary detected language/package-manager tool.
  harnessTool: boolean;
  extraTools: string[];
  toolDraft: string;
  buildSteps: string;
  byoRef: string;
}
export function defaultBaseImageState(): BaseImageState {
  return {
    choice: "recommended",
    customBase: "ubuntu:24.04",
    customTools: {},
    harnessTool: true,
    extraTools: [],
    toolDraft: "",
    buildSteps: "",
    byoRef: "",
  };
}

// A registry image "that fits" is display-only client guesswork (there is no
// suggest-an-image endpoint) — a light heuristic off the detected chips,
// falling back to a generic devcontainer base. Shared by the card's display
// and toBaseImageInput() below so the two can't disagree about what "fits" means.
export function suggestedRegistryImage(detectedChips: string[]): string {
  const lower = detectedChips.map((c) => c.toLowerCase());
  if (lower.some((c) => c.includes("go"))) return "mcr.microsoft.com/devcontainers/go:1";
  if (lower.some((c) => c.includes("python"))) return "mcr.microsoft.com/devcontainers/python:3";
  if (lower.some((c) => c.includes("node") || c.includes("typescript"))) {
    return "mcr.microsoft.com/devcontainers/typescript-node:20";
  }
  return "mcr.microsoft.com/devcontainers/base:ubuntu";
}

export function toBaseImageInput(state: BaseImageState, detectedChips: string[] = []): WorkspaceBaseImageInput {
  if (state.choice === "registry") return { kind: "registry", image: suggestedRegistryImage(detectedChips) };
  if (state.choice === "byo") return { kind: "byo", image: state.byoRef.trim() };
  if (state.choice === "custom") {
    return {
      kind: "custom",
      image: state.customBase.trim(),
      steps: state.buildSteps.split("\n").map((l) => l.trim()).filter(Boolean),
    };
  }
  return { kind: "recommended" };
}

// ---- Resolved model/harness power source (step ②'s power-source line + peek) ----
// Deliberately NOT wired to a real llm_cred write yet: the backend moved
// WorkspaceLLMCred to an integration_ref shape (internal/types/types.go) that
// ui/src/app/lib/types/workspaces.ts's WorkspaceLLMCred (mode/api_key_secret/
// bedrock) hasn't caught up to. This stays wizard-local state — a later step
// wires the pin through once that shared type catches up.
export type PowerSource =
  | { kind: "default" }
  | { kind: "none" }
  | { kind: "pinned"; integrationId: string; name: string };

// Credential-shaped-line detection for the custom build-steps editor — WARNS,
// never blocks (V2C.CRED_WARN). Ported verbatim from the mock's CRED_PATTERNS.
export const CRED_PATTERNS: { re: RegExp; kind: string }[] = [
  { re: /AKIA[0-9A-Z]{12,}/, kind: "AWS access key–shaped" },
  { re: /ghp_[A-Za-z0-9]{20,}/, kind: "GitHub token–shaped" },
  { re: /\bsk-[A-Za-z0-9_-]{16,}/, kind: "API key–shaped" },
  { re: /-----BEGIN [A-Z ]*PRIVATE KEY-----/, kind: "private key block" },
  { re: /Authorization:\s*Bearer\s+\S+/i, kind: "bearer token header" },
  { re: /^\s*ENV\s+\w+=?\s*['"]?[A-Za-z0-9+/_-]{32,}['"]?\s*$/, kind: "high-entropy ENV value" },
];

export interface CredFlag {
  line: number;
  kind: string;
}

export function credFlags(text: string): CredFlag[] {
  const out: CredFlag[] = [];
  (text || "").split("\n").forEach((line, i) => {
    const hit = CRED_PATTERNS.find((p) => p.re.test(line));
    if (hit) out.push({ line: i + 1, kind: hit.kind });
  });
  return out;
}

// Whether an AI integration of this type can drive Claude Code at all — the
// step ②/peek "incompatible rows muted" rule reads straight off the same
// IMPOSSIBLE map the Integrations screen renders reasons from, so the two
// surfaces can't disagree about what's compatible.
export function canDriveClaudeCode(aiType: AiType | undefined): boolean {
  if (!aiType) return false;
  return !IMPOSSIBLE[aiType]?.claude_code;
}

// ============================ Requirements (step ③) ============================
export function requirementKey(type: "secret" | "egress" | "write" | "integration", rest: string): string {
  return `${type}:${rest}`;
}

// Splits on the FIRST colon only, mirroring internal/api/workspaces.go's
// splitRequirementKey — a write:<path> suffix may itself legally contain colons.
export function splitRequirementKey(key: string): { type: string; rest: string } | null {
  const i = key.indexOf(":");
  if (i <= 0 || i === key.length - 1) return null;
  return { type: key.slice(0, i), rest: key.slice(i + 1) };
}

// Seeds a requirements map from the scanned profile — the "Defaults below
// came from the scan" starting point (C.SEEDED) an operator then edits. A
// secret defaults to required unless the scan itself flagged it optional
// (SecretNeed.optional); an auto-allowed egress host defaults to required (it
// is already reachable; this just states it as part of the contract); a
// local_dir's write access defaults to optional (mounted read-only until a
// run asks for more — C.REPO_RO-adjacent honesty for the write row). Existing
// entries — a prior PUT, or the operator's own edits this session — always
// win: this only fills in what's missing, never overwrites a decision.
export function deriveInitialRequirements(
  profile: WorkspaceProfile | null | undefined,
  localDirPaths: string[],
  existing?: WorkspaceRequirementsMap,
): WorkspaceRequirementsMap {
  const out: WorkspaceRequirementsMap = { ...existing };
  const seed = (key: string, level: RequirementLevel) => {
    if (!out[key]) out[key] = { level, provenance: "scan_seeded" };
  };
  for (const s of profile?.required_secrets ?? []) {
    seed(requirementKey("secret", s.name), s.optional ? "optional" : "required");
  }
  for (const host of profile?.egress_domains ?? []) {
    seed(requirementKey("egress", host), "required");
  }
  for (const path of localDirPaths) {
    seed(requirementKey("write", path), "optional");
  }
  return out;
}

// A row's lane edit — always "operator_set" (an explicit click), whether it's
// flipping an existing row's lane or promoting a holding-block/advisory
// candidate into the contract for the first time.
export function setRequirementLane(
  reqs: WorkspaceRequirementsMap,
  key: string,
  level: RequirementLevel,
): WorkspaceRequirementsMap {
  return { ...reqs, [key]: { level, provenance: "operator_set" } };
}

// ============================ Contract summary (step ③ header, step ④) ============================
export interface ContractSummary {
  always: string[];
  onRequest: string[];
}

function countPhrase(n: number, noun: string): string {
  return `${n} ${noun}${n === 1 ? "" : "s"}`;
}

export function summarizeRequirements(reqs: WorkspaceRequirementsMap): ContractSummary {
  let reqSecrets = 0;
  let optSecrets = 0;
  let reqHosts = 0;
  let optHosts = 0;
  let reqWrite = 0;
  let optWrite = 0;
  for (const [key, v] of Object.entries(reqs)) {
    const split = splitRequirementKey(key);
    if (!split) continue;
    const required = v.level === "required";
    if (split.type === "secret") required ? reqSecrets++ : optSecrets++;
    else if (split.type === "egress") required ? reqHosts++ : optHosts++;
    else if (split.type === "write") required ? reqWrite++ : optWrite++;
  }
  const always: string[] = [];
  if (reqSecrets) always.push(countPhrase(reqSecrets, "secret"));
  if (reqHosts) always.push(countPhrase(reqHosts, "host"));
  if (reqWrite) always.push("write access");
  const onRequest: string[] = [];
  if (optSecrets) onRequest.push(countPhrase(optSecrets, "secret"));
  if (optHosts) onRequest.push(countPhrase(optHosts, "host"));
  if (optWrite) onRequest.push("write access");
  return { always, onRequest };
}

// Required secrets not yet in the store — "runs start without it" (C.UNMET_OK).
export function unmetRequiredSecrets(reqs: WorkspaceRequirementsMap, storedSecretNames: string[]): string[] {
  const out: string[] = [];
  for (const [key, v] of Object.entries(reqs)) {
    if (v.level !== "required") continue;
    const split = splitRequirementKey(key);
    if (split?.type === "secret" && !storedSecretNames.includes(split.rest)) out.push(split.rest);
  }
  return out;
}

// Two-tier leak classification, ported verbatim from the mock's TEST_PATH_RE
// (wardyn-workspaces.js). A key-shaped string under a test-conventional path is
// USUALLY a fixture — "rotate" is meaningless advice for one — so those group
// into a muted, collapsed tier. Never suppression: still shown, still counted.
export const TEST_PATH_RE =
  /(^|\/)(testdata|__tests__|fixtures)(\/|$)|_test\.(go|py|rb|js|ts|tsx)$|\.(test|spec)\.[a-z0-9]+$/i;

export function isFixtureLeak(lk: { path: string }): boolean {
  return TEST_PATH_RE.test(lk.path);
}
