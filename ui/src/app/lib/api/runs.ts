/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Run lifecycle: list/get/create/preflight/profile/kill + attach ticket +
// credential-grant eligibility. Consumed directly (import { runs }) so a route
// that never touches runs drops this module from its chunk.
import type {
  AgentRun,
  AttachHolder,
  CreateRunInput,
  CreateRunResult,
  CredentialGrant,
  PreflightResult,
  ProfileProposal,
  RunFilesResult,
  RunPolicySpec,
  RunResources,
} from "../types";
import { asJson, ccRank, errText, HttpError, str, unwrapList, wfetch, withLimit } from "./core";

// Map a backend credential-grant eligibility record (the GET /runs/{id}/grants
// shape: { id, run_id, created_at, spec: { kind, scope, ttl_seconds,
// requires_approval } }) into the CredentialGrant shape the run-detail screen
// renders. These are ELIGIBILITY records (what the run may request), not issued
// credentials, so there is no jti/expiry; they render as "active" eligibility.
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
    } satisfies CredentialGrant;
  });
}

type RunWireInput = (Partial<AgentRun> | CreateRunInput) & {
  interactive?: boolean;
  inline_policy?: RunPolicySpec;
  // Per-run half of the requirements contract: which optional requirements
  // this run enables, plus any read-only narrowing, per attached workspace.
  workspaces?: { workspace_id: string; enabled_optional?: string[]; read_only?: boolean }[];
  // Primary-workspace id for a selection that resolves to no mount/repo (a
  // pure-ephemeral / migrated-0029 workspace) — routes its base_image through
  // the server's seedRequestWorkspace, which the mount-less spec can't.
  workspace_id?: string;
  // Explicit model-access override — tier 1 of the server's resolution chain.
  integration_id?: string;
};

// The ONE projection from wizard input to the POST /runs wire body. createRun
// and preflightRun both send exactly this — a field added here reaches both, a
// field missed here reaches neither, and the two verdicts can never drift.
// (They used to be two hand-built whitelists; preflight's lagged by five fields.)
function runWireBody(input: RunWireInput): Record<string, unknown> {
  const body: Record<string, unknown> = {
    agent: input.agent,
    repo: input.repo,
    task: input.task,
  };
  if (input.policy_id) body.policy_id = input.policy_id;
  // A run may request an equal-or-STRONGER tier than its policy floor, never a
  // weaker one (the server 422s "confinement_class X is weaker than the policy
  // minimum Y"). Defensively raise a requested class UP to the inline policy's
  // floor so a stale/edited selection can never produce that rejection — clamping
  // up only ever strengthens confinement, so it is always safe.
  let cc = input.confinement_class;
  const floor = input.inline_policy?.min_confinement_class;
  if (cc && floor && ccRank(cc) < ccRank(floor)) cc = floor;
  if (cc) body.confinement_class = cc;
  if (input.interactive) body.interactive = true;
  if (input.inline_policy) body.inline_policy = input.inline_policy;
  // BYOI + governed-command pass-through — previously dropped on the floor here.
  if (input.image) body.image = input.image;
  if ("task_mode" in input && input.task_mode) body.task_mode = input.task_mode;
  // The run's name/note and the interactive start choice — same lesson as the
  // two above: this whitelist is hand-built, so an unlisted field is discarded
  // between the form and the wire with no error anywhere. The title the
  // operator typed would simply never exist.
  if ("title" in input && input.title) body.title = input.title;
  if ("description" in input && input.description) body.description = input.description;
  if ("interactive_start" in input && input.interactive_start) {
    body.interactive_start = input.interactive_start;
  }
  // The boot-seed opt-in and the autonomous tool-approval posture — same
  // hand-built-whitelist trap as everything else on this list: an unlisted
  // field is silently discarded between the form and the wire.
  if ("seed_auto_tools" in input && input.seed_auto_tools) body.seed_auto_tools = true;
  if ("tool_approvals" in input && input.tool_approvals) body.tool_approvals = input.tool_approvals;
  // Composition-model pass-through. This whitelist has dropped a wizard field
  // on the floor once before (image/task_mode, above) — a selection the
  // operator made, silently discarded between the form and the wire. These
  // two carry the per-run half of the requirements contract (which optional
  // requirements this run enables, and any read-only narrowing) and the
  // explicit model-access override, so dropping them would launch a run the
  // Review screen did not describe.
  if (input.workspaces?.length) body.workspaces = input.workspaces;
  if (input.workspace_id) body.workspace_id = input.workspace_id;
  if (input.integration_id) body.integration_id = input.integration_id;
  return body;
}

