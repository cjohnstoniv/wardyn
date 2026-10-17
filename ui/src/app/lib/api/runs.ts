/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Run lifecycle: list/get/create/preflight/profile/kill + attach ticket +
// credential-grant eligibility. Consumed directly (import { runs }) so a route
// that never touches runs drops this module from its chunk.
import type {
  AdminProxyWindowResult,
  RunCapacityResponse,
  AdminRestartResponse,
  AgentRun,
  AttachHolder,
  CreateRunInput,
  CreateRunResult,
  CredentialGrant,
  PolicyGrade,
  PreflightResult,
  ProfileProposal,
  ReviveResult,
  RunDetail,
  RunEndWaitResult,
  RunFilesResult,
  RunPolicySpec,
  RunPolicyView,
  RunResources,
} from "../types";
import type { ComponentRef } from "../types/components";
import type { PolicyPreviewResult } from "../types/policy-preview";
import type {
  Experience,
  IncludedTool,
  PlacementValue,
  RepoSelection,
  RequestedResources,
  RunOverrides,
  RunStartup,
  RunWorkload,
  StartFolder,
} from "../types/new-run-contract";
import {
  asJson,
  errText,
  HttpError,
  LAUNCH_DEADLINE_MS,
  LIST_LIMIT,
  str,
  unwrapList,
  wfetch,
  withLimit,
} from "./core";

// Map a backend credential-grant eligibility record (the GET /runs/{id}/grants
// shape: { id, run_id, created_at, spec: { kind, scope, ttl_seconds,
// requires_approval } }) into the CredentialGrant shape the run-detail screen
// renders. These are ELIGIBILITY records (what the run may request), not issued
// credentials, so there is no jti/expiry; they render as "active" eligibility.
// The host a git_pat / ssh_key grant's scope names — the one scope field the
// kill outcome's per-kind lines carry (held-credentials.ts); never the scope.
function grantHost(scope: unknown): string | undefined {
  return scope != null && typeof scope === "object" ? str((scope as Record<string, unknown>).host) : undefined;
}

function grantsFromRecords(payload: unknown): CredentialGrant[] {
  return unwrapList<Record<string, unknown>>(payload).map((g) => {
    const spec = (g.spec ?? {}) as Record<string, unknown>;
    const kind = str(spec.kind) ?? "—";
    // Render the scope object compactly; fall back to the kind when absent.
    let scope = kind;
    if (spec.scope != null && typeof spec.scope === "object") {
      try {
        scope = `${kind} ${JSON.stringify(spec.scope)}`;
      } catch {
        scope = kind;
      }
    }
    return {
      id: str(g.id) ?? "—",
      scope,
      audience: kind,
      state: "active",
      minted_at: str(g.created_at),
      host: grantHost(spec.scope),
    } satisfies CredentialGrant;
  });
}

