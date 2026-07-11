/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// HarnessLoginPane — "Connect via container login" for a Claude subscription in
// deployments with no host ~/.claude (compose/team). Cribs the record-pane
// pattern: launch an interactive login sandbox, embed the AttachTerminal, the
// operator runs `claude setup-token` (remote OAuth callback + paste-code — no
// localhost dependency), then pastes the printed long-lived token into the field
// below. Wardyn stores it and injects it proxy-side into every later run; the
// sandbox never holds a live credential. This pane renders inline (never routes
// away) exactly like record-pane, so it drops into the ModelStep unchanged.
import * as React from "react";
import { Loader2, ShieldCheck, TriangleAlert, KeyRound, Square } from "lucide-react";
import { api } from "../../../lib/api";
import { AttachTerminal } from "../../attach-terminal";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";

type Phase = "idle" | "launching" | "attached" | "saving" | "done" | "error";

export function HarnessLoginPane({
  provider = "anthropic",
  onDone,
  onCancel,
}: {
  provider?: string;
  // Called after the token is captured (parent refreshes setup status + closes).
  onDone: () => void;
  // Called when the operator backs out before capturing.
  onCancel: () => void;
}) {
  const [phase, setPhase] = React.useState<Phase>("idle");
  const [runId, setRunId] = React.useState<string | null>(null);
  const [token, setToken] = React.useState("");
  const [error, setError] = React.useState("");

  const launch = React.useCallback(async () => {
    setPhase("launching");
    setError("");
    try {
      const id = await api.harnessLogin(provider);
      setRunId(id);
      setPhase("attached");
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setPhase("error");
    }
  }, [provider]);

  const save = React.useCallback(async () => {
    const t = token.trim();
    if (!t) return;
    setPhase("saving");
    setError("");
    try {
      await api.harnessCredentialPaste(provider, t);
      // Best-effort: stop the login sandbox now that we have the token.
      if (runId) await api.killRun(runId).catch(() => {});
      setPhase("done");
      onDone();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setPhase("attached"); // stay on the terminal so they can retry the paste
    }
  }, [provider, token, runId, onDone]);

  const cancel = React.useCallback(() => {
    if (runId) api.killRun(runId).catch(() => {});
    onCancel();
  }, [runId, onCancel]);

  return (
    <div className="space-y-3 rounded-lg border border-border bg-surface-2/40 p-3" data-testid="harness-login-pane">
      <div className="flex items-center gap-2">
        <KeyRound className="size-4 shrink-0 text-primary" />
        <span className="text-sm font-medium text-foreground">Connect a Claude subscription via container login</span>
      </div>
      <p className="text-xs leading-relaxed text-muted-foreground">
        No Claude Code on your machine? Wardyn opens a sandbox for you. In the terminal below run{" "}
        <code className="rounded bg-background/70 px-1 py-0.5 font-mono">claude setup-token</code>, open the printed URL
        in your own browser, paste the code back, then copy the long-lived token it prints into the field below. Wardyn
        stores it and injects it proxy-side into every run — the sandbox never holds a live credential.
      </p>

      {error && (
        <div className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2 text-xs text-warning">
          <TriangleAlert className="mt-0.5 size-4 shrink-0" />
          <p>{error}</p>
        </div>
      )}

      {phase === "idle" && (
        <div className="flex flex-wrap gap-2">
          <Button size="sm" onClick={launch}>
            <KeyRound className="size-3.5" /> Open login sandbox
          </Button>
          <Button size="sm" variant="ghost" onClick={onCancel}>
            Cancel
          </Button>
        </div>
      )}

      {phase === "launching" && (
        <p className="flex items-center gap-2 text-xs text-muted-foreground">
          <Loader2 className="size-3.5 animate-spin" /> Starting the login sandbox…
        </p>
      )}

      {(phase === "attached" || phase === "saving") && runId && (
        <div className="space-y-2">
          <AttachTerminal runId={runId} />
          <div className="flex flex-wrap items-center gap-2">
            <Input
              value={token}
              onChange={(e) => setToken(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") save();
              }}
              placeholder="paste the setup-token output (starts with sk-ant-oat…)"
              className="h-9 min-w-[18rem] flex-1 font-mono"
              aria-label="setup-token"
              type="password"
            />
            <Button size="sm" onClick={save} disabled={phase === "saving" || !token.trim()}>
              {phase === "saving" ? <Loader2 className="size-3.5 animate-spin" /> : <ShieldCheck className="size-3.5" />}
              Save token
            </Button>
            <Button size="sm" variant="outline" onClick={cancel}>
              <Square className="size-3.5" /> Cancel
            </Button>
          </div>
        </div>
      )}

      {phase === "done" && (
        <p className="flex items-center gap-2 text-xs text-success">
          <ShieldCheck className="size-3.5" /> Token captured — your Claude subscription is connected.
        </p>
      )}
    </div>
  );
}
