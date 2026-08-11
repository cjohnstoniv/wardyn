/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Types + pure logic shared by the "Add workspace" wizard's four steps
// (wizard.tsx, step-sources/step-base-image/step-requirements/step-done.tsx).
// No React, no fetch, no DOM — component files import from here so the same
// derivation can be unit-tested without rendering anything, matching the
// convention new-run/wizard-types.ts already established for that wizard.
import type { WireIntegration, Workspace, WorkspaceProfile } from "../../../lib/types";
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
export type WizardStepId = "sources" | "image" | "integrations" | "build" | "reqs" | "verify" | "done";
export const WIZARD_STEPS: { id: WizardStepId; label: string }[] = [
  { id: "sources", label: "Sources" },
  { id: "image", label: "Base image" },
  // Integrations sit BEFORE Requirements: what a workspace connects through
  // (AI, git hosts, feeds) joins its sources + base image in shaping what the
  // Requirements step shows.
  { id: "integrations", label: "Integrations" },
  // The image build is its OWN, followable step — it used to hide inside the
  // first session launch and freeze that click for minutes.
  { id: "build", label: "Build" },
  { id: "reqs", label: "Requirements" },
  // Verify is its own step: always walk through it — drive the workspace,
  // approve/deny at the door, adjust — before Done.
  { id: "verify", label: "Verify" },
  { id: "done", label: "Done" },
];

// Where the wizard was opened from — changes ONLY Done's primary action
// (wizard.tsx / step-done.tsx); every other step is origin-independent.
export type WizardOrigin = "library" | "setup" | "run";

// The wizard's landing step when hydrated onto an EXISTING workspace (the
// "Edit workspace…" entry point) — land wherever this workspace left off,
// not always back at Sources. ponytail: two checks, no per-source
// granularity; the rail's onJump still reaches every other step from here.
export function initialStepFor(ws: Workspace): WizardStepId {
  if (ws.status === "pending_scan") return "sources";
  if (!ws.image_ref) return "image";
  return "reqs";
}

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
  // local_dir only (WorkspaceSourceInput's own "field relevance by kind"
  // rule below) — mounted read-only until explicitly opted into read-write.
  // Always a definite boolean (never undefined) so the checkbox never needs
  // a `!!` guard.
  writable: boolean;
  // True only for the auto-seeded floor row (V2C.FLOOR copy instead of the
  // generic ephemeral description) — never set by newSourceRow() callers.
  seeded?: boolean;
}

export function newSourceRow(type: WorkspaceSourceKind, seeded = false): SourceRow {
  return { id: newSourceId(), type, path: "", source: "", ref: "", target: "", writable: false, seeded };
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
  // Threaded uniformly (not just for local_dir): a repo/ephemeral row's own
  // `writable` is always false from the UI, but emitting it unconditionally
  // means a round-trip through sourceRowsFromWorkspace never silently drops
  // whatever the server actually stored, whatever the row's kind (H1).
  const writable = row.writable || undefined;
  if (row.type === "local_dir") return { type: "local_dir", path: row.path.trim(), target, writable };
  if (row.type === "repo") {
    const ref = row.ref.trim();
    return { type: "repo", source: row.source.trim(), ref: ref || undefined, target, writable };
  }
  return { type: "ephemeral", target, writable };
}

// ============================ Edit hydration (opening the wizard on an EXISTING workspace) ============================
// The inverse of toSourceInput() — seeds the Sources step from an already-
// onboarded workspace's row, for the "Edit workspace…" entry point
// (workspaces.tsx / workspace-detail.tsx). Every source gets a fresh UI-only
// id; an empty composition (shouldn't happen server-side) falls back to the
// same floor a blank wizard starts with.
export function sourceRowsFromWorkspace(ws: Workspace): SourceRow[] {
  const sources = ws.sources ?? [];
  if (sources.length === 0) return seedFloor();
  return sources.map((src) => ({
    id: newSourceId(),
    type: src.type,
    path: src.path ?? "",
    source: src.source ?? "",
    ref: src.ref ?? "",
    target: src.target ?? "",
    writable: src.writable ?? false,
  }));
}

// ============================ Base image (step ②) ============================
export type ScanStatus = "pending" | "scanning" | "done" | "failed";
export interface SourceScanState {
  status: ScanStatus;
  startedAt?: number;
  error?: string;
}

// mm:ss elapsed, clamped at 0 — ported from the mock's fmtElapsed.
export function fmtElapsed(startedAtMs: number, nowMs: number = Date.now()): string {
  const s = Math.max(0, Math.floor((nowMs - startedAtMs) / 1000));
  const m = Math.floor(s / 60);
  const pad = (n: number) => (n < 10 ? `0${n}` : String(n));
  return `${pad(m)}:${pad(s % 60)}`;
}

