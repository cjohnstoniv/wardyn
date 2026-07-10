/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/**
 * AttachTerminal — live interactive terminal that attaches to a running
 * agent container via the Wardyn WebSocket endpoint:
 *
 *   GET /api/v1/runs/{id}/attach  (upgraded to WebSocket)
 *
 * Protocol (binary frame = raw PTY bytes, text frame = control message):
 *   - binary frames from server  → term.write()   (PTY output)
 *   - binary frames to server    ← xterm's onData  (PTY input)
 *   - text frame to server       ← JSON resize     {"type":"resize","cols":N,"rows":N}
 *
 * The server side runs a PERSISTENT tmux session per run, so detaching (tab
 * switch, refresh, drop) and re-attaching restores the same session.
 *
 * Auth note: the browser WebSocket API cannot set an Authorization header,
 * so the endpoint is authenticated via the OIDC session cookie that the
 * browser sends automatically (same-origin). If the UI is running in
 * admin-token-only mode (no OIDC session cookie, just a localStorage token)
 * we cannot inject the bearer token into the WebSocket handshake; in that
 * case we surface a clear inline message rather than failing silently.
 */
import * as React from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
// Bundle a real terminal font (same-origin, no external load). JetBrains Mono
// has proper box-drawing (─ │ ╭) and block-element (▐ ▛ █) glyphs at the right
// cell positions, so Claude Code's TUI renders cleanly instead of degrading to
// "__" the way the OS monospace fallback does.
import "@fontsource/jetbrains-mono/400.css";
import { getToken } from "../lib/api";
import { Loader2, TriangleAlert, Maximize2, Minimize2 } from "lucide-react";
import { cn } from "./ui/utils";