export const runs = {
  // GET /api/v1/runs
  async listRuns(): Promise<AgentRun[]> {
    const res = await wfetch(withLimit("/runs"), { method: "GET" });
    return unwrapList<AgentRun>(await asJson<unknown>(res));
  },

  // GET /api/v1/runs/{id}
  async getRun(id: string): Promise<AgentRun | undefined> {
    const res = await wfetch(`/runs/${encodeURIComponent(id)}`, { method: "GET" });
    if (res.status === 404) return undefined;
    return asJson<AgentRun>(res);
  },

  // POST /api/v1/runs/{id}/attach-ticket — mint a single-use, short-TTL ticket
  // the attach WebSocket accepts as ?ticket= (browsers cannot put the admin
  // bearer on a WS handshake). Minted through this NORMAL authenticated call;
  // consumed on first WS connect, so each (re)connect mints a fresh one.
  async attachTicket(runId: string): Promise<string> {
    const res = await wfetch(`/runs/${encodeURIComponent(runId)}/attach-ticket`, {
      method: "POST",
    });
    const body = await asJson<{ ticket: string }>(res);
    return body.ticket;
  },

  // POST /api/v1/runs  { agent, repo, task, policy_id?, confinement_class?,
  //   interactive?, inline_policy? }
  // interactive=true brings the sandbox up idle (no agent task) so a human can
  // attach to it; pair with a never-reap policy (auto_stop_after_sec < 0).
  // inline_policy carries a RunPolicySpec inline; it is MUTUALLY EXCLUSIVE with
  // policy_id (XOR) — the wizard sends exactly one, never both. Neither set =>
  // the configured default policy (unchanged behavior).
  // The response is the created run's fields PLUS an optional advisory
  // `warnings: string[]` (e.g. a workspace-directory collision with another
  // active run) — the run still launched; callers surface warnings without
  // blocking. CreateRunResult is structurally an AgentRun, so existing onCreated
  // callbacks keep working.
  async createRun(input: RunWireInput): Promise<CreateRunResult> {
    const res = await wfetch("/runs", { method: "POST", body: JSON.stringify(runWireBody(input)) });
    return asJson<CreateRunResult>(res);
  },

  // POST /api/v1/runs/preflight — a DRY-RUN of createRun's resolution + gating:
  // mints/persists/dispatches NOTHING, just returns the deterministic setup
  // checklist and the enforced confinement class (post floor + blast-radius
  // raise). The wizard fires this when the operator enters Review, sending the
  // SAME body createRun would, so the checklist and any 4xx (unknown-secret 422,
  // XOR, invalid spec) are the real launch verdicts. Advisory: callers render an
  // error as a quiet "preflight unavailable" and never block Review.
  async preflightRun(input: RunWireInput): Promise<PreflightResult> {
    const res = await wfetch("/runs/preflight", {
      method: "POST",
      body: JSON.stringify(runWireBody(input)),
    });
    return asJson<PreflightResult>(res);
  },

  // POST /api/v1/runs/{id}/profile — Recording-Mode profile synthesis (ADVISORY,
  // read-only). Replays the run's observed behaviour into a PROPOSED least-
  // privilege run + inline_policy plus the raw observations + Wardyn's
  // deterministic risk assessment. Never creates a run or mints a credential.
  async profileRun(id: string): Promise<ProfileProposal> {
    const res = await wfetch(`/runs/${encodeURIComponent(id)}/profile`, { method: "POST" });
    return asJson<ProfileProposal>(res);
  },

  // POST /api/v1/runs/{id}/kill  -> 202 Accepted
  async killRun(id: string): Promise<void> {
    const res = await wfetch(`/runs/${encodeURIComponent(id)}/kill`, { method: "POST" });
    if (!res.ok) {
      throw new HttpError(res.status, await errText(res));
    }
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

  // GET /api/v1/runs/{id}/resources — CPU/memory/disk/process counts read from
  // cgroup v2 + procfs inside the sandbox. Every field is optional; see
  // RunResources for why absent must never be rendered as zero.
  async getResources(runId: string): Promise<RunResources> {
    const res = await wfetch(`/runs/${encodeURIComponent(runId)}/resources`, { method: "GET" });
    if (!res.ok) throw new HttpError(res.status, await errText(res));
    return asJson<RunResources>(res);
  },

  // GET /api/v1/runs/{id}/attach-holder — who currently holds the run's shared
  // tmux PTY. `held:false` means "nobody is attached through THIS daemon";
  // the registry is in-process (see internal/api/attach_holder.go), so the UI
  // must not phrase it as a stronger claim than that.
  async getAttachHolder(runId: string): Promise<AttachHolder> {
    const res = await wfetch(`/runs/${encodeURIComponent(runId)}/attach-holder`, { method: "GET" });
    if (!res.ok) throw new HttpError(res.status, await errText(res));
    return asJson<AttachHolder>(res);
  },

  // POST /api/v1/runs/{id}/attach/takeover — displace the current holder and
  // become the writer. Audited server-side (session.takeover, naming the actor
  // and the previous holder) because it ends someone else's live session.
  async takeoverAttach(runId: string): Promise<void> {
    const res = await wfetch(`/runs/${encodeURIComponent(runId)}/attach/takeover`, { method: "POST" });
    if (!res.ok) throw new HttpError(res.status, await errText(res));
  },
};
