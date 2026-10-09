/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type {
  MeCapabilities,
  SetupModelProvider,
  Workspace,
  WorkspaceMount,
  WorkspaceRepo,
  WorkspaceRequirementsMap,
  WorkspaceSelection,
  WorkspaceSourceInput,
} from "../../../lib/types";
import { effectiveWorkspaceRequirements } from "../../../lib/types";
import { capabilityAllowed } from "../../../lib/capabilities";

// A run-creation-time selection, widened with the per-run requirements-contract
// options CreateRunRequest.Workspaces carries (pkg/client/client.go's
// WorkspaceSelection.EnabledOptional/ReadOnly) — lib/types/workspaces.ts's
// WorkspaceSelection doesn't carry enabledOptional yet, so extend locally like
// the import above rather than widening the shared type mid-flight.
export interface RunWorkspaceSelection extends WorkspaceSelection {
  // Optional-requirement KEYS ("secret:NAME" | "egress:host" | "write:/path")
  // this run opts into for this workspace. A Required entry never needs to be
  // listed — it applies automatically. Wire: WorkspaceSelection.enabled_optional.
  enabledOptional?: string[];
}

// What a run against `ws` is actually held to: the server's FOLD of attached
// sources' contracts under the workspace's own overlay. One shared reader so
// every New Run surface (picker chips, preflight, review) reads the same
// contract the create-run gate enforces.
export function workspaceRequirements(ws: Workspace): WorkspaceRequirementsMap {
  return effectiveWorkspaceRequirements(ws);
}

// The derived sources view, in attachment order (sources[0] is primary).
function workspaceSources(ws: Workspace): WorkspaceSourceInput[] {
  return ws.sources ?? [];
}

// Whether a workspace's `secret:<name>` requirement will actually be
// auto-granted at run-create — the TRUST BOUNDARY in
// internal/api/runs_create.go's applyWorkspaceRequirements: only an
// operator_set row ever auto-mints a grant; a scan_seeded row NEVER does,
// required or optional, stored or not (untrusted repo content must never
// route the operator's own stored secrets into a run just by naming them).
// A Required-and-not-auto-granting row must never be presented as "you get
// this automatically" — see the plan's honesty constraints.
export function secretAutoGrants(ws: Workspace, name: string): boolean {
  return workspaceRequirements(ws)[`secret:${name}`]?.provenance === "operator_set";
}

// A multi-source workspace's composition ("2 dirs · 1 repo") instead of a
// single kind label. Returns null for a single-source (or sources-less, i.e.
// pre-composition) workspace, so the caller falls back to its ordinary
// single-kind rendering — only a genuinely multi-source workspace needs this.
export function compositionSummary(ws: Workspace): string | null {
  const sources = workspaceSources(ws);
  if (sources.length <= 1) return null;
  const counts = { local_dir: 0, repo: 0, ephemeral: 0 };
  for (const s of sources) if (s.type in counts) counts[s.type]++;
  const bits: string[] = [];
  if (counts.local_dir) bits.push(`${counts.local_dir} dir${counts.local_dir > 1 ? "s" : ""}`);
  if (counts.repo) bits.push(`${counts.repo} repo${counts.repo > 1 ? "s" : ""}`);
  if (counts.ephemeral) bits.push(`${counts.ephemeral} scratch dir${counts.ephemeral > 1 ? "s" : ""}`);
  return bits.join(" · ");
}

// Whether ANY of this workspace's attached sources is refused by the current
// workspace-provider policy (`admitted === false`, A3's per-repo-source wire
// flag — the ONLY value meaning "refused"; `undefined` means "nothing to
// say", never "no"). ONE predicate so the /workspaces list row and the New
// Run picker's reason line never diverge on what counts as not-admitted.
export function hasSourceNotAdmitted(ws: Workspace): boolean {
  return workspaceSources(ws).some((s) => s.admitted === false);
}

