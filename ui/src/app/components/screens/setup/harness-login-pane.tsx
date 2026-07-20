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
//
// The AWS flow adds one step BEFORE the sandbox launches: it asks for the
// organization's access portal (start) URL, because `aws sso login` reads
// sso_start_url + sso_region from ~/.aws/config and Wardyn stores no start URL
// (the region is daemon boot config). The server seeds both into the sandbox as
// a credential-free ~/.aws/config, which is what makes the auto-typed
// `aws sso login --sso-session wardyn …` run unattended.
import * as React from "react";
import { Loader2, ShieldCheck, TriangleAlert, KeyRound, Square, ExternalLink, CornerDownLeft } from "lucide-react";
import { harnessAuth as harnessAuthApi } from "../../../lib/api/harness-auth";
import { runs as runsApi } from "../../../lib/api/runs";
import { AttachTerminal, type AttachTerminalHandle } from "../../attach-terminal";
import { Button } from "../../ui/button";
import { Input } from "../../ui/input";

type Phase = "prompt" | "launching" | "attached" | "saving" | "done" | "error";

// Per-provider login conventions. Adding a provider is a new row here (mirrors
// the server-side agentHarnessLogin table), not a forked component.
//
// The two flows differ in HOW the credential comes back:
//   · anthropic — `claude setup-token` PRINTS the token, so we scrape it off the
//     PTY and PUT it (capture: "scrape").
//   · aws — `aws sso login` writes its token to ~/.aws/sso/cache/*.json and
//     prints only a short-lived device code + verification URL. The in-sandbox
//     `wardyn-aws-sso` helper uploads the file through the brokered internal
//     endpoint, so the pane never sees (and must never scrape) a credential —
//     it just watches for the helper's success marker (capture: "helper").
type CaptureMode = "scrape" | "helper";
type LoginFlow = {
  cmd: string;
  title: string;
  blurb: React.ReactNode;
  capture: CaptureMode;
  // Marker the in-sandbox helper prints on success (capture: "helper" only).
  doneMarker?: string;
  // The flow cannot start until the operator supplies their AWS access-portal
  // start URL: `aws sso login` reads sso_start_url + sso_region from the
  // sandbox's ~/.aws/config, and Wardyn stores no start URL anywhere (the region
  // is boot config; the start URL is per-organization and asked for here). The
  // server seeds both into the sandbox before the command is auto-typed.
  needsStartUrl?: boolean;
};

// isLikelyStartUrl mirrors the server's validateSSOStartURL (harnesscred.go) so
// the operator sees the problem before a round trip. Deliberately loose — the
// server is the authority, and the egress policy, not this check, decides what
// the sandbox may dial. Exported for tests.
export function isLikelyStartUrl(s: string): boolean {
  const v = s.trim();
  return /^https:\/\/[^\s/]+/.test(v);
}

const LOGIN_FLOWS: Record<string, LoginFlow> = {
  anthropic: {
    cmd: "claude setup-token",
    title: "Connect a Claude subscription via container login",
    capture: "scrape",
    blurb: (
      <>
        Wardyn opened a sandbox and is running{" "}
        <code className="rounded bg-background/70 px-1 py-0.5 font-mono">claude setup-token</code> for you. It opens the
        Claude login page in a new tab — approve it, then paste the code it gives you into the{" "}
        <span className="font-medium">field below</span> (not the terminal) and hit Send. Wardyn captures the printed
        token automatically and connects your subscription; the token is injected proxy-side into every run and the
        sandbox never holds a live credential.
      </>
    ),
  },
  aws: {
    // --sso-session wardyn selects the [sso-session wardyn] block the server
    // seeded into ~/.aws/config; chained so the helper uploads the moment the
    // login succeeds — the operator never has to run a second command.
    cmd: "aws sso login --sso-session wardyn --no-browser --use-device-code && wardyn-aws-sso",
    title: "Connect an AWS SSO session via container login",
    capture: "helper",
    doneMarker: "wardyn: aws sso credential captured",
    needsStartUrl: true,
    blurb: (
      <>
        Give Wardyn your organization&apos;s AWS access portal URL and it opens a sandbox, writes a minimal{" "}
        <code className="rounded bg-background/70 px-1 py-0.5 font-mono">~/.aws/config</code> holding just that URL and
        the configured SSO region (no credential — the sandbox has none to start with), and runs{" "}
        <code className="rounded bg-background/70 px-1 py-0.5 font-mono">aws sso login</code> for you. It prints a
        verification URL and a short user code — open the link in any browser, enter the code, and approve. Wardyn then
        captures the SSO session automatically so later Bedrock runs can exchange it for short-lived role credentials —
        with no host <code className="rounded bg-background/70 px-1 py-0.5 font-mono">~/.aws</code> mount and no static
        keys.
      </>
    ),
  },
};