export type BaseImageChoice = "recommended" | "registry" | "custom" | "byo" | "catalog";

// A picked tier-2 CATALOG entry (already saved, shared across workspaces).
// Submitting it sends the entry's exact identity — the server's upsert lands
// base_image_id on the SAME catalog row, never a duplicate.
export interface CatalogPick {
  id: string;
  kind: "registry" | "custom" | "byo";
  name: string;
  image: string;
  steps?: string[];
}

// Step ②'s "Customize the build" state, held by the wizard and passed down —
// mirrors new-run/step-access.tsx's WizardState+patch convention rather than
// exploding into a dozen individual props.
export interface BaseImageState {
  choice: BaseImageChoice;
  customBase: string;
  buildSteps: string;
  byoRef: string;
  // Set iff choice === "catalog".
  catalog: CatalogPick | null;
  // UI-WS-5: the exact stored image ref for a plain (non-catalog) "registry"
  // choice hydrated from an existing workspace — carried forward byte-for-byte
  // the same way byoRef/customBase already are. Empty for a FRESH pick (the
  // card has no text field of its own), in which case toBaseImageInput falls
  // back to the suggestedRegistryImage() heuristic as before.
  registryRef: string;
}
export function defaultBaseImageState(): BaseImageState {
  return {
    choice: "recommended",
    customBase: "ubuntu:24.04",
    buildSteps: "",
    byoRef: "",
    catalog: null,
    registryRef: "",
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
  if (state.choice === "catalog" && state.catalog) {
    return { kind: state.catalog.kind, image: state.catalog.image, steps: state.catalog.steps };
  }
  if (state.choice === "registry") {
    return { kind: "registry", image: state.registryRef || suggestedRegistryImage(detectedChips) };
  }
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

// The inverse of toBaseImageInput(), for edit hydration. A "catalog" pick
// (base_image_id set) is intentionally NOT reconstructed into a CatalogPick —
// that needs a name plus a catalog fetch this hydration doesn't do — it
// degrades to its own kind's plain card (registry/custom/byo) instead, still
// carrying the exact stored image (registryRef) so a zero-edit Continue can't
// silently swap it for the client heuristic's guess (UI-WS-5). byo/custom
// round-trip byte-for-byte too (the ref/steps are carried forward verbatim
// below). The catalog row's own IDENTITY (which saved entry this was) is not
// reconstructed — the selected CARD still degrades to the plain registry
// choice — only the image it resolves to survives.
export function baseImageStateFromWorkspace(ws: Workspace): BaseImageState {
  const base = defaultBaseImageState();
  const b = ws.base_image;
  if (!b || b.kind === "recommended") return base;
  if (b.kind === "registry") return { ...base, choice: "registry", registryRef: b.image ?? "" };
  if (b.kind === "byo") return { ...base, choice: "byo", byoRef: b.image ?? "" };
  return {
    ...base,
    choice: "custom",
    customBase: b.image ?? base.customBase,
    buildSteps: (b.steps ?? []).join("\n"),
  };
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

// Mirrors internal/api/compose.go's sanitizeSecretName: a scan reports the
// ENV-VAR name the code reads ("AWS_DEFAULT_REGION"); a secret: contract row
// names an entry in Wardyn's secret STORE, whose lowercase grammar can never
// hold that shape. Lowercase, '_'/' ' → '-', other invalid runes drop, edge
// punctuation trims; "" when nothing storable remains.
export function storableSecretName(name: string): string {
  let out = "";
  for (const ch of name.toLowerCase()) {
    if ((ch >= "a" && ch <= "z") || (ch >= "0" && ch <= "9") || ch === "." || ch === "-") out += ch;
    else if (ch === "_" || ch === " ") out += "-";
  }
  out = out.replace(/^[.\-_]+|[.\-_]+$/g, "");
  return /^[a-z0-9]([a-z0-9._-]{0,126}[a-z0-9])?$/.test(out) ? out : "";
}

// Splits on the FIRST colon only, mirroring internal/api/workspaces.go's
// splitRequirementKey — a write:<path> suffix may itself legally contain colons.
export function splitRequirementKey(key: string): { type: string; rest: string } | null {
  const i = key.indexOf(":");
  if (i <= 0 || i === key.length - 1) return null;
  return { type: key.slice(0, i), rest: key.slice(i + 1) };
}

// Agent-CLI bake honesty (step ②'s "Carries", step ④ Build, Verify's carry
// card): mirrors internal/workspacescan.AgentToolsForIntegrationTypes's
// prefix rule exactly (gen.go) — NOT canDriveClaudeCode/IMPOSSIBLE above,
// which answers a different question (run-time auth compatibility — bedrock
// CAN drive an already-baked claude-code) and would show a chip for a binary
// the server never bakes. A named `anthropic_*` integration type is the only
// thing that ever bakes: codex-cli has no verified install lane and is never
// emitted server-side (AgentToolsForIntegrationTypes no longer maps
// openai_* at all), so the result is always exactly ["claude-code"] or [].
export function agentToolsCarried(
  requirements: WorkspaceRequirementsMap,
  integrations: WireIntegration[],
): string[] {
  const byId = new Map(integrations.map((w) => [w.id, w]));
  for (const key of Object.keys(requirements)) {
    const split = splitRequirementKey(key);
    if (split?.type === "integration" && byId.get(split.rest)?.type?.startsWith("anthropic_")) {
      return ["claude-code"];
    }
  }
  return [];
}

// Mirrors internal/api/workspace_run.go's repoOwnDevcontainerURL's four
// conditions exactly (minus the actual clone-URL parse, which the client has
// no use for): a repo PRIMARY source carrying its own devcontainer wins over
// the recommended/generated build entirely — resolveWorkspaceImage builds
// that devcontainer AS-IS, so nothing gen.go would bake ever reaches the
// image. Named integrations still ride along in the requirements contract,
// but agentToolsCarried's claim must be withheld wherever this is true (step
// ②'s preview included — not just Build's own post-hoc `detail` caveat,
// which doesn't exist yet at that point in the wizard).
export function repoOwnDevcontainerWins(sources: SourceRow[], profile: WorkspaceProfile | null): boolean {
  const primary = sources[0];
  return !!primary && primary.type === "repo" && !!profile?.has_devcontainer && !isSshRemote(primary);
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
    const name = storableSecretName(s.name);
    if (name) seed(requirementKey("secret", name), s.optional ? "optional" : "required");
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

// UI-WS-7: only operator-authored rows belong in a workspace's own
// requirements OVERLAY — the scan-derived half already lives on the SOURCE's
// own contract and rebuilds there on every rescan (SetSourceScanResult,
// store_sources.go). Writing a scan_seeded row back into the overlay would
// freeze it past the rescan that's supposed to refresh it: the source drops
// or changes the row, the overlay still has the stale copy, and the fold
// keeps auto-granting it forever with no editor row left to remove it from.
// Every caller that PUTs a full requirements map — whether seeding the
// contract (deriveInitialRequirements above) or persisting a single toggle
// composed onto the effective fold (requirements-card.tsx) — filters through
// this first.
export function operatorOverlay(reqs: WorkspaceRequirementsMap): WorkspaceRequirementsMap {
  const out: WorkspaceRequirementsMap = {};
  for (const [k, v] of Object.entries(reqs)) if (v.provenance === "operator_set") out[k] = v;
  return out;
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
  let reqIntegrations = 0;
  let optIntegrations = 0;
  for (const [key, v] of Object.entries(reqs)) {
    const split = splitRequirementKey(key);
    if (!split) continue;
    const required = v.level === "required";
    if (split.type === "secret") required ? reqSecrets++ : optSecrets++;
    else if (split.type === "egress") required ? reqHosts++ : optHosts++;
    else if (split.type === "write") required ? reqWrite++ : optWrite++;
    // UI-WS-9: a named integration (e.g. an operator-required Artifactory) is
    // as real a part of "always"/"on request" as a secret or a host — omitting
    // it here left it uncounted everywhere this summary renders (Done, the
    // detail page), even though it's the one row that actually opens hosts
    // and presents a credential.
    else if (split.type === "integration") required ? reqIntegrations++ : optIntegrations++;
  }
  const always: string[] = [];
  if (reqSecrets) always.push(countPhrase(reqSecrets, "secret"));
  if (reqHosts) always.push(countPhrase(reqHosts, "host"));
  if (reqWrite) always.push("write access");
  if (reqIntegrations) always.push(countPhrase(reqIntegrations, "integration"));
  const onRequest: string[] = [];
  if (optSecrets) onRequest.push(countPhrase(optSecrets, "secret"));
  if (optHosts) onRequest.push(countPhrase(optHosts, "host"));
  if (optWrite) onRequest.push("write access");
  if (optIntegrations) onRequest.push(countPhrase(optIntegrations, "integration"));
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