// #922 (UT-7c), the person side: is this workspace pinned (llm_cred.provider_ref,
// WorkspaceLLMCred) to a model provider this caller's own filtered list does
// not carry? `modelProviders` is GET /setup/status's own `model_providers`
// (SetupModelProvider[]) — already narrowed to what THIS caller may use by
// the server's capVisible(capModelProvider) (#832, #1015): a provider this
// caller isn't granted is dropped from that array whole, "so it reads exactly
// as a resource the deployment does not have", not merely disabled. So an
// absent match here is the caller's OWN answer, never re-derived from a grant
// table the console cannot safely read for a plain member (there is no
// member-safe way to learn a restricted value's own name — #1018 tracks the
// same gap for the refusal sentence).
//
// undefined `modelProviders` means one of two things, and this function
// deliberately can't and doesn't need to tell them apart: "haven't read the
// list yet" (or the read failed), or "this deployment has no provider block
// at all" — either way there is nothing to check the pin against, so it
// answers false (fail open), the same default hasSourceNotAdmitted's sibling
// checks take. A REAL, loaded, EMPTY array (`[]`, "granted no provider at
// all") is different from undefined and correctly answers "unavailable" —
// resolvedModelProviders (below) is what turns the wire's own distinction
// between the two into this function's own `undefined` vs `[]` contract; this
// function itself just trusts whatever it's given.
//
// Since #1018 the server itself answers this for a pin the caller isn't
// granted: it sends llm_cred.provider_unavailable in place of the id (which
// the caller may not see), so the bit alone is "unavailable". The id compare
// still covers a visible pin the caller's list lacks for another reason.
export function workspaceModelProviderUnavailable(
  ws: Workspace,
  modelProviders: { id: string }[] | undefined,
): boolean {
  if (!modelProviders) return false;
  if (ws.llm_cred?.provider_unavailable) return true;
  const ref = ws.llm_cred?.provider_ref;
  if (!ref) return false;
  return !modelProviders.some((p) => p.id === ref);
}

// UNUSABLE_PIN is the rail's pin for a workspace whose pin the server hid
// (llm_cred.provider_unavailable, #1018): still a pin, to a provider this
// person can't use, so it must preselect nothing and never let the rail
// substitute another provider (resolveProviderSelection's non-candidate pin
// rule). No provider id can equal it: ids are lowercase letters, digits and
// ._- only.
export const UNUSABLE_PIN = "(unavailable)";

// workspacePin is the pin the rail reads off a workspace: its provider id, or
// UNUSABLE_PIN when the server hid one.
export function workspacePin(ws: Workspace | undefined): string | undefined {
  if (ws?.llm_cred?.provider_unavailable) return UNUSABLE_PIN;
  return ws?.llm_cred?.provider_ref || undefined;
}

// SetupStatus.model_providers is `omitzero` on the wire (setup.go): absent
// means no provider block exists at all; a present `[]` means a block exists
// but this caller is granted nothing from it (or the deployment has no
// provider). So this is now a near-pass-through — the wire itself already
// carries the distinction workspaceModelProviderUnavailable needs. The one
// thing this function still adds: `unreachable` (the same bit
// new-run-screen.tsx's own llmReady/harnesses reads) folds to `undefined`
// too, since a failed read is not a loaded answer either way.
export function resolvedModelProviders(
  status: { unreachable?: boolean; model_providers?: SetupModelProvider[] } | null | undefined,
): SetupModelProvider[] | undefined {
  if (!status || status.unreachable) return undefined;
  return status.model_providers;
}

// #922 (widened by #1267, which also closes #1250): the STRONGER answer New
// Run's own Launch button needs, folding every person-side "isn't available
// to you" reason into one boolean. Every arm answers
// DENIED.WORKSPACE_NOT_AVAILABLE wherever this is read (workspace-card.tsx's
// own advisory line, the rail's own `workspaceUnavailable` prop,
// workspace-detail.tsx, workspaces.tsx) — one sentence, never two different
// ones for the same reason.
//
// `ws.available_to_you` (#1267) is the SAME decide path launch runs, over the
// TWO arms that apply to EVERY run type: the workspace's own capability, and
// the git-provider row its repo sources resolve to. That is strictly more
// than this function used to see on its own: the per-VALUE "Available to:
// Only these" restriction (the kind-wide switch left off, the common case
// #1249's own review found invisible) and a git-provider pin the caller lacks
// (#1250), neither of which `capabilityAllowed` alone could ever answer for.
// It deliberately excludes the model-provider pin — server-side, a Shell/exec
// run never asks that door either (createDoorIsModelRun/needsModel) — so
// `available_to_you` means the same thing for every run type and needs no
// isAgent recombination here.
//
// The model-provider arm stays exactly where #1249 put it: local,
// isAgent-gated (`workspaceModelProviderUnavailable`, undefined for a
// non-agent run so it never flashes on for one).
//
// Absent `available_to_you` (an older server) falls back to the pre-#1267
// answer: a plain ungranted workspace, via `capabilityAllowed`.
export function workspaceUnavailableToCaller(
  ws: Workspace,
  caps: MeCapabilities | null,
  modelProviders: { id: string }[] | undefined,
  isAgent: boolean,
): boolean {
  const workspaceOrProviderUnavailable =
    ws.available_to_you !== undefined ? !ws.available_to_you : !capabilityAllowed(caps, "workspace", ws.id);
  return workspaceOrProviderUnavailable || workspaceModelProviderUnavailable(ws, isAgent ? modelProviders : undefined);
}