function loginFlow(provider: string): LoginFlow {
  return LOGIN_FLOWS[provider] ?? LOGIN_FLOWS.anthropic;
}

// Force the login PTY wide so `claude setup-token` never hard-wraps the OAuth URL
// (~200 chars) or the token across lines — a narrow PTY wrap mid-URL dropped
// response_type=code and produced "Invalid OAuth Request" on the opened tab. The
// visual xterm still fits its pane (long lines soft-wrap for display, stay
// copyable); only the width claude sees is widened, so the byte stream the
// extractors read has each value on one line.
const LOGIN_PTY_COLS = 512;

// The login PTY is forced WIDE (LOGIN_PTY_COLS below) so `claude setup-token`
// prints the OAuth URL and the sk-ant-oat token each on a SINGLE line — the earlier
// "Invalid OAuth Request" bug was a narrow PTY hard-wrapping the URL mid-string
// (dropping response_type=code). With no wrap in the byte stream, these two
// single-line extractors are correct and need no reassembly.

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
// chunk of terminal output so we can open it in a new tab. Restricted to the known
// Claude/Anthropic auth hosts (never api.anthropic.com — that's the token exchange,
// not a user-facing page). Same trailing-boundary rule as the token so a
// still-streaming URL isn't opened truncated. Exported for tests.
export function extractAuthUrl(s: string): string | null {
  const re = /https:\/\/(?:claude\.ai|claude\.com|console\.anthropic\.com|platform\.claude\.com)\/[^\s'"<>]+/gi;
  let m: RegExpExecArray | null;
  while ((m = re.exec(s)) !== null) {
    if (m.index + m[0].length < s.length) return m[0].replace(/[.,)]+$/, "");
  }
  return null;
}

