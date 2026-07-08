/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Real Wardyn API client — same-origin fetch() against /api/v1.
// Method names/signatures match exactly what the screens already call.
import type {
  AgentRun,
  ApprovalRequest,
  AuditEvent,
  AsciicastEvent,
  ComposeAssistRequest,
  ComposeAssistResponse,
  ComposeRequest,
  ComposeResult,
  ComposeWorkspace,
  ComposerBackend,
  CredentialGrant,
  CreateRunInput,
  CreateRunResult,
  EgressDecision,
  ProfileProposal,
  Recording,
  RunPolicy,
  RunPolicySpec,
  SecretName,
  SetupCommand,
  SetupStatus,
  SiteConfig,
  VerifyFixSuggestion,
  Workspace,
  WorkspaceKind,
  WorkspaceSelection,
} from "./types";
import { lsGet, lsSet } from "./storage";

const BASE = "/api/v1";
const TOKEN_KEY = "wardyn_admin_token";

// ------------------------------------------------------------
// Auth token + 401 handling
// ------------------------------------------------------------
let _unauthorized: (() => void) | null = null;

export function getToken(): string | null {
  return lsGet(TOKEN_KEY);
}

export function setToken(token: string | null): void {
  lsSet(TOKEN_KEY, token || null);
}

export function onUnauthorized(fn: () => void): void {
  _unauthorized = fn;
}

export class HttpError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
    this.name = "HttpError";
  }
}

// Confinement tiers weakest→strongest, for clamping a run's requested class up to
// its policy floor before create (a weaker request 422s server-side).
const ccRank = (cc: string): number =>
  ({ CC1: 1, CC2: 2, CC3: 3 })[cc as "CC1" | "CC2" | "CC3"] ?? 0;

// Central fetch wrapper:
//  (a) attaches Bearer token when a wardyn_admin_token is set,
//  (b) always sends the OIDC session cookie (credentials: 'include'),
//  (c) routes HTTP 401 to the module-level onUnauthorized handler.
async function wfetch(path: string, init: RequestInit = {}): Promise<Response> {
  const headers = new Headers(init.headers);
  const token = getToken();
  if (token) headers.set("Authorization", `Bearer ${token}`);
  if (init.body != null && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }
  headers.set("Accept", headers.get("Accept") ?? "application/json");

  const res = await fetch(`${BASE}${path}`, {
    ...init,
    headers,
    credentials: "include",
  });

  if (res.status === 401) {
    _unauthorized?.();
    throw new HttpError(401, "Unauthorized");
  }
  return res;
}

async function asJson<T>(res: Response): Promise<T> {
  if (!res.ok) {
    let detail = res.statusText;
    try {
      const body = await res.text();
      if (body) {
        // The control plane returns `{"error": "<human message>"}` on failures.
        // Surface that message verbatim (readable in a toast / inline error) and
        // fall back to the raw body only when it is not that shape.
        try {
          const j = JSON.parse(body) as { error?: unknown };
          detail = typeof j.error === "string" && j.error ? j.error : body;
        } catch {
          detail = body;
        }
      }
    } catch {
      /* ignore */
    }
    throw new HttpError(res.status, detail);
  }
  return (await res.json()) as T;
}

// Read an error/response body as a human string WITHOUT throwing (unlike asJson):
// prefer the control plane's `{"error":"…"}` message, fall back to the raw body,
// then the status text. Used where a non-2xx is an EXPECTED, actionable outcome
// the caller renders inline (e.g. verifyWorkspace's 422/503/409).
async function errText(res: Response): Promise<string> {
  try {
    const body = await res.text();
    if (!body) return res.statusText;
    try {
      const j = JSON.parse(body) as { error?: unknown };
      return typeof j.error === "string" && j.error ? j.error : body;
    } catch {
      return body;
    }
  } catch {
    return res.statusText;
  }
}