// Resolve one WorkspaceSelection against the fetched onboarded-workspace list
// into its onboarded kind/source/name. Returns undefined for a stale selection
// (the workspace was deleted after it was picked) — buildSpec defensively skips
// those rather than emitting a dangling reference. EXPORTED: buildSpec and
// impliedEgressHosts (wizard-spec.ts) resolve selections the same way
// primaryWorkspaceId below does.
export function resolveWorkspace(sel: WorkspaceSelection, workspaces: Workspace[]): Workspace | undefined {
  return workspaces.find((w) => w.id === sel.workspaceId);
}

// Mirrors internal/api/runs_create.go's applyWriteNarrowing CLIENT-SIDE, so the
// mount buildSpec composes is ALREADY the true resolved value — Review's "exact
// policy" JSON must show what will actually be enforced, not a placeholder the
// server silently rewrites later. Required (or an enabled Optional) grants
// write by default; sel.readOnly may only narrow that to read-only, never widen
// a not-granted default to writable (matches the Go doc comment exactly).
// EXPORTED so every caller that resolves a local_dir mount's write access
// shares this ONE reader instead of re-deriving it — lib/api/compose.ts's
// resolveComposeWorkspace uses it for the AI Run Composer's `workspaces[]`
// resolution, the same way buildSpec below uses it for the manual wizard's
// workspace_mounts[]. Two derivations that can disagree is exactly the defect
// class this function exists to close.
//
// `path` is the SOURCE's own locator — the write:<locator> requirement key is
// scoped to ONE source (source_scan.go:134's seed["write:"+locator]), never
// the whole workspace. Defaults to ws.source (the single-source mirror) so
// every existing 2-arg call site keeps its old whole-workspace behavior
// unchanged; a multi-source caller (resolveWorkspaceMounts below,
// resolveComposeWorkspace) passes each source's own path explicitly — a
// multi-source workspace can carry several write:<path> rows with DIFFERENT
// levels, which a single aggregated "does ANY of them grant write" flag would
// blur across sources (PARITY-2).
export function resolvedMountReadOnly(
  ws: Workspace,
  sel: RunWorkspaceSelection,
  path: string = ws.source,
): boolean {
  const req = workspaceRequirements(ws)[`write:${path}`];
  // A source the operator explicitly ticked "Allow writes to this directory"
  // on grants write the same way a required write: row does. Without this the
  // checkbox in AddWorkspaceDialog is a NO-OP for anything launched from the
  // UI: it stores sources[].writable=true, nothing ever creates a write: row,
  // so every mount resolved here came out read-only and an agent's edits could
  // not reach the host. internal/api/workspace_run.go already does exactly
  // this (`ro := !src.Writable`) — this is the client mirror catching up, and
  // it widens nothing that a human did not tick.
  const src = resolvableSources(ws).find((s) => (s.path ?? s.source) === path);
  const grantedDefault =
    req?.level === "required" ||
    (sel.enabledOptional ?? []).includes(`write:${path}`) ||
    src?.writable === true;
  if (!grantedDefault) return true;
  return sel.readOnly === true;
}

// The workspace's REAL source list a mount/repo resolution should iterate —
// ws.sources[] when the record carries it (every server-fetched Workspace
// does — PARITY-2), else a SYNTHETIC single-entry list built from the legacy
// kind/source mirror, so a hand-built fixture (or a stale cached record) that
// never populated .sources resolves EXACTLY as the old single-mirror code
// did. Exported so lib/api/compose.ts's resolveComposeWorkspace shares the
// SAME fallback resolveWorkspaceMounts uses below — two derivations of "what
// are this workspace's sources" that could disagree is the exact defect class
// this function exists to close.
export function resolvableSources(ws: Workspace): WorkspaceSourceInput[] {
  if (ws.sources?.length) return ws.sources;
  if (ws.kind === "local_dir") {
    return [{ type: "local_dir", path: ws.source, target: ws.default_target }];
  }
  if (ws.kind === "repo") {
    return [{ type: "repo", source: ws.source, target: ws.default_target }];
  }
  // "ephemeral", or an unrecognized/empty kind (a genuinely multi-source
  // record whose sources[] wasn't fetched) — no host path to mount, no repo
  // to clone; callers correctly emit nothing for it rather than a garbage
  // empty-string mount source (the exact PARITY-2 bug).
  return [{ type: "ephemeral", target: ws.default_target }];
}