// extractDeviceVerificationUrl pulls the AWS SSO device-authorization verification
// URL out of `aws sso login --no-browser --use-device-code` output. Restricted to
// the IAM Identity Center device endpoint + the org access portal; the CLI prints
// both a bare URL and (usually) a `verificationUriComplete` with ?user_code=…,
// and we prefer the complete one since it pre-fills the code. Same
// trailing-boundary rule as the others so a still-streaming URL isn't opened
// truncated. Exported for tests.
export function extractDeviceVerificationUrl(s: string): string | null {
  const re = /https:\/\/(?:device\.sso\.[a-z0-9-]+\.amazonaws\.com|[a-z0-9-]+\.awsapps\.com)\/[^\s'"<>]*/gi;
  let best: string | null = null;
  let m: RegExpExecArray | null;
  while ((m = re.exec(s)) !== null) {
    if (m.index + m[0].length >= s.length) continue; // still streaming
    const url = m[0].replace(/[.,)]+$/, "");
    // Prefer the pre-filled variant so the operator doesn't retype the code.
    if (url.includes("user_code=")) return url;
    best = url;
  }
  return best;
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
  const flow = loginFlow(provider);
  const [phase, setPhase] = React.useState<Phase>(flow.needsStartUrl ? "prompt" : "launching");
  const [startUrl, setStartUrl] = React.useState("");
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
      const id = await harnessAuthApi.harnessLogin(provider, startUrl.trim());
      setRunId(id);
      setPhase("attached");
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setPhase("error");
    }
  }, [provider, startUrl]);

  // Opening the pane IS the intent to connect — launch the sandbox immediately,
  // once. (The ref guards React's dev-mode double-invoke of mount effects.)
  // EXCEPT when the flow needs a start URL first (AWS): there is nothing to
  // launch until the operator supplies it, so the pane waits on the form below.
  React.useEffect(() => {
    if (flow.needsStartUrl || launchedRef.current) return;
    launchedRef.current = true;
    void launch();
  }, [launch, flow.needsStartUrl]);

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
        const url = flow.capture === "helper" ? extractDeviceVerificationUrl(outBufRef.current) : extractAuthUrl(outBufRef.current);
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
      // Helper-capture providers (AWS SSO): the credential is uploaded by the
      // in-sandbox helper through the brokered endpoint — it is NEVER printed, so
      // there is nothing to scrape. Watch only for the helper's success marker.
      if (flow.capture === "helper") {
        if (flow.doneMarker && outBufRef.current.includes(flow.doneMarker)) {
          savedRef.current = true;
          setAutoCaptured(true);
          setPhase("done");
          if (runId) void runsApi.killRun(runId).catch(() => {});
          onDone();
        }
        return;
      }
      const tok = extractSetupToken(outBufRef.current);
      if (tok) {
        setToken(tok);
        setAutoCaptured(true);
        void saveToken(tok);
      }
    },
    [saveToken, flow, runId, onDone],
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
        <span className="text-sm font-medium text-foreground">{flow.title}</span>
      </div>
      <p className="text-xs leading-relaxed text-muted-foreground">{flow.blurb}</p>

      {error && (
        <div className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning-subtle px-3 py-2 text-xs text-warning">
          <TriangleAlert className="mt-0.5 size-4 shrink-0" />
          <p>{error}</p>
        </div>
      )}

      {phase === "prompt" && (
        <div className="space-y-2">
          <label className="block text-xs font-medium text-foreground" htmlFor="harness-login-start-url">
            Your AWS access portal URL
          </label>
          <div className="flex flex-wrap items-center gap-2">
            <Input
              id="harness-login-start-url"
              value={startUrl}
              onChange={(e) => setStartUrl(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && isLikelyStartUrl(startUrl)) void launch();
              }}
              placeholder="https://my-org.awsapps.com/start"
              className="h-9 min-w-[18rem] flex-1 font-mono"
              aria-label="AWS access portal start URL"
            />
            <Button size="sm" onClick={() => void launch()} disabled={!isLikelyStartUrl(startUrl)}>
              <KeyRound className="size-3.5" /> Start login
            </Button>
            <Button size="sm" variant="outline" onClick={onCancel}>
              Cancel
            </Button>
          </div>
          <p className="text-xs text-muted-foreground">
            Find it in the AWS access portal (IAM Identity Center) — it looks like{" "}
            <code className="rounded bg-background/70 px-1 py-0.5 font-mono">https://my-org.awsapps.com/start</code>.
            Wardyn does not store it; the SSO region comes from the daemon&apos;s{" "}
            <code className="rounded bg-background/70 px-1 py-0.5 font-mono">-bedrock-aws-sso-region</code> /{" "}
            <code className="rounded bg-background/70 px-1 py-0.5 font-mono">-bedrock-region</code> setting.
          </p>
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
              <ExternalLink className="size-3.5" />{" "}
              {flow.capture === "helper" ? "Open the AWS verification page ↗" : "Open the Claude login page ↗"}
            </a>
          )}
          <AttachTerminal
            ref={termRef}
            runId={runId}
            autoRun={flow.cmd}
            onOutput={handleOutput}
            ptyCols={LOGIN_PTY_COLS}
          />
          {autoCaptured ? (
            <p className="flex items-center gap-2 text-xs text-success" data-testid="auto-capture-note">
              {phase === "saving" ? <Loader2 className="size-3.5 animate-spin" /> : <ShieldCheck className="size-3.5" />}
              {flow.capture === "helper"
                ? "SSO session captured — connecting…"
                : "Token detected — connecting your subscription…"}
            </p>
          ) : flow.capture === "helper" ? (
            /* Device-code flow: the code is entered on the AWS verification PAGE,
               not in the terminal, and the credential is uploaded by the in-sandbox
               helper — so there is no code field and nothing to paste here. */
            <div className="flex flex-wrap items-center gap-2">
              <p className="flex-1 text-xs leading-relaxed text-muted-foreground">
                Open the verification link above, enter the user code shown in the terminal, and approve. Wardyn
                captures the session automatically when the login completes.
              </p>
              <Button size="sm" variant="outline" onClick={cancel}>
                <Square className="size-3.5" /> Cancel
              </Button>
            </div>
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
