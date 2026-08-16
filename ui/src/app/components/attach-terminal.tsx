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
 *   - text frame from server     → JSON attach-mode {"type":"attach-mode","read_only":…}
 *
 * Attach is a SHARED tmux session, so a second client is admitted READ-ONLY and
 * the server says so in the attach-mode frame on EVERY connect (read_only=false
 * included) — see internal/api/attach_holder.go. This component never infers
 * its mode from silence, and a client the server DISPLACES (close 1008) must
 * not reconnect; both rules are implemented below with the reasoning inline.
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
// Bundle a real terminal font (same-origin, no external load) so the TUI gets
// true fixed-advance cells instead of whatever monospace the OS picks. Same
// latin/latin-ext subsets as styles/index.css — the other four subsets carry no
// glyph xterm draws (box-drawing U+2500.. and block elements U+2580.. are in
// NONE of the fontsource subsets; those already come from the OS fallback).
import "@fontsource/jetbrains-mono/latin-400.css";
import "@fontsource/jetbrains-mono/latin-ext-400.css";
import { getToken, HttpError } from "../lib/api/core";
import { runs } from "../lib/api/runs";
import type { AttachHolder, AttachModeMsg } from "../lib/types/runs";
import { getErrorMessage } from "../lib/format";
import { Eye, Loader2, TriangleAlert, Maximize2, Minimize2, RotateCw } from "lucide-react";
import { cn } from "./ui/utils";
import { Button } from "./ui/button";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "./ui/alert-dialog";
import { RUN_COCKPIT } from "./wardyn/copy";
import { useOperator, usePrincipal } from "./wardyn/operator-context";