export type RunWireInput = (Partial<AgentRun> | CreateRunInput) & {
  interactive?: boolean;
  inline_policy?: RunPolicySpec;
  // Per-run half of the requirements contract: which optional requirements
  // this run enables, plus any read-only narrowing, per attached workspace.
  workspaces?: { workspace_id: string; enabled_optional?: string[]; read_only?: boolean; target?: string; repos?: RepoSelection[] }[];
  // Primary-workspace id for a selection that resolves to no mount/repo (a
  // pure-ephemeral / migrated-0029 workspace) — routes its base_image through
  // the server's seedRequestWorkspace, which the mount-less spec can't.
  workspace_id?: string;
  // Explicit model-access override — tier 1 of the server's resolution chain.
  integration_id?: string;
  // The member's per-run USER DRIVE opt-in (pkg/client.DriveSelection).
  // Nothing here names a drive or a path: the server resolves which drive
  // belongs to the authenticated caller and derives their directory from their
  // own identity, so this flag can only ever ask for storage already granted.
  // read_only NARROWS a writable allocation; false is a no-op the server
  // refuses as a widening, so buildSpec omits it rather than sending it.
  drive?: { enabled: boolean; read_only?: boolean };
  // #542/#526 — this run's chosen model provider (CreateRunRequest.ModelProvider).
  model_provider?: string;
  // The components this run carries (CreateRunRequest.Components, #1914): a
  // stored one by id or a run-only inline definition. Absent is none.
  components?: ComponentRef[];
  // The 0.9 New Run contract (pkg/client.CreateRunRequest): where the run lives
  // ("Runs on" in the console), its size, and the person's per-component edits.
  // Each is absent unless the person chose it; the console never sends a value
  // for a field whose section is inactive (see lib/run-overrides.ts).
  placement?: PlacementValue;
  runner_id?: string;
  // The pool the run starts on (pkg/client.CreateRunRequest.RunnerPoolID); absent
  // keeps today's placement, and once pools are managed applies the person's
  // own default, then the organisation's.
  runner_pool_id?: string;
  // One of the preview's allowed_images, by ref (pkg/client.CreateRunRequest.AllowedImage).
  allowed_image?: string;
  resources?: RequestedResources;
  overrides?: RunOverrides;
  // The run-mode contract (pkg/client/runs_mode.go). A request with the marker or
  // an experience is a new client's: the older agent, task, interactive,
  // task_mode, interactive_start, seed_auto_tools, tool_approvals and
  // model_provider are then omitted: the carriers replace them and the server refuses both.
  // The contract marker (pkg/client.RunModeContractV09): New Run always
  // sends it, so a request whose mode is unset is refused by the server and never
  // read as an older client's.
  mode_contract?: "0.9";
  experience?: Experience;
  workload?: RunWorkload;
  tools?: IncludedTool[];
  startup?: RunStartup;
  start_folder?: StartFolder;
  no_repositories_or_drives?: boolean;
};

// The create-time model-credential refusal — writeProviderRefusal's 422
// carrying reason `model_credential`, the failure audit row's own word. It is
// the one launch refusal a sign-in repairs, so the New Run rail answers it with
// the named provider's door and launches again. Beside createRun because that is
// the call that raises it, and so the screen (at the file-size gate) reaches it
// through the import line it already has.
export function isCredentialRefusal(e: unknown): boolean {
  return e instanceof HttpError && e.status === 422 && e.reason === "model_credential";
}

// The create-time git-credential refusal (#386's launch door) —
// gitCredentialRefusal's 422 carrying reason `git_credential`: a repository
// admitted onto a per-user Azure DevOps row with no usable captured sign-in
// for this caller. The New Run rail answers it with the Connect Azure DevOps
// dialog and launches again once the connection lands — isCredentialRefusal's
// twin, for a second credential.
export function isGitCredentialRefusal(e: unknown): boolean {
  return e instanceof HttpError && e.status === 422 && e.reason === "git_credential";
}

// #159: the Recordings screen paginates instead of stopping at LIST_LIMIT
// (1000). unwrapList discards the response object, so it can never carry
// X-Wardyn-Truncated — the caller needs it FROM listRuns, not from a second
// helper duplicating this fetch. Overloaded so every existing caller (the
// board's poll, the shell's mount probe, permissions, new-run's preflight, …)
// keeps asking for the plain default page and keeps getting a bare
// AgentRun[] back, unchanged; only a caller that opts into BOTH limit and
// offset gets the wider { runs, truncated } shape carrying the header.
export type PagedRuns = { runs: AgentRun[]; truncated: boolean };

async function listRuns(opts: {
  includeRecordingMeta?: boolean;
  limit: number;
  offset: number;
}): Promise<PagedRuns>;
async function listRuns(opts?: { includeRecordingMeta?: boolean }): Promise<AgentRun[]>;
async function listRuns(
  opts?: { includeRecordingMeta?: boolean; limit?: number; offset?: number },
): Promise<AgentRun[] | PagedRuns> {
  const paging = opts?.limit != null && opts?.offset != null;
  let path = paging ? `/runs?limit=${opts.limit}&offset=${opts.offset}` : withLimit("/runs");
  if (opts?.includeRecordingMeta) path += "&include=recording_meta";
  const res = await wfetch(path, { method: "GET" });
  const rows = unwrapList<AgentRun>(await asJson<unknown>(res));
  return paging ? { runs: rows, truncated: res.headers.get("X-Wardyn-Truncated") === "true" } : rows;
}

