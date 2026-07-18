/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// HarnessLoginPane — "Connect via container login" for a Claude subscription in
// deployments with no host ~/.claude (compose/team). It launches an interactive
// login sandbox IMMEDIATELY (opening the pane IS the intent — no extra button),
// embeds the AttachTerminal, AUTO-TYPES `claude setup-token`, AUTO-OPENS the
// printed OAuth URL in a new browser tab, and AUTO-CAPTURES the printed
// long-lived token straight off the terminal stream — then stores it and injects
// it proxy-side into every later run; the sandbox never holds a live credential.
// The only unavoidable human step is approving the OAuth in the browser and
// pasting the callback code back into the terminal. A manual paste field remains
// as a fallback if auto-capture misses. Renders inline (never routes away).
import * as React from "react";
import { Loader2, ShieldCheck, TriangleAlert, KeyRound, Square, ExternalLink, CornerDownLeft } from "lucide-react";
import { harnessAuth as harnessAuthApi } from "../../../lib/api/harness-auth";
import { runs as runsApi } from "../../../lib/api/runs";
import { AttachTerminal, type AttachTerminalHandle } from "../../attach-terminal";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";

type Phase = "launching" | "attached" | "saving" | "done" | "error";

// The command Wardyn types into the login sandbox for the operator.
const SETUP_TOKEN_CMD = "claude setup-token";

// extractSetupToken pulls a COMPLETE `claude setup-token` token out of a chunk of
// terminal output. Shape: `sk-ant-oat<2 digits>-<long url-safe body>`. We only
// return a match followed by another character (newline, ANSI reset, …) — proof
// the token finished printing — so a token still streaming in (truncated at the
// buffer's end) is not captured early. Exported for tests.
export function extractSetupToken(s: string): string | null {
  const re = /sk-ant-oat\d{2}-[A-Za-z0-9_-]{40,}/g;
  let m: RegExpExecArray | null;
  while ((m = re.exec(s)) !== null) {
    if (m.index + m[0].length < s.length) return m[0];
  }
  return null;
}