// Some backends wrap collections as { items: [...] }; tolerate both.
function unwrapList<T>(payload: unknown): T[] {
  if (Array.isArray(payload)) return payload as T[];
  if (payload && typeof payload === "object") {
    const obj = payload as Record<string, unknown>;
    for (const key of ["items", "data", "results"]) {
      if (Array.isArray(obj[key])) return obj[key] as T[];
    }
  }
  return [];
}

// Probe auth by hitting a protected, cheap endpoint.
export async function probeAuth(): Promise<boolean> {
  try {
    const res = await wfetch("/runs", { method: "GET" });
    return res.ok;
  } catch {
    return false;
  }
}

// ------------------------------------------------------------
// Audit-event projections (backend has no /grants or /egress)
// ------------------------------------------------------------
function str(v: unknown): string | undefined {
  return typeof v === "string" ? v : undefined;
}
function num(v: unknown): number | undefined {
  return typeof v === "number" ? v : undefined;
}

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

// Project egress.allow / egress.deny / egress.pending audit events into
// the EgressDecision shape the run-detail screen renders. Exported so callers
// that already hold a run's audit events can derive egress WITHOUT a second
// /audit round-trip (getEgress fetches audit internally).
export function egressFromAudit(events: AuditEvent[]): EgressDecision[] {
  const map: Record<string, "allow" | "deny" | "pending"> = {
    "egress.allow": "allow",
    "egress.deny": "deny",
    "egress.pending": "pending",
  };
  return events
    .filter((e) => e.action in map)
    .map((e) => {
      const d = (e.data ?? {}) as Record<string, unknown>;
      // Prefer an explicit domain in data; otherwise strip a :port off target.
      const domain =
        str(d.domain) ?? (e.target ? e.target.replace(/:\d+$/, "") : "—");
      return {
        id: e.id,
        time: e.time,
        domain,
        decision: map[e.action],
        bytes: num(d.bytes),
      } satisfies EgressDecision;
    });
}

// Resolve one WorkspaceSelection (from the compose form's onboarded multi-select)
// against the fetched Workspace[] list into the compose wire shape. Mirrors
// wizard-types.ts's buildSpec resolution (repo -> "git", local_dir -> "local").
// Returns undefined for a stale selection (the workspace was deleted after it
// was picked) — the caller skips those rather than sending a dangling reference.
export function resolveComposeWorkspace(
  sel: WorkspaceSelection,
  workspaces: Workspace[],
): ComposeWorkspace | undefined {
  const w = workspaces.find((x) => x.id === sel.workspaceId);
  if (!w) return undefined;
  return w.kind === "repo"
    ? { kind: "git", repo: w.source }
    : { kind: "local", path: w.source, read_write: !sel.readOnly };
}

// Parse an asciicast (v2) recording document into the Recording shape.
// Accepts either JSON ({header, events|stdout}) or raw asciicast text
// (header line + one JSON event array per line).
function parseRecording(runId: string, text: string): Recording {
  const events: AsciicastEvent[] = [];
  let header: Recording["header"] = { version: 2, width: 96, height: 26 };

  const lines = text.split("\n").filter((l) => l.trim().length > 0);
  for (let i = 0; i < lines.length; i++) {
    let parsed: unknown;
    try {
      parsed = JSON.parse(lines[i]);
    } catch {
      continue;
    }
    if (i === 0 && parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
      const h = parsed as Record<string, unknown>;
      header = {
        version: num(h.version) ?? 2,
        width: num(h.width) ?? 96,
        height: num(h.height) ?? 26,
        title: str(h.title),
      };
      continue;
    }
    if (Array.isArray(parsed) && parsed.length >= 3 && parsed[1] === "o") {
      events.push([Number(parsed[0]) || 0, "o", String(parsed[2])]);
    }
  }

  return { run_id: runId, header, events, cast: text };
}

// GET /api/v1/setup/status permissive fallback — an endpoint-less build (older
// backend, or the endpoint mid-rollout in a concurrent workstream) must never
// trap the operator behind an auto-opened wizard. ready:true so the "should we
// auto-open Getting-started" check (shouldOpenSetup) always says no.
const READY_FALLBACK: SetupStatus = {
  ready: true,
  checks: [],
  auth: { mode: "local", local_loopback: true },
  runner: { driver: "none", confinement_classes: [] },
  composer: { enabled: false, backends: [] },
  providers: [],
  secrets: { present: [], github_app: false },
  age_key: { durable: false },
  restart_required: false,
  has_runs: false,
  platform: { os: "", wsl: false },
};