// The Runs landing page's own query surface (#1197 L3), riding L1's opt-in
// GET /runs params (internal/api/runs_list_filter.go's parseRunsListParams):
// view/owner/status/ended_within/include_killed/workspace/q, plus the two
// hidden-count response headers the ageing note reads. A separate function
// from listRuns above rather than a third overload — that one's callers (the
// wizard's preflight reads, the Recordings pager, the CLI/SDK) want the plain
// full-page shape, and folding a five-header-reading return type onto it
// would touch every one of them for a page only this screen renders.
export type RunsListStatus = "active" | "ended" | "failed" | "killed" | "needs";
export type RunsEndedWithin = "24h" | "7d" | "30d" | "all";

export interface RunsListFilter {
  view: "user" | "admin";
  // Omitted lets the server resolve its own default (H-4: "me" for a member
  // or view=user; "all" for an operator's Admin view) — never send "me"/"all"
  // speculatively, since that would short-circuit the server's own fail-closed
  // owner force (runs_list_filter.go's parseRunsListParams).
  owner?: "me" | "all";
  status?: RunsListStatus;
  endedWithin?: RunsEndedWithin;
  includeKilled?: boolean;
  workspace?: string;
  q?: string;
  limit?: number;
  offset?: number;
}

export interface RunsListFilteredResult {
  runs: AgentRun[];
  truncated: boolean;
  // The ageing note's own counts (design.md §2.1 AGED) — 0 on the status=needs
  // branch by construction (server-side; that branch never hides anything).
  hiddenOlder: number;
  hiddenKilled: number;
}

async function listRunsFiltered(filter: RunsListFilter): Promise<RunsListFilteredResult> {
  const q = new URLSearchParams();
  q.set("view", filter.view);
  if (filter.owner) q.set("owner", filter.owner);
  if (filter.status) q.set("status", filter.status);
  if (filter.endedWithin) q.set("ended_within", filter.endedWithin);
  if (filter.includeKilled) q.set("include_killed", "1");
  if (filter.workspace) q.set("workspace", filter.workspace);
  if (filter.q) q.set("q", filter.q);
  q.set("limit", String(filter.limit ?? LIST_LIMIT));
  q.set("offset", String(filter.offset ?? 0));
  const res = await wfetch(`/runs?${q.toString()}`, { method: "GET" });
  const rows = unwrapList<AgentRun>(await asJson<unknown>(res));
  return {
    runs: rows,
    truncated: res.headers.get("X-Wardyn-Truncated") === "true",
    hiddenOlder: Number(res.headers.get("X-Wardyn-Hidden-Older") ?? "0") || 0,
    hiddenKilled: Number(res.headers.get("X-Wardyn-Hidden-Killed") ?? "0") || 0,
  };
}

