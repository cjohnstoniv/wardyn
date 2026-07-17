/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Onboarded workspaces + the guided Import flow (scan/build/verify/record/
// finalize). Run-creation pickers offer ONLY these; a run may not reference any
// other source.
import type { SetupCommand, Workspace, WorkspaceKind } from "../types";
import { asJson, errText, HttpError, unwrapList, wfetch, withLimit } from "./core";

export const workspaces = {
  // GET /api/v1/workspaces — onboarded local dirs + repos (admin-gated). Run-
  // creation pickers offer ONLY these; a run may not reference any other source.
  async listWorkspaces(): Promise<Workspace[]> {
    const res = await wfetch(withLimit("/workspaces"), { method: "GET" });
    return unwrapList<Workspace>(await asJson<unknown>(res));
  },

  // POST /api/v1/workspaces  { name, kind, source, ref?, default_target? } ->
  // 201 created workspace (status starts "pending_scan" until scanned).
  async createWorkspace(input: {
    name: string;
    kind: WorkspaceKind;
    source: string;
    ref?: string;
    default_target?: string;
    writable?: boolean;
  }): Promise<Workspace> {
    const res = await wfetch("/workspaces", { method: "POST", body: JSON.stringify(input) });
    return asJson<Workspace>(res);
  },

  // PUT /api/v1/workspaces/{id}  same body shape -> updated workspace.
  async updateWorkspace(
    id: string,
    input: {
      name: string;
      kind: WorkspaceKind;
      source: string;
      ref?: string;
      default_target?: string;
      writable?: boolean;
    },
  ): Promise<Workspace> {
    const res = await wfetch(`/workspaces/${encodeURIComponent(id)}`, {
      method: "PUT",
      body: JSON.stringify(input),
    });
    return asJson<Workspace>(res);
  },

  // PUT /api/v1/workspaces/{id}/approved-egress  { domains } -> the updated
  // workspace (same shape as GET /workspaces/{id}). FULL replacement + idempotent:
  // send the WHOLE desired allowlist, not a delta. These operator-approved hosts
  // are unioned into a run's egress allowlist at launch (like the profile's
  // egress_domains). The needs panel calls this to approve/remove a host.
  async setApprovedEgress(id: string, domains: string[]): Promise<Workspace> {
    const res = await wfetch(`/workspaces/${encodeURIComponent(id)}/approved-egress`, {
      method: "PUT",
      body: JSON.stringify({ domains }),
    });
    return asJson<Workspace>(res);
  },

  // GET /api/v1/workspaces/{id}/observed-egress -> { denied, runs_examined }.
  // Egress hosts that runs USING this workspace were DENIED — least-privilege
  // promotion candidates the needs panel offers one-click approval for. 404
  // (older backend / no run history) degrades to an empty result, like getGrants.
  async getObservedEgress(id: string): Promise<{ denied: string[]; runs_examined: number }> {
    const res = await wfetch(`/workspaces/${encodeURIComponent(id)}/observed-egress`, { method: "GET" });
    if (res.status === 404) return { denied: [], runs_examined: 0 };
    return asJson<{ denied: string[]; runs_examined: number }>(res);
  },

  // GET /api/v1/workspaces/{id} -> the single onboarded workspace, or undefined on
  // 404. The import panel polls this to watch one workspace's status advance
  // (scanning → building → verifying → ready) without re-listing every workspace.
  async getWorkspace(id: string): Promise<Workspace | undefined> {
    const res = await wfetch(`/workspaces/${encodeURIComponent(id)}`, { method: "GET" });
    if (res.status === 404) return undefined;
    return asJson<Workspace>(res);
  },

  // PUT /api/v1/workspaces/{id}/setup-commands { commands } -> updated Workspace.
  // The operator-approved list of build/verify commands (FULL replacement) that a
  // verify run will execute. Distinct from the scanner's profile.setup_commands
  // proposal — this is what the operator confirmed.
  async setSetupCommands(id: string, commands: SetupCommand[]): Promise<Workspace> {
    const res = await wfetch(`/workspaces/${encodeURIComponent(id)}/setup-commands`, {
      method: "PUT",
      body: JSON.stringify({ commands }),
    });
    return asJson<Workspace>(res);
  },

  // POST /api/v1/workspaces/{id}/verify. Kicks a governed build+verify run.
  //   202 -> { verify_run_id, workspace_id, state }: started; poll the workspace.
  //   422 -> no approved setup commands yet (approve some in Configure first)
  //   503 -> this control plane has no runner (-runner none) — can't verify, but
  //          the operator can still finalize as configured
  //   409 -> a verify is already running for this workspace
  // The 422/503/409 cases are EXPECTED, actionable states the panel renders inline,
  // so they resolve to { ok:false, status, detail } rather than throw. Any OTHER
  // non-2xx is a real failure and throws HttpError.
  async verifyWorkspace(
    id: string,
  ): Promise<{ ok: boolean; status: number; verify_run_id?: string; state?: string; detail?: string }> {
    const res = await wfetch(`/workspaces/${encodeURIComponent(id)}/verify`, { method: "POST" });
    if (res.status === 202) {
      const body = await asJson<{ verify_run_id?: string; state?: string }>(res);
      return { ok: true, status: 202, verify_run_id: body?.verify_run_id, state: body?.state };
    }
    if (res.status === 422 || res.status === 503 || res.status === 409) {
      return { ok: false, status: res.status, detail: await errText(res) };
    }
    if (!res.ok) throw new HttpError(res.status, await errText(res));
    // A synchronous 200 (some backends may verify inline) — treat as started.
    return { ok: true, status: res.status };
  },

  // POST /api/v1/workspaces/{id}/verify/suggest-fix — the AGENTIC half of the
  // verify-fix loop (ADVISORY). Asks a composer backend to diagnose a FAILED verify
  // from the failing step + already-masked logs + detected profile, and propose the
  // single most likely concrete fix (an egress host to allow, a secret to add by
  // name, or a corrected command). Human-gated: it returns prose the operator
  // applies via the existing endpoints — it never auto-applies anything. An optional
  // backend override picks a non-default composer backend.
  //   200 -> { suggestion }
  //   404 -> composer not enabled here (the panel hides the affordance up front)
  //   422 -> no failed verify result to diagnose yet
  //   400 -> unknown backend / 502 -> backend failed
  // Errors surface as HttpError with .status, like compose().
  async suggestVerifyFix(id: string, backend?: string): Promise<string> {
    const qs = backend ? `?backend=${encodeURIComponent(backend)}` : "";
    const res = await wfetch(`/workspaces/${encodeURIComponent(id)}/verify/suggest-fix${qs}`, {
      method: "POST",
    });
    return (await asJson<{ suggestion?: string }>(res)).suggestion ?? "";
  },

  // POST /api/v1/workspaces/{id}/record { name, confined }. Kicks an OPEN
  // (allow-all-egress) recording sandbox for one task under the strongest available
  // confinement.
  //   202 -> { run_id }: recording started; poll the workspace for the outcome.
  //   422 -> unknown task / no approved commands for an auto task
  //   503 -> this control plane has no runner (can't record)
  //   409 -> another import step (record/verify/…) is already running
  // Mirrors verifyWorkspace exactly: 422/503/409 are EXPECTED, actionable states the
  // pane renders inline as { ok:false, status, detail }; any OTHER non-2xx throws.
  async recordTask(
    id: string,
    name: string,
    confined = false,
  ): Promise<{ ok: boolean; status: number; record_run_id?: string; detail?: string }> {
    // Named interactive session (the server slugs `name` → the record_results key).
    // The operator drives the real activity in the attach shell. `confined` picks a
    // VERIFY session (default-deny egress, limited to the approved set) over an open
    // learning session — off-policy hosts are denied live in the confined case.
    const res = await wfetch(`/workspaces/${encodeURIComponent(id)}/record`, {
      method: "POST",
      body: JSON.stringify({ name, confined }),
    });
    if (res.status === 202) {
      const body = await asJson<{ record_run_id?: string }>(res);
      return { ok: true, status: 202, record_run_id: body?.record_run_id };
    }
    if (res.status === 422 || res.status === 503 || res.status === 409) {
      return { ok: false, status: res.status, detail: await errText(res) };
    }
    if (!res.ok) throw new HttpError(res.status, await errText(res));
    return { ok: true, status: res.status };
  },

  // POST /api/v1/workspaces/{id}/record/{task}/promote-egress -> the updated
  // Workspace (record_results[task].egress_promoted flips true + approved_egress
  // widened server-side). 404-tolerant: on a build whose backend hasn't shipped the
  // endpoint, fall back to the existing approved-egress PUT with the caller-computed
  // desired allowlist (full approved ∪ observed) — same end state, one round-trip.
  async promoteRecordEgress(id: string, task: string, fallbackDomains: string[]): Promise<Workspace> {
    const res = await wfetch(
      `/workspaces/${encodeURIComponent(id)}/record/${encodeURIComponent(task)}/promote-egress`,
      { method: "POST" },
    );
    if (res.status === 404) return workspaces.setApprovedEgress(id, fallbackDomains);
    return asJson<Workspace>(res);
  },

  // POST /api/v1/workspaces/{id}/finalize { emit_env_as_code } ->
  // { workspace, emitted_files }. emitted_files maps filename -> content
  // (devcontainer.json / AGENTS.md / …) when emit_env_as_code is set; empty otherwise.
  async finalizeWorkspace(
    id: string,
    opts: { emitEnvAsCode: boolean },
  ): Promise<{ workspace: Workspace; emitted_files: Record<string, string> }> {
    const res = await wfetch(`/workspaces/${encodeURIComponent(id)}/finalize`, {
      method: "POST",
      body: JSON.stringify({ emit_env_as_code: opts.emitEnvAsCode }),
    });
    const body = await asJson<{ workspace: Workspace; emitted_files?: Record<string, string> }>(res);
    return { workspace: body.workspace, emitted_files: body.emitted_files ?? {} };
  },

  // DELETE /api/v1/workspaces/{id} -> 204.
  async deleteWorkspace(id: string): Promise<void> {
    const res = await wfetch(`/workspaces/${encodeURIComponent(id)}`, { method: "DELETE" });
    if (!res.ok && res.status !== 404) {
      throw new HttpError(res.status, await errText(res));
    }
  },

  // POST /api/v1/workspaces/{id}/scan — kick off a (re-)scan. The response body
  // shape DIFFERS by kind and is NOT a Workspace: a local dir scans INLINE (200,
  // body is the derived profile, status already flipped to ready/error), while a
  // repo launches a governed scan run (202 { scan_run_id, … } — the profile/status
  // update asynchronously when it finishes). So callers must NOT treat the body as a
  // Workspace; re-fetch the list for the authoritative status. Returns only the
  // async signal + the scan-run id (repo) so the UI can message accordingly.
  async scanWorkspace(id: string): Promise<{ async: boolean; scanRunId?: string }> {
    const res = await wfetch(`/workspaces/${encodeURIComponent(id)}/scan`, { method: "POST" });
    const body = await asJson<{ scan_run_id?: string }>(res);
    return { async: res.status === 202, scanRunId: body?.scan_run_id };
  },
};