// ---------------------------------------------------------------------------
// Auth-mode detection
// ---------------------------------------------------------------------------
// api.ts stores the admin token in localStorage under this key.  When the
// token is present AND there is no valid OIDC session (we can't read
// HttpOnly cookies from JS, but we know the UI only uses a token when the
// OIDC flow is not active), the WS handshake cannot carry the bearer — so we
// mint a single-use attach ticket via the normal authenticated REST surface
// and present it as ?ticket= instead.
function isAdminTokenOnlyMode(): boolean {
  return getToken() !== null;
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------
function buildWsUrl(runId: string, ticket?: string): string {
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  const host = window.location.host; // same-origin → cookie is sent
  const base = `${proto}//${host}/api/v1/runs/${encodeURIComponent(runId)}/attach`;
  return ticket ? `${base}?ticket=${encodeURIComponent(ticket)}` : base;
}

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------
export interface AttachTerminalProps {
  runId: string;
  /** Called when the WebSocket closes (graceful or error) */
  onClose?: () => void;
  /**
   * A command auto-typed into the PTY ONCE, shortly after the first successful
   * attach (e.g. "claude setup-token"). Reconnects do not re-type it. Purely a
   * convenience — it types exactly what the operator would; it grants no new
   * capability the interactive terminal didn't already have.
   */
  autoRun?: string;
  /**
   * Called with each decoded chunk of PTY output as it arrives — lets a parent
   * watch the stream (e.g. to detect a printed token). The raw bytes still go to
   * the terminal unchanged; this is an observer, not an interceptor.
   */
  onOutput?: (chunk: string) => void;
  /**
   * Force the terminal to this COLUMN count — both the PTY and the VISUAL xterm
   * grid — instead of fitting the container's width (rows still fit the
   * container; the pane scrolls horizontally). Used by the login flow so
   * `claude setup-token` prints the OAuth URL/token on single lines for the
   * stream extractors. The grids must MATCH: the CLI is a full-screen TUI that
   * cursor-addresses the grid it is told, so the old decoupled mode (wide PTY,
   * container-fit xterm) interleaved its redraw frames into garbage. Omit for a
   * normal interactive terminal (width follows the visible size).
   */
  ptyCols?: number;
  /** Non-fullscreen height (any Tailwind h-* class). The default suits a page
   *  panel; an embed inside a dialog wants something far shorter — a 70vh
   *  terminal inside an already-height-capped dialog is what blew the Add
   *  integration login out of its frame. */
  heightClass?: string;
  /**
   * Fill the parent instead of standing at a fixed height: `h-full min-h-0`,
   * for a mount inside a flex COLUMN that already owns the height (the run
   * page's cockpit). Overrides heightClass. Opt-in precisely because the
   * default must stay `h-[70vh]` for every other mount site — a dialog embed
   * passes its own shorter heightClass, and `h-full` inside a dialog with no
   * height of its own collapses the terminal to nothing.
   */
  fill?: boolean;
  /**
   * The run's creator (AgentRun.created_by). Compared against the signed-in
   * principal so a member can attach to a run THEY own — see the operator
   * gate below. Omit for a mount site with no run object (e.g. a fresh
   * interactive session the caller just created themselves): the gate then
   * falls back to operator-only, same as before this prop existed.
   */
  createdBy?: string;
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

// THE DISPLACEMENT CONTRACT (internal/api/attach_holder.go). A take-over closes
// the displaced client's socket with code 1008 (StatusPolicyViolation) and a
// reason of exactly `taken over by <principal>`. That close MUST NOT take the
// bounded-reconnect path above: the reconnect would land this browser straight
// back on the PTY as a competing client — the exact two-clients-fighting state
// the holder registry exists to end — and it would do it while the new holder
// is typing. We match on the code AND on the reason prefix (a proxy that
// rewrites the code still forwards the reason, and vice versa: either signal
// alone is enough to stop). Every OTHER close keeps today's behavior.
const TAKEN_OVER_CLOSE_CODE = 1008;
const TAKEN_OVER_REASON_PREFIX = "taken over by ";

export interface AttachTerminalHandle {
  /** Write text straight to the PTY stdin (e.g. a pasted code + "\r"). */
  sendText: (text: string) => void;
}

export const AttachTerminal = React.forwardRef<AttachTerminalHandle, AttachTerminalProps>(function AttachTerminal(
  { runId, onClose, autoRun, onOutput, ptyCols, heightClass = "h-[70vh]", fill, createdBy },
  ref,
) {
  // Attach is owner-or-admin, not operator-only: the WS's cookie lane is
  // admin-only (ticketOrHumanAuth's fall-through), but the ticket lane
  // (mintAttachTicket / getRunAuthorized, see http.go + attach_ticket.go) lets
  // a member hold a ticket for a run THEY created. Gating here, before either
  // lane is ever attempted, is the one chokepoint for every mount site (run
  // detail, the demo screen, …): no failed ticket POST, no WS handshake the
  // server would refuse anyway.
  const operator = useOperator();
  const principal = usePrincipal();
  const owned = !!createdBy && createdBy === principal;
  const containerRef = React.useRef<HTMLDivElement>(null);
  // The whole panel (title bar + grid) — the element handed to the native
  // Fullscreen API below.
  const panelRef = React.useRef<HTMLDivElement>(null);
  const termRef = React.useRef<Terminal | null>(null);
  const fitAddonRef = React.useRef<FitAddon | null>(null);
  const wsRef = React.useRef<WebSocket | null>(null);
  const [connState, setConnState] = React.useState<ConnState>("connecting");
  const [errorMsg, setErrorMsg] = React.useState<string>("");
  const [fullscreen, setFullscreen] = React.useState(false);
  // What the server told us this socket is (attach-mode frame). null = it has
  // not told us yet: the frame arrives on EVERY connect, so silence means "not
  // yet", never "you are driving" — an assumed-writer UI is how a spectator
  // ends up typing into someone else's session believing it works.
  const [mode, setMode] = React.useState<{ readOnly: boolean; holder?: AttachHolder } | null>(null);
  // Set when THIS client was displaced (close 1008). Carries the principal
  // parsed out of the close reason; empty string if a proxy dropped the reason
  // and only the code survived — we still know we were displaced, we just
  // cannot name who to. The socket is gone and we deliberately do not reopen it.
  const [takenOverBy, setTakenOverBy] = React.useState<string | null>(null);
  // Live grid, surfaced in the header (design board 2d's right-aligned meta).
  // Updated by refit, which is the one place that knows the real geometry.
  const [geom, setGeom] = React.useState<{ cols: number; rows: number } | null>(null);
  const [confirmTakeover, setConfirmTakeover] = React.useState(false);
  const [takeoverErr, setTakeoverErr] = React.useState("");

  // Keep onClose in a ref so a fresh closure on every parent render does NOT
  // re-run the connect effect (which would tear down + reconnect the terminal
  // on every RunDetail re-render — flicker, lost scroll position).
  const onCloseRef = React.useRef(onClose);
  React.useEffect(() => {
    onCloseRef.current = onClose;
  }, [onClose]);

  // Same ref pattern for autoRun/onOutput: a fresh closure each parent render
  // must NOT re-run the connect effect (that would tear down + reconnect the
  // terminal and re-type the command on every render).
  const autoRunRef = React.useRef(autoRun);
  React.useEffect(() => {
    autoRunRef.current = autoRun;
  }, [autoRun]);
  const onOutputRef = React.useRef(onOutput);
  React.useEffect(() => {
    onOutputRef.current = onOutput;
  }, [onOutput]);
  // Forced PTY width (login flow) — kept in a ref so refit (deps []) reads the
  // current value without re-running the connect effect.
  const ptyColsRef = React.useRef(ptyCols);
  React.useEffect(() => {
    ptyColsRef.current = ptyCols;
  }, [ptyCols]);

  // Imperative "type this into the PTY" — lets a parent bridge a normal input
  // field to the terminal's stdin (e.g. paste an OAuth code without needing the
  // terminal's Ctrl+Shift+V). Reads the CURRENT socket so it works post-reconnect.
  const sendToPty = React.useCallback((text: string) => {
    const ws = wsRef.current;
    if (ws && ws.readyState === WebSocket.OPEN) ws.send(new TextEncoder().encode(text));
  }, []);
  React.useImperativeHandle(ref, () => ({ sendText: sendToPty }), [sendToPty]);

  // Drop the current socket and attach again, KEEPING the xterm instance and
  // its scrollback. Assigned by the connect effect (which owns `connect` and
  // the reconnect budget) and called by the take-over flow — see doTakeover for
  // why a take-over needs a reconnect at all.
  const reclaimRef = React.useRef<() => void>(() => {});

  // Token-only mode routes the WS handshake through a minted attach ticket
  // (the browser cannot put the bearer on the handshake itself).
  const tokenOnlyMode = React.useMemo(() => isAdminTokenOnlyMode(), []);

  // Refit the terminal to its (current) container size and tell the PTY. With
  // ptyCols set, the visual grid is pinned to that width (rows still follow the
  // container) so the PTY and xterm always agree — see the ptyCols doc.
  // refit(force) — measure the container, resize the local grid, tell the PTY.
  //
  // `force` sends a ONE-COLUMN-SMALLER size first, then the real one. That looks
  // pointless and is not: the session is tmux, tmux clamps a shared window to
  // the SMALLEST attached client, and it re-evaluates on a client size CHANGE.
  // So when a second client (a `wardyn attach` from another terminal) attaches
  // small, the browser's grid fills with tmux's `·` filler — and when that
  // client leaves, the filler STAYS, because the browser's own size never
  // changed and a same-size resize frame is a no-op tmux ignores.
  //
  // Measured: 0 dots before a second client, 1001 while attached, still 1001
  // after it detached, and 0 again the moment the viewport actually changed
  // size. The nudge manufactures that change on demand.
  const refit = React.useCallback((force = false) => {
    const fit = fitAddonRef.current;
    const term = termRef.current;
    const ws = wsRef.current;
    if (!fit || !term) return;
    try {
      if (ptyColsRef.current) {
        const dims = fit.proposeDimensions();
        term.resize(ptyColsRef.current, dims && dims.rows > 0 ? dims.rows : term.rows);
      } else {
        fit.fit();
      }
    } catch {
      /* container not measurable yet — a later observer/raf will refit */
      return;
    }
    // Same-value guard: the ResizeObserver fires on layout changes that leave
    // the CELL grid identical, and a fresh object every time would re-render
    // the panel for nothing.
    setGeom((g) => (g && g.cols === term.cols && g.rows === term.rows ? g : { cols: term.cols, rows: term.rows }));
    if (ws && ws.readyState === WebSocket.OPEN && term.cols > 0 && term.rows > 0) {
      if (force && term.cols > 1) {
        ws.send(JSON.stringify({ type: "resize", cols: term.cols - 1, rows: term.rows }));
      }
      ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
    }
  }, []);

  React.useEffect(() => {
    // Fail-open default (operator-context.tsx) means this stays exactly
    // today's behavior — connects immediately — for every deployment that
    // never sets WARDYN_OIDC_OPERATOR_EMAILS. A confirmed non-operator who
    // does not own this run (createdBy unset or mismatched) skips straight to
    // the reason below, before creating a terminal or a socket.
    if (!operator && !owned) {
      setConnState("error");
      setErrorMsg("Attaching to a live sandbox requires the operator role or ownership of this run.");
      return;
    }

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

    // Streaming decoder for the output observer (a multibyte glyph split across
    // frames stays correct; our token of interest is ASCII regardless).
    const outDecoder = new TextDecoder();
    // autoRun fires once per mounted run, across reconnects.
    let autoRunSent = false;
    let autoRunTimer: ReturnType<typeof setTimeout> | null = null;

    const send = (payload: ArrayBufferView | string) => {
      const cur = wsRef.current;
      if (cur && cur.readyState === WebSocket.OPEN) cur.send(payload);
    };

    // Paste plumbing. Sandbox shells don't reliably unwrap xterm's bracketed-paste
    // markers (\x1b[200~ … \x1b[201~), which then show up as literal ^[[200~
    // garbage. So we OWN paste and send the clipboard text RAW. A native paste
    // event (Ctrl+Shift+V / right-click / Cmd+V) and a Ctrl+V keydown can BOTH
    // fire for one paste, so coalesce within a short window to avoid double-paste.
    let lastPasteAt = 0;
    const sendPaste = (text: string | null | undefined) => {
      if (!text) return;
      const now = performance.now();
      if (now - lastPasteAt < 120) return; // one paste, whichever path fired first
      lastPasteAt = now;
      send(new TextEncoder().encode(text));
    };

    const connect = () => {
      if (disposed) return;
      setConnState(reconnectAttempts > 0 ? "reconnecting" : "connecting");
      // Ticket lane needed for token-only mode (the bearer can't ride a WS
      // handshake) AND for a non-operator owner (the cookie lane is
      // admin-only server-side — ticketOrHumanAuth's fall-through, http.go);
      // an operator on a cookie session keeps using the cookie lane directly.
      if (tokenOnlyMode || !operator) {
        // Mint a fresh single-use ticket per (re)connect — the previous one was
        // consumed by the last handshake — then open the WS with ?ticket=.
        runs
          .attachTicket(runId)
          .then((ticket) => {
            if (disposed) return;
            openSocket(buildWsUrl(runId, ticket));
          })
          .catch((e: unknown) => {
            if (disposed) return;
            setConnState("error");
            setErrorMsg(
              `Could not mint an attach ticket: ${e instanceof Error ? e.message : String(e)}`,
            );
            onCloseRef.current?.();
          });
        return;
      }
      openSocket(buildWsUrl(runId));
    };

    const openSocket = (url: string) => {
      if (disposed) return;
      const ws = new WebSocket(url);
      ws.binaryType = "arraybuffer";
      wsRef.current = ws;

      ws.onopen = () => {
        if (reconnectAttempts > 0) {
          term.writeln("\r\n\x1b[2m[reconnected]\x1b[0m");
        }
        reconnectAttempts = 0; // a successful attach resets the budget
        setConnState("open");
        // Fit + send the real size once the PTY is attached. Forced, so an
        // attach that lands while another client has the window clamped starts
        // from this client's own size rather than inheriting the filler.
        requestAnimationFrame(() => refit(true));
        // Auto-type the convenience command ONCE, after the shell prompt has had
        // a moment to render. Guarded so a reconnect never re-types it.
        if (!autoRunSent && autoRunRef.current) {
          autoRunSent = true;
          const cmd = autoRunRef.current;
          autoRunTimer = setTimeout(() => {
            send(new TextEncoder().encode(cmd + "\r"));
          }, 900);
        }
      };

      ws.onmessage = (ev) => {
        if (ev.data instanceof ArrayBuffer) {
          const bytes = new Uint8Array(ev.data);
          term.write(bytes);
          // Mirror the decoded text to the optional observer (token detection).
          const cb = onOutputRef.current;
          if (cb) cb(outDecoder.decode(bytes, { stream: true }));
          return;
        }
        // TEXT frame = control JSON. Today the server sends exactly one, the
        // attach-mode frame, immediately on open (attach_holder.go). Anything
        // else — a keepalive, a frame from a newer daemon — is ignored exactly
        // as it was before this branch existed.
        if (typeof ev.data !== "string") return;
        try {
          const msg = JSON.parse(ev.data) as AttachModeMsg;
          if (msg?.type === "attach-mode") {
            setMode({ readOnly: !!msg.read_only, holder: msg.holder });
          }
        } catch {
          /* not JSON — nothing to do, same as before */
        }
      };

      ws.onclose = (ev) => {
        if (disposed) return;
        // DISPLACED — checked BEFORE the reconnect path, because it is the one
        // close that looks unexpected and must never be retried. See
        // TAKEN_OVER_CLOSE_CODE: reconnecting here re-claims the PTY on top of
        // the human who just took it, which is the whole bug this state exists
        // to prevent. No retry, no onClose() either — the parent should keep
        // this panel mounted so the operator can see what happened and take it
        // back, which is the only remaining action.
        const reason = ev.reason ?? "";
        if (ev.code === TAKEN_OVER_CLOSE_CODE || reason.startsWith(TAKEN_OVER_REASON_PREFIX)) {
          setTakenOverBy(reason.startsWith(TAKEN_OVER_REASON_PREFIX) ? reason.slice(TAKEN_OVER_REASON_PREFIX.length) : "");
          setMode(null); // we hold nothing now; the header must not still say "driving"
          setConnState("closed");
          term.writeln(`\r\n\x1b[2m[${reason || "taken over"} — not reconnecting]\x1b[0m`);
          return;
        }
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
          // Drop the stale attach mode, exactly as the taken-over branch does.
          // Whether we come back as writer or observer is the SERVER's call,
          // announced by the next attach-mode frame — carrying the old answer
          // across the gap asserts "you are driving" for a whole round trip on
          // a socket that currently holds nothing. That is inferring mode from
          // silence, which this protocol exists to avoid.
          setMode(null);
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

    // TAKE-OVER EVICTS, IT DOES NOT PROMOTE (handleAttachTakeover). After the
    // POST returns 200 the OLD holder's socket is closed, but OURS is still the
    // read-only one the server admitted — the server has no mid-stream "you may
    // now type" message, and inventing one on both ends buys nothing over the
    // reconnect this component already does perfectly. So: drop our socket and
    // attach again; the fresh attach registers as holder. Between the eviction
    // and that attach the holder endpoint honestly reports held:false.
    reclaimRef.current = () => {
      if (disposed) return;
      const cur = wsRef.current;
      if (cur) {
        // Detach the handlers first: this close is OUR teardown, not a drop,
        // and must not run the reconnect/closed bookkeeping on its way out.
        cur.onclose = null;
        cur.onerror = null;
        cur.onmessage = null;
        try {
          cur.close(1000, "reclaiming the writer slot");
        } catch {
          /* already closing — connect() below is what matters */
        }
      }
      wsRef.current = null;
      reconnectAttempts = 0; // a deliberate re-attach starts from a full budget
      connect();
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
      // Ctrl+V / Cmd+V (NOT Ctrl+Shift+V): read the clipboard and paste RAW.
      // Otherwise xterm sends a literal ^V and never pastes. Ctrl+Shift+V /
      // right-click go through the native paste event below; sendPaste coalesces
      // so a key + event pair never double-pastes.
      if (e.type === "keydown" && (e.ctrlKey || e.metaKey) && !e.shiftKey && (e.key === "v" || e.key === "V")) {
        navigator.clipboard?.readText?.().then(sendPaste).catch(() => {});
        return false;
      }
      return true;
    });

    // Native paste (Ctrl+Shift+V, right-click, Cmd+V) → send RAW and STOP xterm's
    // own (bracketed) paste from also firing. Capture phase so this wins over
    // xterm's textarea listener.
    const onPaste = (e: ClipboardEvent) => {
      const text = e.clipboardData?.getData("text");
      if (text == null) return;
      e.preventDefault();
      e.stopPropagation();
      sendPaste(text);
    };
    mount.addEventListener("paste", onPaste, true);

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
      if (autoRunTimer) clearTimeout(autoRunTimer);
      cancelAnimationFrame(rafId);
      window.removeEventListener("resize", onWinResize);
      mount.removeEventListener("paste", onPaste, true);
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
    // operator/owned are added deliberately: in the single-operator/default
    // case they never change value, so this never causes an extra run there —
    // today's behavior is untouched. They matter for the (rare) case where
    // /me resolves to a non-owning viewer shortly after an optimistic mount;
    // the early return above then tears the effect back down via its own
    // cleanup before running again.
  }, [runId, tokenOnlyMode, refit, operator, owned]);

  // Refit shortly after entering/leaving fullscreen (the box just changed).
  React.useEffect(() => {
    const id = requestAnimationFrame(() => refit());
    const t = setTimeout(() => refit(), 60);
    return () => {
      cancelAnimationFrame(id);
      clearTimeout(t);
    };
  }, [fullscreen, refit]);

  // Fullscreen uses the NATIVE Fullscreen API, not a `fixed inset-0` overlay.
  //
  // The CSS approach cannot be made reliable: `position: fixed` is resolved
  // against the nearest ancestor that establishes a containing block, and the
  // list of things that do is long and growing — transform, filter,
  // backdrop-filter, perspective, contain, will-change, and (Tailwind v4's
  // default for translate-x-*) the INDIVIDUAL `translate` property, which
  // `transform: none` does not reset. Measured on the login dialog: computed
  // `transform: none` yet `translate: -50% -50%`, and a `fixed inset-0` child
  // still sized to the dialog rather than the viewport.
  //
  // requestFullscreen promotes the element to the browser's TOP LAYER, which
  // sits outside the whole containing-block question, so this works identically
  // inside a dialog, a card, or a page. It also gives real fullscreen — over the
  // browser chrome, not just the page — and the browser handles Escape itself
  // (WCAG 2.1.2), so there is no key handler to fight xterm's textarea for.
  const toggleFullscreen = React.useCallback(() => {
    const el = panelRef.current;
    if (!el) return;
    if (document.fullscreenElement === el) {
      void document.exitFullscreen().catch(() => {});
      return;
    }
    const req = el.requestFullscreen?.bind(el);
    if (!req) {
      // No API (very old browser, or a sandboxed iframe without
      // allow-fullscreen): fall back to the in-page overlay. It is still
      // subject to the containing-block rules above, so it may only fill an
      // ancestor — degraded, never broken.
      setFullscreen((f) => !f);
      return;
    }
    void req().catch(() => setFullscreen((f) => !f));
  }, []);

  // The browser owns the truth: Escape, F11 and the OS window chrome can all
  // leave fullscreen without going through our button.
  React.useEffect(() => {
    const sync = () => setFullscreen(document.fullscreenElement === panelRef.current);
    document.addEventListener("fullscreenchange", sync);
    return () => document.removeEventListener("fullscreenchange", sync);
  }, []);

  // Escape exits the FALLBACK overlay (WCAG 2.1.2, no keyboard trap). Native
  // fullscreen needs no help — the browser exits on Escape before the page sees
  // the key — so this only binds when we are overlaying rather than promoted.
  // Capture phase, so it runs before xterm's textarea swallows the key and
  // sends it to the PTY as literal input.
  React.useEffect(() => {
    if (!fullscreen || document.fullscreenElement) return;
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape" && !e.ctrlKey && !e.metaKey && !e.shiftKey && !e.altKey) {
        e.preventDefault();
        e.stopPropagation();
        setFullscreen(false);
      }
    };
    document.addEventListener("keydown", onKeyDown, true);
    return () => document.removeEventListener("keydown", onKeyDown, true);
  }, [fullscreen]);

  // --- Holder / take-over ---------------------------------------------------
  // Spectator: the server admitted us read-only because someone else holds the
  // PTY. Our keystrokes and resize frames are dropped SERVER-side (attachPump),
  // so this is purely about saying so — the UI never had words for a state the
  // console has always been able to reach.
  const readOnly = mode?.readOnly === true;
  const displaced = takenOverBy !== null;
  // Who we would evict. Empty when the server omitted the holder or a proxy ate
  // the close reason: we then cannot name them, so we do not offer to end
  // "their" session — a confirm that cannot say whose session it ends is worse
  // than no button.
  const holderPrincipal = (displaced ? takenOverBy : mode?.holder?.principal) ?? "";

  const doTakeover = React.useCallback(async () => {
    setConfirmTakeover(false);
    setTakeoverErr("");
    try {
      await runs.takeoverAttach(runId);
    } catch (e) {
      // A 409 means the server says NOBODY holds it — the holder left while we
      // sat here as an observer, and nothing told us: the attach-mode frame is
      // sent once, at connect. Without this branch the panel was a dead end,
      // because the whole page's hero is a terminal permanently convinced it is
      // read-only, and the only way out was a full reload. There IS nothing to
      // take over, so reclaiming is the correct response to that answer, not an
      // error to display.
      if (e instanceof HttpError && e.status === 409) {
        setTakenOverBy(null);
        setMode(null);
        reclaimRef.current();
        return;
      }
      setTakeoverErr(getErrorMessage(e));
      return;
    }
    setTakenOverBy(null);
    setMode(null);
    reclaimRef.current(); // evict-then-reconnect; see reclaimRef's assignment
  }, [runId]);

  // Mounting note: this panel is safe to embed anywhere — a page, a card, a
  // dialog — because fullscreen goes through the native API (see
  // toggleFullscreen) rather than a `fixed inset-0` overlay that any ancestor
  // could capture. Portaling on toggle would NOT have been safe: the xterm setup
  // effect is keyed on [runId, tokenOnlyMode, refit, operator, owned] and not on
  // fullscreen, so React would rebuild this container under the new parent
  // without re-running term.open() and leave a permanently blank terminal.
  return (
    <div
      ref={panelRef}
      className={cn(
        "flex flex-col overflow-hidden border border-border bg-[#0d1117]",
        // In native fullscreen the element already fills the screen, so it only
        // needs to drop its rounding and its fixed height. The `fixed inset-0`
        // branch is the no-API fallback described above.
        fullscreen ? "h-full w-full rounded-none" : `${fill ? "h-full min-h-0" : heightClass} rounded-lg`,
        fullscreen && !document.fullscreenElement && "fixed inset-0 z-[100]",
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
        {/* State chip (design board 2d). Only ever rendered from what the
            SERVER said: "driving" needs an attach-mode frame with
            read_only=false, so a daemon that never sends one shows no chip
            rather than a claim we cannot back. */}
        {(readOnly || displaced) && holderPrincipal && (
          <span className="inline-flex shrink-0 items-center gap-1 rounded border border-info/25 bg-info-subtle px-1.5 py-0.5 font-mono text-[11px] text-info">
            <Eye className="size-3" />
            {RUN_COCKPIT.heldBy(holderPrincipal)}
          </span>
        )}
        {!readOnly && !displaced && mode && connState === "open" && (
          <span className="inline-flex shrink-0 items-center gap-1 rounded border border-success/25 bg-success-subtle px-1.5 py-0.5 font-mono text-[11px] text-success">
            <span className="size-1.5 rounded-full bg-current" />
            {RUN_COCKPIT.driving}
          </span>
        )}
        <div className="ml-auto flex items-center gap-2">
          {/* Live geometry — the grid this client actually has, post-refit. */}
          {geom && (
            <span className="font-mono text-[11px] text-muted-foreground">
              {geom.cols}×{geom.rows}
            </span>
          )}
          {(connState === "connecting" || connState === "reconnecting") && (
            <Loader2 className="size-3.5 animate-spin text-muted-foreground" />
          )}
          {connState === "open" && (
            <span className="inline-flex size-2 rounded-full bg-success" title="Connected" />
          )}
          {/* Redraw. The browser cannot observe another client detaching, so it
              cannot know the tmux window is still clamped to a size that left —
              see refit's note. One click forces the size change that clears it. */}
          <button
            type="button"
            onClick={() => refit(true)}
            title="Redraw (fixes a terminal left clamped by another attached client)"
            aria-label="Redraw terminal"
            className="rounded p-1 text-muted-foreground hover:bg-muted hover:text-foreground"
          >
            <RotateCw className="size-3.5" />
          </button>
          <button
            type="button"
            onClick={toggleFullscreen}
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

      {/* xterm container — flex-grows to fill the panel / fullscreen viewport.
          A pinned-width grid (ptyCols) renders wider than the pane; scroll it
          horizontally rather than clipping the right edge. The wrapper exists
          so the read-only badge can be positioned over the grid without being
          a CHILD of the element xterm owns. */}
      <div className="relative flex min-h-0 flex-1 flex-col">
        <div
          ref={containerRef}
          className={cn("min-h-0 flex-1 p-1", ptyCols && "overflow-x-auto")}
          // Keep clicks on the terminal from bubbling to the outer shell (focus).
          onMouseDown={(e) => e.stopPropagation()}
        />
        {readOnly && (
          // pointer-events-none: this is a label, not a shield. The input it
          // describes is dropped SERVER-side; blocking clicks here would also
          // block selecting and copying the output, which a spectator can do.
          <div className="pointer-events-none absolute bottom-3 left-3 inline-flex items-center gap-2 rounded-lg border border-info/35 bg-info/15 px-2.5 py-1.5">
            <Eye className="size-3.5 text-info" />
            <span className="font-mono text-[11px] text-info">{RUN_COCKPIT.watchingReadOnly}</span>
          </div>
        )}
      </div>

      {/* Holder footer — only in the two states that have an action. A driving
          terminal keeps its existing chrome (every dialog embed depends on the
          panel not growing a footer it did not budget height for). */}
      {(readOnly || displaced) && (
        <div className="flex items-center gap-2 border-t border-border bg-card/60 px-3 py-2">
          {/* The server's own words on failure (e.g. the 409 for taking over
              nothing), in place of the hint — there is no second line to lose. */}
          {/* Two different facts, two different sentences: arriving second
              ("someone is already driving") is not the same as having been
              displaced mid-session, and heldHint reads as a mild lie in the
              second case. displacedHint also says the session survived, which
              is the thing a just-kicked operator actually needs to know. */}
          <p className={cn("min-w-0 flex-1 text-xs", takeoverErr ? "text-danger" : "text-muted-foreground")}>
            {takeoverErr ||
              (displaced
                ? holderPrincipal
                  ? RUN_COCKPIT.displacedHint(holderPrincipal)
                  : // Displaced, but a proxy ate the close reason so there is no
                    // principal to name. heldHint would offer to "watch it live"
                    // and "take it from them" — the socket is closed and there is
                    // no them, so both halves would be false.
                    RUN_COCKPIT.displacedUnknownHint
                : RUN_COCKPIT.heldHint)}
          </p>
          {holderPrincipal && (
            <Button variant="outline" size="sm" onClick={() => setConfirmTakeover(true)}>
              {RUN_COCKPIT.takeOver}
            </Button>
          )}
        </div>
      )}

      {/* Take-over ends another human's live session, so it gets the same
          confirm stop as the irreversible deny in live-approvals.tsx. */}
      <AlertDialog open={confirmTakeover} onOpenChange={(o) => !o && setConfirmTakeover(false)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{RUN_COCKPIT.takeOver}</AlertDialogTitle>
            <AlertDialogDescription>{RUN_COCKPIT.takeOverConfirm(holderPrincipal)}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={(e) => {
                e.preventDefault();
                void doTakeover();
              }}
            >
              {RUN_COCKPIT.takeOver}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
});