// ---------------------------------------------------------------------------
// Auth-mode detection
// ---------------------------------------------------------------------------
// api.ts stores the admin token in localStorage under this key.  When the
// token is present AND there is no valid OIDC session (we can't read
// HttpOnly cookies from JS, but we know the UI only uses a token when the
// OIDC flow is not active), we must warn the user.
function isAdminTokenOnlyMode(): boolean {
  return getToken() !== null;
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------
function buildWsUrl(runId: string): string {
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  const host = window.location.host; // same-origin → cookie is sent
  return `${proto}//${host}/api/v1/runs/${encodeURIComponent(runId)}/attach`;
}

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------
export interface AttachTerminalProps {
  runId: string;
  /** Called when the WebSocket closes (graceful or error) */
  onClose?: () => void;
}

type ConnState = "connecting" | "open" | "reconnecting" | "closed" | "error";

// Bounded reconnect/backoff. The server keeps a PERSISTENT tmux session per run,
// so an unexpected WebSocket drop (proxy hiccup, brief network blip, idle
// timeout) does NOT mean the session is gone — re-attaching restores it. We try
// a few times with exponential backoff before giving up. A CLEAN close (code
// 1000, e.g. component unmount or the run finishing) is intentional and is never
// retried.
const MAX_RECONNECT_ATTEMPTS = 4;
const RECONNECT_BASE_DELAY_MS = 600;
const RECONNECT_MAX_DELAY_MS = 5000;

export function AttachTerminal({ runId, onClose }: AttachTerminalProps) {
  const containerRef = React.useRef<HTMLDivElement>(null);
  const termRef = React.useRef<Terminal | null>(null);
  const fitAddonRef = React.useRef<FitAddon | null>(null);
  const wsRef = React.useRef<WebSocket | null>(null);
  const [connState, setConnState] = React.useState<ConnState>("connecting");
  const [errorMsg, setErrorMsg] = React.useState<string>("");
  const [fullscreen, setFullscreen] = React.useState(false);

  // Keep onClose in a ref so a fresh closure on every parent render does NOT
  // re-run the connect effect (which would tear down + reconnect the terminal
  // on every RunDetail re-render — flicker, lost scroll position).
  const onCloseRef = React.useRef(onClose);
  React.useEffect(() => {
    onCloseRef.current = onClose;
  }, [onClose]);

  // Warn early if we're in token-only mode; don't even attempt the WebSocket.
  const tokenOnlyMode = React.useMemo(() => isAdminTokenOnlyMode(), []);

  // Refit the terminal to its (current) container size and tell the PTY.
  const refit = React.useCallback(() => {
    const fit = fitAddonRef.current;
    const term = termRef.current;
    const ws = wsRef.current;
    if (!fit || !term) return;
    try {
      fit.fit();
    } catch {
      /* container not measurable yet — a later observer/raf will refit */
      return;
    }
    if (ws && ws.readyState === WebSocket.OPEN && term.cols > 0 && term.rows > 0) {
      ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
    }
  }, []);

  React.useEffect(() => {
    if (tokenOnlyMode) return; // nothing to tear down
    const mount = containerRef.current;
    if (!mount) return;

    // --- xterm setup --------------------------------------------------------
    const term = new Terminal({
      cursorBlink: true,
      scrollback: 50000,
      fontFamily: "'JetBrains Mono', ui-monospace, 'Cascadia Code', monospace",
      fontSize: 13,
      theme: {
        background: "#0d1117",
        foreground: "#e6edf3",
        cursor: "#58a6ff",
        selectionBackground: "#264f78",
        black: "#0d1117",
        red: "#ff7b72",
        green: "#3fb950",
        yellow: "#d29922",
        blue: "#58a6ff",
        magenta: "#bc8cff",
        cyan: "#39c5cf",
        white: "#b1bac4",
        brightBlack: "#6e7681",
        brightRed: "#ffa198",
        brightGreen: "#56d364",
        brightYellow: "#e3b341",
        brightBlue: "#79c0ff",
        brightMagenta: "#d2a8ff",
        brightCyan: "#56d4dd",
        brightWhite: "#f0f6fc",
      },
    });

    const fitAddon = new FitAddon();
    term.loadAddon(fitAddon);
    term.open(mount);
    termRef.current = term;
    fitAddonRef.current = fitAddon;

    // Initial fit after the browser has laid the container out.
    const rafId = requestAnimationFrame(() => refit());
    // Refit once the bundled font has loaded so xterm's cell metrics match the
    // real glyph width (a fit measured against the fallback font would misalign).
    document.fonts.ready.then(() => refit()).catch(() => {});

    // --- WebSocket (with bounded reconnect) ---------------------------------
    // The terminal/xterm instance above persists across reconnects; only the
    // socket is re-created. Input handlers read the *current* socket from
    // wsRef.current so they keep working after a reconnect swaps the socket.
    let reconnectAttempts = 0;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
    let disposed = false; // set in cleanup so a pending backoff never reconnects

    const send = (payload: ArrayBufferView | string) => {
      const cur = wsRef.current;
      if (cur && cur.readyState === WebSocket.OPEN) cur.send(payload);
    };

    const connect = () => {
      if (disposed) return;
      const ws = new WebSocket(buildWsUrl(runId));
      ws.binaryType = "arraybuffer";
      wsRef.current = ws;
      setConnState(reconnectAttempts > 0 ? "reconnecting" : "connecting");

      ws.onopen = () => {
        if (reconnectAttempts > 0) {
          term.writeln("\r\n\x1b[2m[reconnected]\x1b[0m");
        }
        reconnectAttempts = 0; // a successful attach resets the budget
        setConnState("open");
        // Fit + send the real size once the PTY is attached.
        requestAnimationFrame(() => refit());
      };

      ws.onmessage = (ev) => {
        if (ev.data instanceof ArrayBuffer) {
          term.write(new Uint8Array(ev.data));
        }
        // Ignore unexpected text frames (e.g. keepalive pings).
      };

      ws.onclose = (ev) => {
        if (disposed) return;
        // Clean, intentional close (1000) => the run finished / we unmounted.
        // Don't reconnect; persistent tmux re-attach is only for UNEXPECTED drops.
        if (ev.code === 1000) {
          setConnState("closed");
          term.writeln(
            `\r\n\x1b[2m[connection closed${ev.reason ? ": " + ev.reason : ""}]\x1b[0m`,
          );
          onCloseRef.current?.();
          return;
        }
        // Unexpected drop: re-attach to the persistent tmux session with backoff.
        if (reconnectAttempts < MAX_RECONNECT_ATTEMPTS) {
          reconnectAttempts += 1;
          const delay = Math.min(
            RECONNECT_MAX_DELAY_MS,
            RECONNECT_BASE_DELAY_MS * 2 ** (reconnectAttempts - 1),
          );
          setConnState("reconnecting");
          term.writeln(
            `\r\n\x1b[2m[connection lost — reconnecting (${reconnectAttempts}/${MAX_RECONNECT_ATTEMPTS})…]\x1b[0m`,
          );
          reconnectTimer = setTimeout(connect, delay);
          return;
        }
        // Budget exhausted: give up and surface the closed state.
        setConnState("closed");
        term.writeln(
          `\r\n\x1b[2m[connection closed after ${MAX_RECONNECT_ATTEMPTS} reconnect attempts]\x1b[0m`,
        );
        onCloseRef.current?.();
      };

      ws.onerror = () => {
        // An error is always followed by a close event; let onclose drive the
        // reconnect/backoff. Only surface a hard error banner once we've given
        // up (no attempts left), so a transient blip doesn't flash an error.
        if (reconnectAttempts >= MAX_RECONNECT_ATTEMPTS) {
          setConnState("error");
          setErrorMsg(
            "WebSocket connection failed. The run may have already stopped, or you may need to refresh your session.",
          );
        }
      };
    };

    connect();

    // Terminal input → WebSocket binary frame (reads the current socket).
    const inputDispose = term.onData((data) => {
      send(new TextEncoder().encode(data));
    });

    // Binary paste (e.g. via selection) → WebSocket binary frame.
    const binaryDispose = term.onBinary((data) => {
      send(Uint8Array.from(data, (c) => c.charCodeAt(0)));
    });

    // Shift+Enter / Ctrl+Enter → insert a newline instead of submitting. xterm
    // sends a plain CR for these (indistinguishable from Enter), so we send the
    // Alt+Enter sequence (ESC + CR) which Claude Code and similar TUIs treat as
    // "newline". (Plain Enter still submits; "\\" + Enter also works in Claude.)
    term.attachCustomKeyEventHandler((e) => {
      if (e.type === "keydown" && e.key === "Enter" && (e.shiftKey || e.ctrlKey)) {
        send(new TextEncoder().encode("\x1b\r"));
        return false; // don't let xterm also send a plain CR
      }
      return true;
    });

    // --- Resize wiring ------------------------------------------------------
    // Observe the terminal's own (flex-grown) box so any layout change — panel
    // resize, fullscreen toggle, window resize — refits and re-sizes the PTY.
    const resizeObserver = new ResizeObserver(() => refit());
    resizeObserver.observe(mount);
    const onWinResize = () => refit();
    window.addEventListener("resize", onWinResize);

    // --- Cleanup ------------------------------------------------------------
    return () => {
      // Stop any pending backoff from spawning a new socket after unmount, and
      // mark the close as intentional (so the in-flight ws.onclose won't retry).
      disposed = true;
      if (reconnectTimer) clearTimeout(reconnectTimer);
      cancelAnimationFrame(rafId);
      window.removeEventListener("resize", onWinResize);
      inputDispose.dispose();
      binaryDispose.dispose();
      resizeObserver.disconnect();
      const ws = wsRef.current;
      if (ws && (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING)) {
        ws.close(1000, "component unmounted");
      }
      term.dispose();
      termRef.current = null;
      fitAddonRef.current = null;
      wsRef.current = null;
    };
  }, [runId, tokenOnlyMode, refit]);

  // Refit shortly after entering/leaving fullscreen (the box just changed).
  React.useEffect(() => {
    const id = requestAnimationFrame(() => refit());
    const t = setTimeout(() => refit(), 60);
    return () => {
      cancelAnimationFrame(id);
      clearTimeout(t);
    };
  }, [fullscreen, refit]);

  // Token-only mode: can't inject auth into a WebSocket handshake.
  if (tokenOnlyMode) {
    return (
      <div className="flex items-start gap-3 rounded-lg border border-warning/40 bg-warning/10 p-4 text-sm text-warning">
        <TriangleAlert className="mt-0.5 size-4 shrink-0" />
        <div>
          <p className="font-medium">Interactive attach is coming soon in this deployment</p>
          <p className="mt-1 text-muted-foreground">
            The browser cannot send an Authorization header over a WebSocket connection, so
            interactive terminal access needs an OIDC (SSO) session — which is a coming-soon
            team feature. Run Wardyn in host mode (<code className="rounded bg-background/70 px-1 py-0.5">make setup</code>)
            for the full local experience, or use the CLI (<code className="rounded bg-background/70 px-1 py-0.5">wardyn attach</code>).
          </p>
        </div>
      </div>
    );
  }

  return (
    <div
      className={cn(
        "flex flex-col overflow-hidden border border-border bg-[#0d1117]",
        fullscreen ? "fixed inset-0 z-[100] rounded-none" : "h-[70vh] rounded-lg",
      )}
    >
      {/* title bar */}
      <div className="flex items-center gap-1.5 border-b border-border bg-card/60 px-3 py-2">
        <span className="size-3 rounded-full bg-[#ff5f56]" />
        <span className="size-3 rounded-full bg-[#ffbd2e]" />
        <span className="size-3 rounded-full bg-[#28c840]" />
        <span className="ml-3 truncate font-mono text-xs text-muted-foreground">
          {connState === "connecting" && "Connecting…"}
          {connState === "reconnecting" && "Reconnecting…"}
          {connState === "open" && `attach — ${runId}`}
          {connState === "closed" && `[closed] ${runId}`}
          {connState === "error" && `[error] ${runId}`}
        </span>
        <div className="ml-auto flex items-center gap-2">
          {(connState === "connecting" || connState === "reconnecting") && (
            <Loader2 className="size-3.5 animate-spin text-muted-foreground" />
          )}
          {connState === "open" && (
            <span className="inline-flex size-2 rounded-full bg-success" title="Connected" />
          )}
          <button
            type="button"
            onClick={() => setFullscreen((f) => !f)}
            title={fullscreen ? "Exit fullscreen" : "Fullscreen"}
            aria-label={fullscreen ? "Exit fullscreen" : "Fullscreen"}
            className="rounded p-1 text-muted-foreground hover:bg-muted hover:text-foreground"
          >
            {fullscreen ? <Minimize2 className="size-3.5" /> : <Maximize2 className="size-3.5" />}
          </button>
        </div>
      </div>

      {/* error state (before xterm renders) */}
      {connState === "error" && (
        <div className="flex items-start gap-3 p-4 text-sm text-danger">
          <TriangleAlert className="mt-0.5 size-4 shrink-0" />
          <p>{errorMsg}</p>
        </div>
      )}

      {/* xterm container — flex-grows to fill the panel / fullscreen viewport */}
      <div
        ref={containerRef}
        className="min-h-0 flex-1 p-1"
        // Keep clicks on the terminal from bubbling to the outer shell (focus).
        onMouseDown={(e) => e.stopPropagation()}
      />
    </div>
  );
}