// ------------------------------------------------------------
// Public API surface (signatures preserved)
// ------------------------------------------------------------
export const api = {
  // GET /api/v1/setup/status — first-run readiness snapshot (see types.ts for
  // the frozen contract). 404 (endpoint not built yet) or any other non-ok
  // response, or a thrown network error, degrades to READY_FALLBACK so the
  // Getting-started wizard never auto-opens against a build that can't answer
  // it. A 401 is NOT swallowed here — it's already thrown by wfetch (which
  // routes it through onUnauthorized first), so it propagates as usual.
  async getSetupStatus(): Promise<SetupStatus> {
    try {
      const res = await wfetch("/setup/status", { method: "GET" });
      if (!res.ok) return READY_FALLBACK;
      return (await res.json()) as SetupStatus;
    } catch (e) {
      if (e instanceof HttpError && e.status === 401) throw e;
      return READY_FALLBACK;
    }
  },

  // GET /api/v1/runs
  async listRuns(): Promise<AgentRun[]> {
    const res = await wfetch("/runs", { method: "GET" });
    return unwrapList<AgentRun>(await asJson<unknown>(res));
  },

  // GET /api/v1/runs/{id}
  async getRun(id: string): Promise<AgentRun | undefined> {
    const res = await wfetch(`/runs/${encodeURIComponent(id)}`, { method: "GET" });
    if (res.status === 404) return undefined;
    return asJson<AgentRun>(res);
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
    const res = await wfetch("/runs", { method: "POST", body: JSON.stringify(body) });
    return asJson<CreateRunResult>(res);
  },

  // POST /api/v1/runs/{id}/profile — Recording-Mode profile synthesis (ADVISORY,
  // read-only). Replays the run's observed behaviour into a PROPOSED least-
  // privilege run + inline_policy plus the raw observations + Wardyn's
  // deterministic risk assessment. Never creates a run or mints a credential.
  async profileRun(id: string): Promise<ProfileProposal> {
    const res = await wfetch(`/runs/${encodeURIComponent(id)}/profile`, { method: "POST" });
    return asJson<ProfileProposal>(res);
  },

  // POST /api/v1/runs/compose — AI Run Composer (ADVISORY). Turns a
  // natural-language task description (+ optional uploaded attachment TEXT and
  // source-URL hints) into a PROPOSED run setup for human review. It never
  // creates a run or mints a credential. Returns the proposal, Wardyn's
  // DETERMINISTIC risk assessment, a summary, and any clamp warnings.
  //
  // Errors surface as HttpError with .status so callers can distinguish:
  //   404 — composer not enabled on this control plane
  //   400 — invalid request / unknown backend
  //   413 — request too large (exceeds prompt/attachment/total caps)
  //   502 — backend failed (model error / unparseable output)
  // The response is DISCRIMINATED on `kind`: "questions" (the analyzer needs
  // answers — show them, then re-call with the answers in `transcript` and an
  // incremented `round`) or "proposal" (the final reviewable setup).
  // `availableWorkspaces` is the onboarded-workspace list (listWorkspaces())
  // req.workspaceSelections is resolved against — the caller already fetched it
  // (new-run-dialog.tsx fetches it once per dialog-open). Pass onStage to receive
  // live pipeline-stage keys (validate/detect/propose/…) as the server streams
  // them; without it, the original synchronous JSON path is used (CLI/test
  // parity). The promise resolves to the same ComposeResult either way. Errors
  // surface as HttpError (see status codes above); a mid-stream failure arrives
  // as an SSE error frame and is thrown like the JSON path.
  async compose(
    req: ComposeRequest,
    availableWorkspaces: Workspace[] = [],
    onStage?: (stage: string) => void,
  ): Promise<ComposeResult> {
    const body: Record<string, unknown> = { prompt: req.prompt };
    // The onboarded multi-select wins when present: resolve each selection and
    // send the WHOLE array — its first entry is the primary that drives the
    // analyzer (mirrors internal/api/compose.go's ComposeRequest.Workspaces).
    // Falls back to the legacy singular `workspace` only when no selections were
    // made (ephemeral by default) — the workspace-picker offers no other way to
    // reference an un-onboarded local path/repo (onboarded-only, by design).
    const resolvedWorkspaces = (req.workspaceSelections ?? [])
      .map((sel) => resolveComposeWorkspace(sel, availableWorkspaces))
      .filter((w): w is ComposeWorkspace => w !== undefined);
    if (resolvedWorkspaces.length) {
      body.workspaces = resolvedWorkspaces;
    } else {
      body.workspace = req.workspace ?? { kind: "ephemeral" };
    }
    if (req.attachments?.length) body.attachments = req.attachments;
    if (req.sources?.length) body.sources = req.sources;
    if (req.backend) body.backend = req.backend;
    if (req.mode) body.mode = req.mode;
    if (req.transcript?.length) body.transcript = req.transcript;
    if (req.round) body.round = req.round;
    // Always send the run-mode choice (false = background is meaningful, not a default).
    body.interactive = !!req.interactive;
    // Per-run subscription opt-in (only when ticked; absent = api-key default).
    if (req.useSubscription) body.use_subscription = true;
    // Per-run confinement floor (the operator's persisted default tier). Raw — the
    // server caps it at what the host can enforce; absent = the policy minimum.
    if (req.confinementFloor) body.confinement_floor = req.confinementFloor;
    // Client-owned compose-session id (decision 1: no server-side session store) —
    // resent unchanged on every round of the same describe-mode conversation.
    if (req.sessionId) body.session_id = req.sessionId;

    if (!onStage) {
      const res = await wfetch("/runs/compose", { method: "POST", body: JSON.stringify(body) });
      return asJson<ComposeResult>(res);
    }

    // Streaming path: ask the server to stream pipeline-stage events (SSE). We use
    // fetch()+ReadableStream, NOT EventSource (which is GET-only and can't carry the
    // Bearer header or the POST body). Frames are `data: <ComposeEvent json>\n\n`;
    // the terminal frame is {type:"result"} (or {type:"error"}). Pre-flush 4xx and a
    // non-streaming server both come back as a plain JSON response, handled below.
    const res = await wfetch("/runs/compose", {
      method: "POST",
      body: JSON.stringify(body),
      headers: { Accept: "text/event-stream" },
    });
    const ct = res.headers.get("Content-Type") ?? "";
    if (!res.ok || !res.body || !ct.includes("text/event-stream")) {
      return asJson<ComposeResult>(res); // error body throws; a JSON 200 parses normally
    }

    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    let result: ComposeResult | undefined;
    for (;;) {
      const { value, done } = await reader.read();
      if (value) buf += decoder.decode(value, { stream: true });
      let sep: number;
      while ((sep = buf.indexOf("\n\n")) !== -1) {
        const frame = buf.slice(0, sep);
        buf = buf.slice(sep + 2);
        const dataLine = frame.split("\n").find((l) => l.startsWith("data:"));
        if (!dataLine) continue;
        const ev = JSON.parse(dataLine.slice(5).trim()) as {
          type: string;
          stage?: string;
          result?: ComposeResult;
          error?: string;
        };
        if (ev.type === "stage" && ev.stage) onStage(ev.stage);
        else if (ev.type === "result" && ev.result) result = ev.result;
        else if (ev.type === "error") throw new HttpError(502, ev.error || "compose failed");
      }
      if (done) break;
    }
    if (!result) throw new HttpError(502, "compose stream ended without a result");
    return result;
  },

  // POST /api/v1/runs/compose/telemetry — fire-and-forget client beacon for the
  // pre-launch composer funnel (mode transitions). Feeds the same audit chokepoint
  // as everything else (recordAudit → run.compose.client) — not a new analytics
  // pipeline. Records the mode + a client-generated correlation id + risk level
  // ONLY — never prompt/secret content. Best-effort: swallow all errors so a
  // beacon never disrupts the dialog.
  async telemetry(data: { mode: string; correlation_id?: string; risk?: string }): Promise<void> {
    try {
      await wfetch("/runs/compose/telemetry", { method: "POST", body: JSON.stringify(data) });
    } catch {
      /* best-effort beacon — never throw */
    }
  },

  // POST /api/v1/runs/compose/assist — ADVISORY, escalation-only help agent. Answers
  // the operator's free-text question with the current step's structured context in
  // view. The answer is inert display text — it never changes the proposal/policy.
  async ask(req: ComposeAssistRequest): Promise<string> {
    const res = await wfetch("/runs/compose/assist", {
      method: "POST",
      body: JSON.stringify(req),
    });
    return (await asJson<ComposeAssistResponse>(res)).answer;
  },

  // GET /api/v1/composer/backends — the configured composer backends. Returns an
  // empty list when the composer is disabled (or 404, mapped to []). The UI
  // preselects the is_default backend and hides the picker when 0/1 are present.
  async listComposerBackends(): Promise<ComposerBackend[]> {
    const res = await wfetch("/composer/backends", { method: "GET" });
    if (res.status === 404) return [];
    if (!res.ok) {
      throw new HttpError(res.status, `HTTP ${res.status}: failed to list composer backends`);
    }
    const payload = await asJson<{ backends?: unknown }>(res);
    return Array.isArray(payload?.backends) ? (payload.backends as ComposerBackend[]) : [];
  },

  // POST /api/v1/runs/{id}/kill  -> 202 Accepted
  async killRun(id: string): Promise<void> {
    const res = await wfetch(`/runs/${encodeURIComponent(id)}/kill`, { method: "POST" });
    if (!res.ok) {
      throw new HttpError(res.status, `HTTP ${res.status}: failed to kill run`);
    }
  },

  // GET /api/v1/runs/{id}/grants — the run's credential-grant eligibility
  // records (what it MAY request), including grants that were never minted.
  async getGrants(runId: string): Promise<CredentialGrant[]> {
    const res = await wfetch(`/runs/${encodeURIComponent(runId)}/grants`, { method: "GET" });
    if (res.status === 404) return [];
    return grantsFromRecords(await asJson<unknown>(res));
  },

  // GET /api/v1/runs/{id}/egress — synthesized from audit egress.* events.
  async getEgress(runId: string): Promise<EgressDecision[]> {
    const events = await api.listAudit(runId);
    return egressFromAudit(events);
  },

  // GET /api/v1/approvals?state=<state>  (PENDING | APPROVED | DENIED | EXPIRED | "")
  async listApprovals(state: "PENDING" | "APPROVED" | "DENIED" | "EXPIRED" | "" = "PENDING"): Promise<ApprovalRequest[]> {
    const qs = state ? `?state=${encodeURIComponent(state)}` : "";
    const res = await wfetch(`/approvals${qs}`, { method: "GET" });
    return unwrapList<ApprovalRequest>(await asJson<unknown>(res));
  },

  // POST /api/v1/approvals/{id}/approve  { reason }
  async approve(id: string, reason: string): Promise<ApprovalRequest> {
    const res = await wfetch(`/approvals/${encodeURIComponent(id)}/approve`, {
      method: "POST",
      body: JSON.stringify({ reason }),
    });
    return asJson<ApprovalRequest>(res);
  },

  // POST /api/v1/approvals/{id}/deny  { reason }
  async deny(id: string, reason: string): Promise<ApprovalRequest> {
    const res = await wfetch(`/approvals/${encodeURIComponent(id)}/deny`, {
      method: "POST",
      body: JSON.stringify({ reason }),
    });
    return asJson<ApprovalRequest>(res);
  },

  // GET /api/v1/audit?run_id=   (run_id optional)
  async listAudit(runId?: string): Promise<AuditEvent[]> {
    const qs = runId ? `?run_id=${encodeURIComponent(runId)}` : "";
    const res = await wfetch(`/audit${qs}`, { method: "GET" });
    return unwrapList<AuditEvent>(await asJson<unknown>(res));
  },

  // GET /api/v1/runs/{id}/recording/{id}  (asciicast text)
  async getRecording(runId: string): Promise<Recording | undefined> {
    const res = await wfetch(
      `/runs/${encodeURIComponent(runId)}/recording/${encodeURIComponent(runId)}`,
      { method: "GET", headers: { Accept: "text/plain, application/json" } },
    );
    if (res.status === 404) return undefined;
    if (!res.ok) {
      throw new HttpError(res.status, `HTTP ${res.status}: failed to load recording`);
    }
    const text = await res.text();
    if (!text.trim()) return undefined;
    return parseRecording(runId, text);
  },

  // GET /api/v1/policies — all run policies (reverse creation order).
  async listPolicies(): Promise<RunPolicy[]> {
    const res = await wfetch("/policies", { method: "GET" });
    return unwrapList<RunPolicy>(await asJson<unknown>(res));
  },

  // GET /api/v1/policies/{id}
  async getPolicy(id: string): Promise<RunPolicy | undefined> {
    const res = await wfetch(`/policies/${encodeURIComponent(id)}`, { method: "GET" });
    if (res.status === 404) return undefined;
    return asJson<RunPolicy>(res);
  },

  // POST /api/v1/policies  { name, spec } -> 201 created policy.
  // The server validates the spec; a 400 surfaces as an HttpError.
  async createPolicy(name: string, spec: RunPolicySpec): Promise<RunPolicy> {
    const res = await wfetch("/policies", {
      method: "POST",
      body: JSON.stringify({ name, spec }),
    });
    return asJson<RunPolicy>(res);
  },

  // PUT /api/v1/policies/{id}  { name, spec } -> updated policy.
  async updatePolicy(id: string, name: string, spec: RunPolicySpec): Promise<RunPolicy> {
    const res = await wfetch(`/policies/${encodeURIComponent(id)}`, {
      method: "PUT",
      body: JSON.stringify({ name, spec }),
    });
    return asJson<RunPolicy>(res);
  },

  // DELETE /api/v1/policies/{id} -> 204.
  async deletePolicy(id: string): Promise<void> {
    const res = await wfetch(`/policies/${encodeURIComponent(id)}`, { method: "DELETE" });
    if (!res.ok && res.status !== 404) {
      throw new HttpError(res.status, `HTTP ${res.status}: failed to delete policy`);
    }
  },

  // GET /api/v1/secrets -> { names: [...] }. Returns secret NAMES only; values
  // are write-only and never surfaced by the API.
  async listSecrets(): Promise<SecretName[]> {
    const res = await wfetch("/secrets", { method: "GET" });
    const payload = await asJson<{ names?: unknown }>(res);
    return Array.isArray(payload?.names) ? (payload.names as SecretName[]) : [];
  },

  // PUT /api/v1/secrets/{name}  { value } -> 204. Stores or overwrites a named
  // secret; the value is write-only.
  async setSecret(name: string, value: string): Promise<void> {
    const res = await wfetch(`/secrets/${encodeURIComponent(name)}`, {
      method: "PUT",
      body: JSON.stringify({ value }),
    });
    if (!res.ok) {
      let detail = res.statusText;
      try {
        const body = await res.text();
        if (body) detail = body;
      } catch {
        /* ignore */
      }
      throw new HttpError(res.status, `HTTP ${res.status}: ${detail}`);
    }
  },

  // DELETE /api/v1/secrets/{name} -> 204.
  async deleteSecret(name: string): Promise<void> {
    const res = await wfetch(`/secrets/${encodeURIComponent(name)}`, { method: "DELETE" });
    if (!res.ok && res.status !== 404) {
      throw new HttpError(res.status, `HTTP ${res.status}: failed to delete secret`);
    }
  },

  // GET /api/v1/workspaces — onboarded local dirs + repos (admin-gated). Run-
  // creation pickers offer ONLY these; a run may not reference any other source.
  async listWorkspaces(): Promise<Workspace[]> {
    const res = await wfetch("/workspaces", { method: "GET" });
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
  }): Promise<Workspace> {
    const res = await wfetch("/workspaces", { method: "POST", body: JSON.stringify(input) });
    return asJson<Workspace>(res);
  },

  // PUT /api/v1/workspaces/{id}  same body shape -> updated workspace.
  async updateWorkspace(
    id: string,
    input: { name: string; kind: WorkspaceKind; source: string; ref?: string; default_target?: string },
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
    return (await asJson<VerifyFixSuggestion>(res)).suggestion ?? "";
  },

  // POST /api/v1/workspaces/{id}/record { task_key, mode }. Kicks an OPEN
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
    if (res.status === 404) return api.setApprovedEgress(id, fallbackDomains);
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
      throw new HttpError(res.status, `HTTP ${res.status}: failed to delete workspace`);
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

  // GET /api/v1/site-config — the operator-wide baseline (upstream proxy secret
  // ref / per-ecosystem artifact-registry overrides / default SCM hosts). An
  // operator who has never configured one gets the zero value ({}), not a 404 —
  // the backend treats "unconfigured" as a valid, common state; the 404 check
  // here is just defensive for an older/mid-rollout backend.
  async getSiteConfig(): Promise<SiteConfig> {
    const res = await wfetch("/site-config", { method: "GET" });
    if (res.status === 404) return {};
    return asJson<SiteConfig>(res);
  },

  // PUT /api/v1/site-config — REPLACES the whole document; callers must GET
  // first and spread onto the current value to avoid clobbering fields they
  // don't intend to change. Admin-gated server-side: a non-admin human gets a
  // 403, which surfaces here as an HttpError (via asJson) like any other write.
  async putSiteConfig(cfg: SiteConfig): Promise<void> {
    const res = await wfetch("/site-config", { method: "PUT", body: JSON.stringify(cfg) });
    await asJson<SiteConfig>(res);
  },

  // GET /healthz — liveness + trust boundary (unauthenticated; surfaced in the
  // shell so the real trust domain / identity provider are always visible).
  async health(): Promise<{ trust_domain?: string; identity_provider?: string; runner?: string; confinement_classes?: string[] }> {
    try {
      const res = await fetch("/healthz", { credentials: "include" });
      if (!res.ok) return {};
      return (await res.json()) as Record<string, unknown>;
    } catch {
      return {};
    }
  },

  // POST /api/v1/auth/logout — terminate the server-side OIDC session.
  //
  // HIGH fix (sign-out): clearing the local admin token is not enough — the
  // OIDC session cookie is HttpOnly and lives on the server, so without this
  // call the very next auth probe (which sends the cookie) silently re-signs the
  // operator back in. We MUST tell the server to clear the session. The cookie
  // is sent via credentials:"include". Best-effort: a failed logout (server
  // error / network down) still resolves so the client can fall back to the
  // sign-in gate; we never want a hung spinner blocking sign-out.
  async logout(): Promise<void> {
    // FIX #6: do NOT silently swallow a failed logout — a non-OK response or a
    // network error means the server-side OIDC session may STILL be valid, so the
    // operator only *believes* they signed out. Surface it (console.error) while
    // still resolving, so the caller can fall back to the sign-in gate without a
    // hung spinner, but a failed sign-out is never invisible.
    try {
      const res = await wfetch("/auth/logout", { method: "POST" });
      if (!res.ok) {
        console.error(`logout: server returned HTTP ${res.status}; session may still be active`);
      }
    } catch (err) {
      console.error("logout: request failed; session may still be active", err);
    }
  },

  // GET /api/v1/me — the authenticated principal + auth method.
  async whoami(): Promise<{ principal: string; method: string } | null> {
    try {
      const res = await wfetch("/me", { method: "GET" });
      if (!res.ok) return null;
      return (await res.json()) as { principal: string; method: string };
    } catch {
      return null;
    }
  },
};