// Resolve ONE workspace selection into its workspace_mounts[]/workspace_repos[]
// entries — one WorkspaceMount per local_dir source, one WorkspaceRepo per repo
// source, nothing for ephemeral (PARITY-2: the old code flattened to the
// workspace's single-mirror kind/source, which is EMPTY for a multi-source or
// migrated-ephemeral workspace, so it silently mounted nothing at all).
// Exported so StepReview's own preview (if it ever needs one) and buildSpec
// below share the identical per-source resolution.
export function resolveWorkspaceMounts(
  w: Workspace,
  sel: RunWorkspaceSelection,
): { mounts: WorkspaceMount[]; repos: WorkspaceRepo[] } {
  const mounts: WorkspaceMount[] = [];
  const repos: WorkspaceRepo[] = [];
  resolvableSources(w).forEach((src, i) => {
    // The picker's single "Target override" field (workspace-picker.tsx) only
    // ever meant one mount point per workspace — apply it to the FIRST source
    // only; every other source keeps its own onboarded target.
    const override = i === 0 ? sel.target?.trim() : undefined;
    if (src.type === "repo" && src.source) {
      const target = override || src.target?.trim();
      const entry: WorkspaceRepo = { repo: src.source };
      if (target) entry.target = target;
      repos.push(entry);
    } else if (src.type === "local_dir" && src.path) {
      mounts.push({
        source: src.path,
        // Mount at the agent's working dir (~/work = /home/agent/work) by
        // convention — that's where `claude` and the `wardyn run attach` shell
        // start. A source's own target, or a per-run override, takes
        // precedence.
        target: override || src.target?.trim() || "/home/agent/work",
        // The workspace's requirements contract decides write access now (a
        // Required write, or an enabled Optional one) — not a bare per-run
        // flag. See resolvedMountReadOnly.
        read_only: resolvedMountReadOnly(w, sel, src.path),
      });
    }
    // ephemeral: no host path to mount and no repo to clone — the server
    // mkdirs the scratch target itself; nothing for the policy to carry.
  });
  return { mounts, repos };
}

// primaryWorkspaceId mirrors the server's own primary pick (referencedWorkspaces,
// internal/api/workspace_run.go): it walks the RESOLVED spec's workspace_mounts
// in FULL before workspace_repos, so the primary is whichever SELECTED
// workspace contributes the FIRST local_dir mount — never simply
// selections[0] (PARITY-3). A workspace's OWN composition decides eligibility
// (resolvableSources, so a multi-source workspace with a local_dir source
// anywhere in it still counts — PARITY-2), never the flattened single-mirror
// kind. Used everywhere a "primary workspace" drives a decision (the
// model-access binding, Review's summary) so the console can't name a
// different workspace's credential than the run actually inherits.
export function primaryWorkspaceId(
  selections: RunWorkspaceSelection[],
  workspaces: Workspace[],
): string | undefined {
  const resolved = selections
    .map((sel) => ({ id: sel.workspaceId, w: resolveWorkspace(sel, workspaces) }))
    .filter((x): x is { id: string; w: Workspace } => !!x.w);
  const local = resolved.find(({ w }) => resolvableSources(w).some((s) => s.type === "local_dir"));
  if (local) return local.id;
  const repo = resolved.find(({ w }) => resolvableSources(w).some((s) => s.type === "repo"));
  return repo?.id;
}

// One entry of CreateRunRequest.Workspaces (pkg/client/client.go's
// WorkspaceSelection) — snake_case wire shape, distinct from this module's own
// camelCase RunWorkspaceSelection (the wizard's per-attachment STATE).
export interface RunWorkspaceSelectionWire {
  workspace_id: string;
  enabled_optional?: string[];
  read_only?: boolean;
}

// toRunWorkspacesWire converts per-workspace selections to the wire shape
// above, emitting an entry ONLY for a selection that opts into something —
// an all-defaults selection is a no-op the server doesn't need to see (a
// Required entry never needs one; it applies automatically). Shared by
// buildSpec (the manual wizard) and the AI Run Composer's approveLaunch
// (new-run-dialog.tsx) so the two paths can't drift on what counts as
// "non-default".
export function toRunWorkspacesWire(selections: RunWorkspaceSelection[]): RunWorkspaceSelectionWire[] {
  const out: RunWorkspaceSelectionWire[] = [];
  for (const sel of selections) {
    if (!sel.enabledOptional?.length && sel.readOnly === undefined) continue;
    const entry: RunWorkspaceSelectionWire = { workspace_id: sel.workspaceId };
    if (sel.enabledOptional?.length) entry.enabled_optional = sel.enabledOptional;
    if (sel.readOnly !== undefined) entry.read_only = sel.readOnly;
    out.push(entry);
  }
  return out;
}