export const runs = {
  // GET /api/v1/runs
  //
  // includeRecordingMeta asks the server to also project has_recording /
  // recording_bytes / recording_duration_sec onto every run (R4-F077) —
  // opt-in (?include=recording_meta) because it costs the server one
  // RecordingStore.StatAndTail call PER RUN in the response, and this
  // endpoint also backs the Runs board's POLL_MS=3000 poll at limit=1000,
  // which renders none of those three fields. Only the Recordings screen,
  // which actually shows them, should pass true.
  //
  // limit/offset (#159): explicit server-side paging — see the PagedRuns
  // overload above.
  listRuns,

  // GET /api/v1/runs — the Runs landing page's own filtered read (#1197 L3).
  // See RunsListFilter's own doc.
  listRunsFiltered,

  // GET /api/v1/runs/{id} — the ONE endpoint that sends ui_apps (RunDetail).
  async getRun(id: string): Promise<RunDetail | undefined> {
    const res = await wfetch(`/runs/${encodeURIComponent(id)}`, { method: "GET" });
    if (res.status === 404) return undefined;
    return asJson<RunDetail>(res);
  },

  // POST /api/v1/runs/{id}/attach/ticket (renamed from attach-ticket in 0.8 —
  // docs/sdk.md "Renamed in 0.8"; the old path still answers, as a chi alias,
  // for one minor) — mint a single-use, short-TTL ticket the attach WebSocket
  // accepts as ?ticket= (browsers cannot put the admin bearer on a WS
  // handshake). Minted through this NORMAL authenticated call; consumed on
  // first WS connect, so each (re)connect mints a fresh one.
  async attachTicket(runId: string): Promise<string> {
    const res = await wfetch(`/runs/${encodeURIComponent(runId)}/attach/ticket`, {
      method: "POST",
    });
    const body = await asJson<{ ticket: string }>(res);
    return body.ticket;
  },

  // New Run's three requests. Each sends the body intentToWire built, byte for byte, so
  // preflight, policy preview and create judge the same request.
  //
  // POST /api/v1/runs. The response is the created run's fields PLUS an optional advisory
  // `warnings: string[]` (e.g. a workspace-directory collision with another active run): the run
  // still launched, and callers surface warnings without blocking. CreateRunResult is
  // structurally an AgentRun. #125/#121: the default deadline, not LAUNCH_DEADLINE_MS, because
  // POST /runs dispatches asynchronously and no longer blocks through CreateSandbox.
  async createRunWire(body: object): Promise<CreateRunResult> {
    const res = await wfetch("/runs", { method: "POST", body: JSON.stringify(body) });
    return asJson<CreateRunResult>(res);
  },
  // Preflight (POST /runs/preflight) is a dry run of create's resolution and gating. It keeps
  // LAUNCH_DEADLINE_MS because that resolution (mounts, grants, the blast-radius raise) is synchronous.
  async preflightRunWire(body: object, signal?: AbortSignal): Promise<PreflightResult> {
    const res = await wfetch("/runs/preflight", { method: "POST", body: JSON.stringify(body), signal }, LAUNCH_DEADLINE_MS);
    return asJson<PreflightResult>(res);
  },
  async previewRunPolicyWire(body: object, signal?: AbortSignal): Promise<PolicyPreviewResult> {
    const res = await wfetch("/runs/policy-preview", { method: "POST", body: JSON.stringify(body), signal });
    try {
      return await asJson<PolicyPreviewResult>(res);
    } catch (e) {
      if (e instanceof HttpError) e.retryAfter = res.headers.get("Retry-After") ?? undefined;
      throw e;
    }
  },

  // POST /api/v1/policies/grade — composer.Grade of a bare spec, no run
  // attached: the policy panel's live safety meter. Unlike preflight (which
  // dry-runs a RESOLVED run — mounts, grants, blast-radius raise), this grades
  // the DOCUMENT exactly as typed. `interactive` is an optional hint (default
  // false, the conservative frame): it only changes the never-reap item's
  // rationale — the run wizard sends the screen's mode, /policies sends nothing.
  async gradePolicy(spec: RunPolicySpec, interactive?: boolean): Promise<PolicyGrade> {
    const body: Record<string, unknown> = { spec };
    if (interactive) body.interactive = true;
    const res = await wfetch("/policies/grade", { method: "POST", body: JSON.stringify(body) });
    return asJson<PolicyGrade>(res);
  },

  // POST /api/v1/runs/{id}/profile/synthesize (renamed from /profile in 0.8 —
  // docs/sdk.md "Renamed in 0.8"; the old path still answers, as a chi alias,
  // for one minor) — Recording-Mode profile synthesis (ADVISORY, read-only).
  // Replays the run's observed behaviour into a PROPOSED least-privilege run
  // + inline_policy plus the raw observations + Wardyn's deterministic risk
  // assessment. Never creates a run or mints a credential.
  async profileRun(id: string): Promise<ProfileProposal> {
    const res = await wfetch(`/runs/${encodeURIComponent(id)}/profile/synthesize`, { method: "POST" });
    return asJson<ProfileProposal>(res);
  },

  // PATCH /api/v1/runs/{id}/title — OWNER ONLY, no admin bypass, any run
  // state (#1197 L2). Deliberately its own call rather than folded into
  // setEndAndWait: the two PATCH doors have different authorization tiers
  // (this one owner only, that one owner-or-SUPER) and different state rules
  // (this one works on a terminal run, that one refuses).
  async setTitle(id: string, title: string): Promise<{ id: string; title: string }> {
    const res = await wfetch(`/runs/${encodeURIComponent(id)}/title`, {
      method: "PATCH",
      body: JSON.stringify({ title }),
    });
    if (!res.ok) throw new HttpError(res.status, await errText(res));
    return asJson<{ id: string; title: string }>(res);
  },

  // POST /api/v1/runs/{id}/kill  -> 202 Accepted
  async killRun(id: string): Promise<void> {
    const res = await wfetch(`/runs/${encodeURIComponent(id)}/kill`, { method: "POST" });
    if (!res.ok) {
      throw new HttpError(res.status, await errText(res));
    }
  },

  // PATCH /api/v1/runs/{id} — the run's end and/or wait (long-holds design
  // rev 4 §2.3, RL-4/RL-15): owner or super admin. `endsAt: null` asks for No
  // end; omitting a field leaves it alone. An over-ask is never refused — the
  // server caps it to the run's captured limit and names the field in the
  // response's `capped`, which the caller renders rather than treating as an
  // error.
  async setRunEndAndWait(
    id: string,
    change: { endsAt?: string | null; waitBudgetSec?: number },
  ): Promise<RunEndWaitResult> {
    const body: Record<string, unknown> = {};
    if ("endsAt" in change) body.ends_at = change.endsAt;
    if (change.waitBudgetSec != null) body.wait_budget_sec = change.waitBudgetSec;
    const res = await wfetch(`/runs/${encodeURIComponent(id)}`, { method: "PATCH", body: JSON.stringify(body) });
    if (!res.ok) throw new HttpError(res.status, await errText(res));
    return asJson<RunEndWaitResult>(res);
  },

  // POST /api/v1/runs/{id}/revive — owner or super admin (long-holds design
  // rev 4 §4.1, RL-10/RL-11). Gives a lost or live run a fresh proxy built
  // from the run's own stored config, with the owner's CURRENT profile denies
  // unioned over the frozen policy; a rebooted/outaged run's agent is started
  // again behind it. Authority is always the OWNER's, never the caller's.
  async reviveRun(id: string): Promise<ReviveResult> {
    const res = await wfetch(`/runs/${encodeURIComponent(id)}/revive`, { method: "POST" });
    if (!res.ok) throw new HttpError(res.status, await errText(res));
    return asJson<ReviveResult>(res);
  },

  // POST /api/v1/runs/{id}/resume — owner or super admin: thaw a run the
  // pause reaper froze (long-holds design rev 4 §3, RL-7). 409s if the run
  // isn't RUNNING or was never paused; the caller re-fetches either way.
  async resumeRun(id: string): Promise<void> {
    const res = await wfetch(`/runs/${encodeURIComponent(id)}/resume`, { method: "POST" });
    if (!res.ok) throw new HttpError(res.status, await errText(res));
  },

  // GET /api/v1/runs/{id}/grants — the run's credential-grant eligibility
  // records (what it MAY request), including grants that were never minted.
  async getGrants(runId: string): Promise<CredentialGrant[]> {
    const res = await wfetch(`/runs/${encodeURIComponent(runId)}/grants`, { method: "GET" });
    if (res.status === 404) return [];
    return grantsFromRecords(await asJson<unknown>(res));
  },

  // GET /api/v1/runs/{id}/files — the per-file diff stat of the run's
  // workspace, read by running git INSIDE the sandbox.
  //
  // The non-200s are all MEANINGFUL and are deliberately left as thrown
  // HttpErrors (the killRun pattern) rather than flattened into a null: the
  // widget renders a different sentence for each, and a caller that swallowed
  // them would render "no files changed" for a run it never managed to read.
  //   409 — the run has no sandbox yet (nothing to read)
  //   501 — this runner has no exec primitive, so it cannot be inspected
  // A workspace that simply isn't a git repo is NOT an error: it comes back
  // 200 with vcs:"none".
  async getFiles(runId: string): Promise<RunFilesResult> {
    const res = await wfetch(`/runs/${encodeURIComponent(runId)}/files`, { method: "GET" });
    if (!res.ok) throw new HttpError(res.status, await errText(res));
    return asJson<RunFilesResult>(res);
  },

  // GET /api/v1/runs/{id}/policy — the policy the run got, where it started
  // from and what launch changed. The tab renders every non-200 as one error
  // state, so they are left as thrown HttpErrors.
  async getPolicy(runId: string): Promise<RunPolicyView> {
    const res = await wfetch(`/runs/${encodeURIComponent(runId)}/policy`, { method: "GET" });
    if (!res.ok) throw new HttpError(res.status, await errText(res));
    return asJson<RunPolicyView>(res);
  },

  // GET /api/v1/runs/{id}/resources — CPU/memory/disk/process counts read from
  // cgroup v2 + procfs inside the sandbox. Every field is optional; see
  // RunResources for why absent must never be rendered as zero.
  async getResources(runId: string): Promise<RunResources> {
    const res = await wfetch(`/runs/${encodeURIComponent(runId)}/resources`, { method: "GET" });
    if (!res.ok) throw new HttpError(res.status, await errText(res));
    return asJson<RunResources>(res);
  },

  // GET /api/v1/runs/{id}/attach/holder (renamed from attach-holder in 0.8 —
  // docs/sdk.md "Renamed in 0.8"; the old path still answers, as a chi alias,
  // for one minor) — who currently holds the run's shared tmux PTY.
  // `held:false` means "nobody is attached through THIS daemon"; the registry
  // is in-process (see internal/api/attach_holder.go), so the UI must not
  // phrase it as a stronger claim than that.
  async getAttachHolder(runId: string): Promise<AttachHolder> {
    const res = await wfetch(`/runs/${encodeURIComponent(runId)}/attach/holder`, { method: "GET" });
    if (!res.ok) throw new HttpError(res.status, await errText(res));
    return asJson<AttachHolder>(res);
  },

  // POST /api/v1/runs/{id}/attach/takeover — displace the current holder and
  // become the writer. Audited server-side (session.takeover, naming the actor
  // and the previous holder) because it ends someone else's live session.
  // `promoted` is true when one of the caller's own queued observer sockets on
  // this run was flipped to writer IN PLACE (attach_holder.go's
  // announceAttachPromotion) — a caller whose socket is still open must not
  // evict-then-reconnect on that answer; see doTakeover in attach-terminal.tsx.
  async takeoverAttach(runId: string): Promise<{ promoted: boolean }> {
    const res = await wfetch(`/runs/${encodeURIComponent(runId)}/attach/takeover`, { method: "POST" });
    // asJson, not a bare HttpError: it carries the envelope's reason, which the
    // console maps (run_owner_only) instead of showing the wire sentence.
    return asJson<{ promoted: boolean }>(res);
  },

  // GET /api/v1/admin/runs/proxy-window — operator only (F2/#580, PR #1317
  // review, RL-10). Every live run whose proxy started outside the supported
  // wardynd N/N-1 window; these are exactly the ids the bulk restart below
  // takes.
  async getAdminProxyWindow(): Promise<AdminProxyWindowResult> {
    const res = await wfetch("/admin/runs/proxy-window", { method: "GET" });
    if (!res.ok) throw new HttpError(res.status, await errText(res));
    return asJson<AdminProxyWindowResult>(res);
  },

  // GET /api/v1/admin/runs/capacity — security operator only. The fleet's
  // configured reservations (not measurements), computed on request.
  async getAdminRunCapacity(): Promise<RunCapacityResponse> {
    const res = await wfetch("/admin/runs/capacity", { method: "GET" });
    if (!res.ok) throw new HttpError(res.status, await errText(res));
    return asJson<RunCapacityResponse>(res);
  },

  // POST /api/v1/admin/runs/restart { run_ids } — operator only. Restarts
  // each named run's proxy under its OWNER's current limits, one at a time;
  // a per-run failure (including a rebooted run's own "revive it from the
  // run's page" 409) never aborts the rest — see AdminRestartResult.
  // timeoutMs is the caller's deadline for the whole batch: the default 60s bounds a hang, not a
  // restart of several runs one after another.
  async restartAdminRuns(runIds: string[], timeoutMs?: number): Promise<AdminRestartResponse> {
    const res = await wfetch("/admin/runs/restart", { method: "POST", body: JSON.stringify({ run_ids: runIds }) }, timeoutMs);
    if (!res.ok) throw new HttpError(res.status, await errText(res));
    return asJson<AdminRestartResponse>(res);
  },
};
