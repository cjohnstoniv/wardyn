/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// AI Run Composer (ADVISORY): natural-language task -> proposed run setup, plus
// the escalation help agent, telemetry beacon, and configured-backend list. The
// heaviest domain (SSE streaming), so keeping it in its own module lets routes
// that never compose drop it from their chunk.
import type {
  ComposeAssistRequest,
  ComposeAssistResponse,
  ComposeRequest,
  ComposeResult,
  ComposeWorkspace,
  ComposerBackend,
  Workspace,
  WorkspaceSelection,
} from "../types";
import { asJson, errText, HttpError, wfetch } from "./core";

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

export const composer = {
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
      throw new HttpError(res.status, await errText(res));
    }
    const payload = await asJson<{ backends?: unknown }>(res);
    return Array.isArray(payload?.backends) ? (payload.backends as ComposerBackend[]) : [];
  },
};
