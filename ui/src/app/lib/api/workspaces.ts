/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Onboarded workspaces + the guided Import flow (scan/record). Verify/finalize
// are retired from the interim import panel (see import-panel.tsx); env-as-code
// generation stays available standalone via getEnvAsCode below. Run-creation
// pickers offer ONLY these onboarded workspaces; a run may not reference any
// other source.
import type { Workspace, WorkspaceKind, WorkspaceLLMCred } from "../types";
import { asJson, errText, HttpError, unwrapList, wfetch, withLimit } from "./core";

export const workspaces = {
  // GET /api/v1/workspaces — onboarded local dirs + repos (admin-gated). Run-
  // creation pickers offer ONLY these; a run may not reference any other source.
  async listWorkspaces(): Promise<Workspace[]> {
    const res = await wfetch(withLimit("/workspaces"), { method: "GET" });
    return unwrapList<Workspace>(await asJson<unknown>(res));
  },

  // POST /api/v1/workspaces  { name, kind, source, ref?, default_target?, llm_cred? } ->
  // 201 created workspace (status starts "pending_scan" until scanned).
  // llm_cred is the ONLY way to set a model/harness binding at create time —
  // editing an existing workspace's binding goes through setWorkspaceLLMCred
  // instead (updateWorkspace below ignores it, mirroring the server).
  async createWorkspace(input: {
    name: string;
    kind: WorkspaceKind;
    source: string;
    ref?: string;
    default_target?: string;
    writable?: boolean;
    llm_cred?: WorkspaceLLMCred;
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
  // (scanning → scanned; building/verifying/ready are legacy-only now) without
  // re-listing every workspace.
  async getWorkspace(id: string): Promise<Workspace | undefined> {
    const res = await wfetch(`/workspaces/${encodeURIComponent(id)}`, { method: "GET" });
    if (res.status === 404) return undefined;
    return asJson<Workspace>(res);
  },

  // PUT /api/v1/workspaces/{id}/llm-cred  body = the WorkspaceLLMCred -> updated
  // Workspace. Sets/clears the operator-owned model/harness credential binding
  // for an EXISTING workspace/container (mode:"" clears it). A run that later
  // picks this workspace/container inherits it, injected proxy-side at
  // dispatch — never resident. The one way to change the binding post-create;
  // updateWorkspace's generic PUT does not touch it.
  async setWorkspaceLLMCred(id: string, cred: WorkspaceLLMCred): Promise<Workspace> {
    const res = await wfetch(`/workspaces/${encodeURIComponent(id)}/llm-cred`, {
      method: "PUT",
      body: JSON.stringify(cred),
    });
    return asJson<Workspace>(res);
  },

  // POST /api/v1/workspaces/{id}/record { name, confined }. Kicks an OPEN
  // (allow-all-egress) recording sandbox for one task under the strongest available
  // confinement.
  //   202 -> { run_id }: recording started; poll the workspace for the outcome.
  //   422 -> unknown task / no approved commands for an auto task
  //   503 -> this control plane has no runner (can't record)
  //   409 -> another import step (record/verify/…) is already running
  // Same 422/503/409-inline pattern as this file's other governed-run kickoffs:
  // EXPECTED, actionable states the pane renders inline as { ok:false, status,
  // detail }; any OTHER non-2xx throws.
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

  // GET /api/v1/workspaces/{id}/env-as-code -> { emitted_files } (same key as
  // finalize) — regenerates the committable files any time, so a repo
  // workspace's env-as-code does not die with the one-shot finalize response.
  // 422 (no scanned profile yet) surfaces via HttpError with the server's text.
  async getEnvAsCode(id: string): Promise<Record<string, string>> {
    const res = await wfetch(`/workspaces/${encodeURIComponent(id)}/env-as-code`);
    const body = await asJson<{ emitted_files?: Record<string, string> }>(res);
    return body.emitted_files ?? {};
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