// extractAuthUrl pulls the `claude setup-token` OAuth authorization URL out of a
// chunk of terminal output so we can open it in a new tab. Restricted to the
// known Claude/Anthropic auth hosts (never api.anthropic.com — that's the token
// exchange, not a user-facing page). Same trailing-boundary rule as the token so
// a still-streaming URL isn't opened truncated. Exported for tests.
export function extractAuthUrl(s: string): string | null {
  const re = /https:\/\/(?:claude\.ai|claude\.com|console\.anthropic\.com|platform\.claude\.com)\/[^\s'"<>]+/gi;
  let m: RegExpExecArray | null;
  while ((m = re.exec(s)) !== null) {
    if (m.index + m[0].length < s.length) return m[0].replace(/[.,)]+$/, "");
  }
  return null;
}

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
  const [phase, setPhase] = React.useState<Phase>("launching");
  const [runId, setRunId] = React.useState<string | null>(null);
  const [token, setToken] = React.useState("");
  const [error, setError] = React.useState("");
  const [autoCaptured, setAutoCaptured] = React.useState(false);
  const [authUrl, setAuthUrl] = React.useState("");
  const [code, setCode] = React.useState("");

  const termRef = React.useRef<AttachTerminalHandle>(null);

  // Rolling buffer of recent PTY output + latches so we act on each thing once.
  const outBufRef = React.useRef("");
  const savedRef = React.useRef(false);
  const openedUrlRef = React.useRef(false);
  const launchedRef = React.useRef(false);

  const launch = React.useCallback(async () => {
    setPhase("launching");
    setError("");
    outBufRef.current = "";
    savedRef.current = false;
    openedUrlRef.current = false;
    setAutoCaptured(false);
    setAuthUrl("");
    try {
      const id = await harnessAuthApi.harnessLogin(provider);
      setRunId(id);
      setPhase("attached");
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setPhase("error");
    }
  }, [provider]);

  // Opening the pane IS the intent to connect — launch the sandbox immediately,
  // once. (The ref guards React's dev-mode double-invoke of mount effects.)
  React.useEffect(() => {
    if (launchedRef.current) return;
    launchedRef.current = true;
    void launch();
  }, [launch]);

  // saveToken stores a token (explicit from auto-capture, or the pasted field).
  const saveToken = React.useCallback(
    async (explicit?: string) => {
      const t = (explicit ?? token).trim();
      if (!t || savedRef.current) return;
      savedRef.current = true;
      setPhase("saving");
      setError("");
      try {
        await harnessAuthApi.harnessCredentialPaste(provider, t);
        if (runId) await runsApi.killRun(runId).catch(() => {});
        setPhase("done");
        onDone();
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
        setPhase("attached"); // stay on the terminal so they can retry the paste
        savedRef.current = false; // allow another attempt (auto or manual)
      }
    },
    [provider, token, runId, onDone],
  );

  // Watch the login terminal: open the OAuth URL in a new tab, then capture and
  // save the printed token — both automatically.
  const handleOutput = React.useCallback(
    (chunk: string) => {
      outBufRef.current = (outBufRef.current + chunk).slice(-16384);
      if (!openedUrlRef.current) {
        const url = extractAuthUrl(outBufRef.current);
        if (url) {
          openedUrlRef.current = true;
          setAuthUrl(url);
          // Best-effort auto-open. A browser may block a popup not tied to a user
          // gesture; the surfaced link below is the reliable one-click fallback.
          try {
            window.open(url, "_blank", "noopener,noreferrer");
          } catch {
            /* blocked — the visible link covers it */
          }
        }
      }
      if (savedRef.current) return;
      const tok = extractSetupToken(outBufRef.current);
      if (tok) {
        setToken(tok);
        setAutoCaptured(true);
        void saveToken(tok);
      }
    },
    [saveToken],
  );

  // Bridge the pasted login code into the terminal's stdin, so the operator uses
  // a normal input field with native paste instead of the terminal's Ctrl+Shift+V.
  const sendCode = React.useCallback(() => {
    const c = code.trim();
    if (!c) return;
    termRef.current?.sendText(c + "\r");
    setCode("");
  }, [code]);

  const cancel = React.useCallback(() => {
    if (runId) runsApi.killRun(runId).catch(() => {});
    onCancel();
  }, [runId, onCancel]);

  return (
    <div className="space-y-3 rounded-lg border border-border bg-surface-2/40 p-3" data-testid="harness-login-pane">
      <div className="flex items-center gap-2">
        <KeyRound className="size-4 shrink-0 text-primary" />
        <span className="text-sm font-medium text-foreground">Connect a Claude subscription via container login</span>
      </div>
      <p className="text-xs leading-relaxed text-muted-foreground">
        Wardyn opened a sandbox and is running{" "}
        <code className="rounded bg-background/70 px-1 py-0.5 font-mono">claude setup-token</code> for you. It opens the
        Claude login page in a new tab — approve it, then paste the code it gives you into the{" "}
        <span className="font-medium">field below</span> (not the terminal) and hit Send. Wardyn captures the printed
        token automatically and connects your subscription; the token is injected proxy-side into every run and the
        sandbox never holds a live credential.
      </p>

      {error && (
        <div className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2 text-xs text-warning">
          <TriangleAlert className="mt-0.5 size-4 shrink-0" />
          <p>{error}</p>
        </div>
      )}

      {phase === "launching" && (
        <p className="flex items-center gap-2 text-xs text-muted-foreground">
          <Loader2 className="size-3.5 animate-spin" /> Opening the login sandbox…
        </p>
      )}

      {phase === "error" && (
        <div className="flex flex-wrap gap-2">
          <Button size="sm" onClick={() => void launch()}>
            <KeyRound className="size-3.5" /> Try again
          </Button>
          <Button size="sm" variant="ghost" onClick={onCancel}>
            Cancel
          </Button>
        </div>
      )}

      {(phase === "attached" || phase === "saving") && runId && (
        <div className="space-y-2">
          {authUrl && (
            <a
              href={authUrl}
              target="_blank"
              rel="noopener noreferrer"
              className="inline-flex items-center gap-1.5 rounded-md border border-primary/40 bg-primary/10 px-2.5 py-1.5 text-xs font-medium text-primary hover:bg-primary/20"
              data-testid="auth-url-link"
            >
              <ExternalLink className="size-3.5" /> Open the Claude login page ↗
            </a>
          )}
          <AttachTerminal ref={termRef} runId={runId} autoRun={SETUP_TOKEN_CMD} onOutput={handleOutput} />
          {autoCaptured ? (
            <p className="flex items-center gap-2 text-xs text-success" data-testid="auto-capture-note">
              {phase === "saving" ? <Loader2 className="size-3.5 animate-spin" /> : <ShieldCheck className="size-3.5" />}
              Token detected — connecting your subscription…
            </p>
          ) : (
            <div className="space-y-2">
              {/* Primary interaction: paste the login-page code; we type it into
                  the terminal's stdin so the operator never needs Ctrl+Shift+V. */}
              <label className="block text-xs font-medium text-foreground" htmlFor="harness-login-code">
                Paste the code from the login page
              </label>
              <div className="flex flex-wrap items-center gap-2">
                <Input
                  id="harness-login-code"
                  value={code}
                  onChange={(e) => setCode(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter") sendCode();
                  }}
                  placeholder="paste the code Claude gave you, then press Enter"
                  className="h-9 min-w-[18rem] flex-1 font-mono"
                  aria-label="login code"
                />
                <Button size="sm" onClick={sendCode} disabled={!code.trim()}>
                  <CornerDownLeft className="size-3.5" /> Send code
                </Button>
                <Button size="sm" variant="outline" onClick={cancel}>
                  <Square className="size-3.5" /> Cancel
                </Button>
              </div>
              {/* Fallback: paste the final token directly if auto-capture missed. */}
              <details className="text-xs text-muted-foreground">
                <summary className="cursor-pointer select-none py-1">Token didn&apos;t auto-capture?</summary>
                <div className="mt-1 flex flex-wrap items-center gap-2">
                  <Input
                    value={token}
                    onChange={(e) => setToken(e.target.value)}
                    onKeyDown={(e) => {
                      if (e.key === "Enter") void saveToken();
                    }}
                    placeholder="paste the sk-ant-oat… token"
                    className="h-9 min-w-[18rem] flex-1 font-mono"
                    aria-label="setup-token"
                    type="password"
                  />
                  <Button
                    size="sm"
                    variant="secondary"
                    onClick={() => void saveToken()}
                    disabled={phase === "saving" || !token.trim()}
                  >
                    {phase === "saving" ? (
                      <Loader2 className="size-3.5 animate-spin" />
                    ) : (
                      <ShieldCheck className="size-3.5" />
                    )}
                    Save token
                  </Button>
                </div>
              </details>
            </div>
          )}
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
