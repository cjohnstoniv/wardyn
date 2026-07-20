/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Run lifecycle: list/get/create/preflight/profile/kill + attach ticket +
// credential-grant eligibility. Consumed directly (import { runs }) so a route
// that never touches runs drops this module from its chunk.
import type {
  AgentRun,
  CreateRunInput,
  CreateRunResult,
  CredentialGrant,
  PreflightResult,
  ProfileProposal,
  RunPolicySpec,
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
  async createRun(
    input: (Partial<AgentRun> | CreateRunInput) & {
      interactive?: boolean;
      inline_policy?: RunPolicySpec;
      // The compose session this run was launched from (see ComposeRequest.sessionId).
      // Threaded through so `run.create`'s audit row can be correlated back to the
      // compose conversation that produced it — absent for a manually-wizarded run.
      compose_session_id?: string;
    },
  ): Promise<CreateRunResult> {
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
    if (input.compose_session_id) body.compose_session_id = input.compose_session_id;
    // BYOI + governed-command pass-through — previously dropped on the floor here.
    if (input.image) body.image = input.image;
    if ("task_mode" in input && input.task_mode) body.task_mode = input.task_mode;
    const res = await wfetch("/runs", { method: "POST", body: JSON.stringify(body) });
    return asJson<CreateRunResult>(res);
  },

  // POST /api/v1/runs/preflight — a DRY-RUN of createRun's resolution + gating:
  // mints/persists/dispatches NOTHING, just returns the deterministic setup
  // checklist and the enforced confinement class (post floor + blast-radius
  // raise). The wizard fires this when the operator enters Review, sending the
  // SAME body createRun would, so the checklist and any 4xx (unknown-secret 422,
  // XOR, invalid spec) are the real launch verdicts. Advisory: callers render an
  // error as a quiet "preflight unavailable" and never block Review.
  async preflightRun(
    input: (Partial<AgentRun> | CreateRunInput) & {
      interactive?: boolean;
      inline_policy?: RunPolicySpec;
    },
  ): Promise<PreflightResult> {
    const body: Record<string, unknown> = {
      agent: input.agent,
      repo: input.repo,
      task: input.task,
    };
    if (input.policy_id) body.policy_id = input.policy_id;
    let cc = input.confinement_class;
    const floor = input.inline_policy?.min_confinement_class;
    if (cc && floor && ccRank(cc) < ccRank(floor)) cc = floor;
    if (cc) body.confinement_class = cc;
    if (input.interactive) body.interactive = true;
    if (input.inline_policy) body.inline_policy = input.inline_policy;
    // Mirror createRun's body exactly, so preflight's verdict matches the real launch.
    if (input.image) body.image = input.image;
    if ("task_mode" in input && input.task_mode) body.task_mode = input.task_mode;
    const res = await wfetch("/runs/preflight", { method: "POST", body: JSON.stringify(body) });
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
};
