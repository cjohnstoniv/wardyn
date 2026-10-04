/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/**
 * useAttachSession drives AttachTerminal's socket/ticket-mint/credential/xterm
 * lifecycle. Split out of attach-terminal.tsx purely to keep that file under
 * its 1000-line cap (scripts/check-file-size.sh, split-by-seam, never
 * allowlist) — this hook has exactly one caller and is not meant to grow a
 * second.
 */
import * as React from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { Unicode11Addon } from "@xterm/addon-unicode11";
import { WebLinksAddon } from "@xterm/addon-web-links";
import { decideKey } from "./attach-terminal-keys";
import { terminalLinkHandlers } from "./attach-terminal-links";
import { exposeTerminalForE2E } from "./attach-terminal-e2e-seam";
import { createRenderer, type RendererControl, type RendererPref, type RendererState } from "./attach-terminal-renderer";
import { createCopyGate, type CopyGate, type CopyOffer, type GateTerm } from "./attach-terminal-clipboard";
import { runs } from "../lib/api/runs";
import { wsURL } from "../lib/base-path";
import { entryErrorMessage } from "../lib/run-entry";
import type { AttachHolder, AttachModeMsg } from "../lib/types/runs";

export type ConnState = "connecting" | "open" | "reconnecting" | "closed" | "error";

// Bounded reconnect/backoff. The server keeps a PERSISTENT tmux session per run,
// so an unexpected WebSocket drop (proxy hiccup, brief network blip, idle
// timeout) does NOT mean the session is gone — re-attaching restores it. We try
// a few times with exponential backoff before giving up. A CLEAN close (code
// 1000, e.g. component unmount or the run finishing) is intentional and is never
// retried.
export const MAX_RECONNECT_ATTEMPTS = 4;
const RECONNECT_BASE_DELAY_MS = 600;
const RECONNECT_MAX_DELAY_MS = 5000;

// ponytail: a handshake that neither completes nor closes (R4-F143). Every
// state here is driven off the socket's open/close/error events, and a
// WebSocket upgrade can do none of the three — the deploy README's own
// warning, "an ingress controller in front of it must not buffer or strip
// the 101 upgrade" (deploy/helm/wardyn/README.md), names exactly a proxy
// that accepts the TCP connection and then sits on it. Measured in Chromium:
// "Connecting…" with a spinner, no message, indefinitely. This deadline
// turns that silence into a failed attempt for the reconnect budget below,
// so the panel reaches the same honest closed state a refused socket does.
// Longer than any healthy upgrade (one round trip), shorter than a human's
// patience.
const CONNECT_TIMEOUT_MS = 15_000;

// The displacement contract (internal/api/attach_holder.go): a take-over
// closes the displaced client's socket with code 1008 (StatusPolicyViolation)
// and a reason of exactly `taken over by <principal>`. That close must not
// take the bounded-reconnect path above: the reconnect would land this
// browser straight back on the PTY as a competing client — the exact
// two-clients-fighting state the holder registry exists to end — and it
// would do it while the new holder is typing. We match on the code and on
// the reason prefix (a proxy that rewrites the code still forwards the
// reason, and vice versa: either signal alone is enough to stop). Every
// other close keeps the reconnect path.
const TAKEN_OVER_CLOSE_CODE = 1008;
const TAKEN_OVER_REASON_PREFIX = "taken over by ";

// Helpers
function buildWsUrl(runId: string, ticket?: string, cols?: number, rows?: number): string {
  const q = new URLSearchParams();
  if (ticket) q.set("ticket", ticket);
  if (cols && rows && cols > 0 && rows > 0) {
    q.set("cols", String(cols));
    q.set("rows", String(rows));
  }
  const base = wsURL(`/runs/${encodeURIComponent(runId)}/attach`);
  const qs = q.toString();
  return qs ? `${base}?${qs}` : base;
}

export interface UseAttachSessionArgs {
  runId: string;
  signedOut: boolean;
  operator: boolean;
  operatorResolved: boolean;
  /** The one entry rule (lib/run-entry.ts), decided by the caller. */
  mayEnter: boolean;
  /** What to say when it is false. */
  refusal: string;
  tokenOnlyMode: boolean;
  containerRef: React.RefObject<HTMLDivElement | null>;
  panelRef: React.RefObject<HTMLDivElement | null>;
  termRef: React.MutableRefObject<Terminal | null>;
  fitAddonRef: React.MutableRefObject<FitAddon | null>;
  wsRef: React.MutableRefObject<WebSocket | null>;
  onCloseRef: React.RefObject<(() => void) | undefined>;
  autoRunRef: React.RefObject<string | undefined>;
  onOutputRef: React.RefObject<((chunk: string) => void) | undefined>;
  reclaimRef: React.MutableRefObject<() => void>;
  manualReconnectRef: React.MutableRefObject<() => void>;
  refit: (force?: boolean) => void;
  setConnState: React.Dispatch<React.SetStateAction<ConnState>>;
  setErrorMsg: React.Dispatch<React.SetStateAction<string>>;
  setMode: React.Dispatch<React.SetStateAction<{ readOnly: boolean; holder?: AttachHolder } | null>>;
  setTakenOverBy: React.Dispatch<React.SetStateAction<string | null>>;
  setReconnectAttempt: React.Dispatch<React.SetStateAction<number>>;
  setReconnectExhausted: React.Dispatch<React.SetStateAction<boolean>>;
  /** The clipboard gate (attach-terminal-clipboard.ts): its offer and notice state, and its handle. */
  setCopyOffer: React.Dispatch<React.SetStateAction<CopyOffer | null>>;
  setCopyNotice: React.Dispatch<React.SetStateAction<string | null>>;
  copyGateRef: React.MutableRefObject<CopyGate | null>;
  /** A clicked link that needs the confirm dialog (attach-terminal-links.ts). */
  setLinkTarget: React.Dispatch<React.SetStateAction<URL | null>>;
  /** The renderer (attach-terminal-renderer.ts): its live handle, the stored choice, and what is in use. */
  rendererRef: React.MutableRefObject<RendererControl | null>;
  rendererPrefRef: React.RefObject<RendererPref>;
  setRenderer: React.Dispatch<React.SetStateAction<RendererState>>;
}

export function useAttachSession(args: UseAttachSessionArgs) {
  const {
    runId,
    signedOut,
    operator,
    operatorResolved,
    mayEnter,
    refusal,
    tokenOnlyMode,
    containerRef,
    panelRef,
    termRef,
    fitAddonRef,
    wsRef,
    onCloseRef,
    autoRunRef,
    onOutputRef,
    reclaimRef,
    manualReconnectRef,
    refit,
    setConnState,
    setErrorMsg,
    setMode,
    setTakenOverBy,
    setReconnectAttempt,
    setReconnectExhausted,
    setCopyOffer,
    setCopyNotice,
    copyGateRef,
    setLinkTarget,
    rendererRef,
    rendererPrefRef,
    setRenderer,
  } = args;

  // Read at each (re)connect, not keyed on: /me landing late must not rebuild
  // the terminal (one socket and one ticket per page load). Teardown on a
  // changed authorisation still rides `mayEnter` and `signedOut` below.
  const operatorRef = React.useRef(operator);
  const operatorResolvedRef = React.useRef(operatorResolved);
  operatorRef.current = operator;
  operatorResolvedRef.current = operatorResolved;

  React.useEffect(() => {
    // Fail-open default (operator-context.tsx) means this stays exactly
    // today's behavior — connects immediately — for every deployment that
    // never sets WARDYN_OIDC_OPERATOR_EMAILS. A confirmed non-operator on a run
    // whose stated creator is somebody else skips straight to the reason below,
    // before creating a terminal or a socket.
    if (signedOut || !mayEnter) {
      setConnState(signedOut ? "closed" : "error");
      setErrorMsg(signedOut ? "" : refusal);
      return;
    }

    const mount = containerRef.current;
    if (!mount) return;

    // xterm setup
    const links = terminalLinkHandlers(setLinkTarget);
    const term = new Terminal({
      cursorBlink: true,
      scrollback: 50000,
      // The unicode addon uses xterm's proposed API.
      allowProposedApi: true,
      // Box-drawing and block glyphs are drawn exactly, not taken from the font.
      customGlyphs: true,
      // Option+drag keeps xterm's native selection on macOS while tmux owns the mouse.
      macOptionClickForcesSelection: true,
      // OSC 8 links share the detected-link policy.
      linkHandler: links.osc8,
      fontFamily: "'JetBrains Mono Terminal', 'JetBrains Mono', ui-monospace, 'Cascadia Code', monospace",
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

    let disposedFont = false;
    const fitAddon = new FitAddon();
    term.loadAddon(fitAddon);
    // Unicode 11 widths match tmux and glibc (emoji take two cells); the default table is Unicode 6.
    term.loadAddon(new Unicode11Addon());
    term.unicode.activeVersion = "11";
    term.loadAddon(new WebLinksAddon(links.detected));
    term.open(mount);
    termRef.current = term;
    // After open: the GPU addon needs the DOM renderer in place to fall back to.
    const renderer = createRenderer(term, rendererPrefRef.current ?? "auto", setRenderer);
    rendererRef.current = renderer;
    const unexpose = exposeTerminalForE2E(term);
    fitAddonRef.current = fitAddon;
    // Measure now so the attach URL carries the real geometry.
    refit();

    // Initial fit after the browser has laid the container out.
    const rafId = requestAnimationFrame(() => refit());
    // The web font is font-display: swap, so xterm may have measured cells
    // against the fallback (17px vs 15px). fit() resizes only when the grid
    // dimensions change, so once the font is in, nudge cols-1 to make the next
    // refit a real change and re-measure the cells.
    document.fonts
      .load("13px 'JetBrains Mono Terminal'")
      .then(() => {
        if (disposedFont || termRef.current !== term) return;
        if (term.cols > 1) term.resize(term.cols - 1, term.rows);
        refit();
      })
      .catch(() => {});

    // WebSocket (with bounded reconnect)
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
    let connectTimer: ReturnType<typeof setTimeout> | null = null;
    // An observer promoted in place (read_only true→false on
    // the same socket, no reconnect) inherits the departed holder's tmux
    // geometry — the server skips handshake geometry for a non-writer and
    // drops an observer's resize frames, so refit() never ran for this
    // client's real size. null on the first frame so an initial writer
    // connect (never "was read-only") does not spuriously force-refit.
    let lastReadOnly: boolean | null = null;
    // The socket the server last announced as the WRITER, and only that one: an
    // observer, or the gap between sockets, is never entitled to a copy offer.
    let writerWs: WebSocket | null = null;
    const copyGate = createCopyGate({
      term: term as unknown as GateTerm,
      isWriter: () => writerWs !== null && writerWs === wsRef.current && writerWs.readyState === WebSocket.OPEN,
      onOffer: setCopyOffer,
      onNotice: setCopyNotice,
    });
    copyGateRef.current = copyGate;

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
      //
      // ...and ALSO whenever the admin role is not POSITIVELY known
      // (R4-F110). `operator` is fail-open by design, so one swallowed /me
      // failure (health.ts's whoami returns null on any 4xx/5xx) leaves a
      // MEMBER reading as an operator with principal "unknown" — `owned` false,
      // `operator` true — and this branch then picked the ADMIN-ONLY cookie
      // lane for a member who owns the run. The server refuses it 403 and
      // writes an authz.denied/admin_surface row against the legitimate owner,
      // once per reconnect attempt, while the ticket lane that would have
      // worked was never tried. Preferring the ticket lane when unsure costs an
      // admin nothing: minting is itself owner-or-admin (handleAttachTicket,
      // attach_ticket.go), so the lane serves both.
      if (tokenOnlyMode || !operatorRef.current || !operatorResolvedRef.current) {
        // Mint a fresh single-use ticket per (re)connect — the previous one was
        // consumed by the last handshake — then open the WS with ?ticket=.
        runs
          .attachTicket(runId)
          .then((ticket) => {
            if (disposed) return;
            openSocket(buildWsUrl(runId, ticket, term.cols, term.rows));
          })
          .catch((e: unknown) => {
            if (disposed) return;
            setConnState("error");
            setErrorMsg(
              entryErrorMessage(e, (err) => `Could not mint an attach ticket: ${err instanceof Error ? err.message : String(err)}`),
            );
            onCloseRef.current?.();
          });
        return;
      }
      openSocket(buildWsUrl(runId, undefined, term.cols, term.rows));
    };

    const openSocket = (url: string) => {
      if (disposed) return;
      const ws = new WebSocket(url);
      ws.binaryType = "arraybuffer";
      wsRef.current = ws;
      // A new connection starts with no writer standing and no offer.
      writerWs = null;
      copyGate.reset();

      // Arm the deadline for THIS attempt. Closing a socket still in CONNECTING
      // fires onclose with an abnormal code — the one path that already knows
      // backoff, the attempt budget and the closed state, so a stalled upgrade
      // needs no second failure vocabulary of its own.
      if (connectTimer) clearTimeout(connectTimer);
      connectTimer = setTimeout(() => {
        connectTimer = null;
        if (disposed || wsRef.current !== ws) return;
        if (ws.readyState !== WebSocket.CONNECTING) return;
        try {
          ws.close();
        } catch {
          /* already closing — onclose still runs the bookkeeping */
        }
      }, CONNECT_TIMEOUT_MS);

      const clearConnectTimer = () => {
        if (connectTimer) {
          clearTimeout(connectTimer);
          connectTimer = null;
        }
      };

      ws.onopen = () => {
        clearConnectTimer();
        // #216 — was `term.writeln([reconnected])`; the open dot below says so.
        reconnectAttempts = 0; // a successful attach resets the budget
        setReconnectAttempt(0);
        setReconnectExhausted(false);
        setConnState("open");
        // Fit + send the real size once the PTY is attached (deduped per socket).
        requestAnimationFrame(() => refit());
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
        // TEXT frame = control JSON: the attach-mode frame, sent on open
        // (attach_holder.go) and again mid-stream, read_only:false, when the
        // server promotes this socket in place. Anything else — a keepalive, a
        // frame from a newer daemon — is ignored, as before this branch existed.
        if (typeof ev.data !== "string") return;
        try {
          const msg = JSON.parse(ev.data) as AttachModeMsg;
          if (msg?.type === "attach-mode") {
            const nowReadOnly = !!msg.read_only;
            if (lastReadOnly === true && !nowReadOnly) {
              // Promoted in place: force the resize nudge (refit's own doc)
              // so THIS client's size wins over the geometry it inherited.
              // Called directly, not deferred to a rAF like the connect-time
              // refit below — the terminal is already mounted and measured by
              // the time a mode change can arrive, same as the Redraw button's
              // own direct call.
              refit(true);
            }
            if (!nowReadOnly) {
              // D3: a writable socket opening (initial connect, reconnect, or
              // a promotion) is exactly when an operator expects to be able to
              // type — do not make them click first.
              term.focus();
            }
            lastReadOnly = nowReadOnly;
            // A role change voids whatever was offered under the old one.
            if ((writerWs === ws) !== !nowReadOnly) copyGate.reset();
            writerWs = nowReadOnly ? null : ws;
            setMode({ readOnly: nowReadOnly, holder: msg.holder });
          }
        } catch {
          /* not JSON — nothing to do, same as before */
        }
      };

      ws.onclose = (ev) => {
        clearConnectTimer();
        writerWs = null;
        copyGate.reset();
        if (disposed) return;
        // Displaced — checked before the reconnect path, because it is the one
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
          // #216 — was a writeln; the holder footer's displacedHint says this.
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
          // #216 — was a writeln; TerminalConnectionStatus below the grid
          // renders the same count, outside the buffer.
          setReconnectAttempt(reconnectAttempts);
          reconnectTimer = setTimeout(connect, delay);
          return;
        }
        // Budget exhausted: give up and surface the closed state.
        // #216 — was a writeln; TerminalConnectionStatus below now carries it,
        // with Reconnect, instead of scrolling out of view.
        setReconnectExhausted(true);
        setConnState("closed");
        onCloseRef.current?.();
      };

      ws.onerror = () => {
        clearConnectTimer();
        // An error is always followed by a close event, and onclose's own
        // "budget exhausted" arm unconditionally sets "closed" right after —
        // so a setConnState("error") here is dead, never observable (R4-F143
        // — deliberate, untouched). "error" is reached only from the
        // caller's own refusal to attach at all.
      };
    };

    // Unless our own open socket was just promoted in place (doTakeover skips
    // this then), a take-over freed the slot rather than promoting us into it
    // (handleAttachTakeover). After the POST returns 200 the old holder's
    // socket is closed, but ours is still the read-only one the server
    // admitted (or we had none) — so: drop our socket and attach again; the
    // fresh attach registers as holder. Between the eviction and that attach
    // the holder endpoint honestly reports held:false.
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

    // #216 — Reconnect: the spent socket is already closed (the browser did
    // that), so just reset the count and attach again, like a fresh mount.
    manualReconnectRef.current = () => {
      if (disposed) return;
      reconnectAttempts = 0;
      setReconnectAttempt(0);
      setReconnectExhausted(false);
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
      // R4-F144 (WCAG 2.1.2 No Keyboard Trap) + #133 (chord must be typeable
      // on every layout): decideKey (attach-terminal-keys.ts, table-tested)
      // carries the why. `false` below stops xterm from ALSO forwarding the
      // key — an escape must not also type, and a newline must not also CR.
      if (e.type !== "keydown") return true;
      switch (decideKey(e)) {
        case "escape":
          panelRef.current?.focus(); // tabIndex -1: next Tab continues in document order
          return false;
        case "newline":
          send(new TextEncoder().encode("\x1b\r"));
          return false;
        case "paste":
          navigator.clipboard?.readText?.().then(sendPaste).catch(() => {});
          return false;
        default:
          return true;
      }
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

    // Resize wiring
    // Observe the terminal's own (flex-grown) box so any layout change — panel
    // resize, fullscreen toggle, window resize — refits and re-sizes the PTY.
    // ResizeObserver and window resize coalesce into one frame, one refit.
    let resizeRaf = 0;
    const scheduleRefit = () => {
      if (resizeRaf) return;
      resizeRaf = requestAnimationFrame(() => {
        resizeRaf = 0;
        refit();
      });
    };
    const resizeObserver = new ResizeObserver(scheduleRefit);
    resizeObserver.observe(mount);
    const onWinResize = scheduleRefit;
    window.addEventListener("resize", onWinResize);

    // Cleanup
    return () => {
      if (resizeRaf) cancelAnimationFrame(resizeRaf);
      // Stop any pending backoff from spawning a new socket after unmount, and
      // mark the close as intentional (so the in-flight ws.onclose won't retry).
      disposed = true;
      disposedFont = true;
      if (reconnectTimer) clearTimeout(reconnectTimer);
      if (autoRunTimer) clearTimeout(autoRunTimer);
      if (connectTimer) clearTimeout(connectTimer);
      cancelAnimationFrame(rafId);
      window.removeEventListener("resize", onWinResize);
      mount.removeEventListener("paste", onPaste, true);
      inputDispose.dispose();
      binaryDispose.dispose();
      copyGate.dispose();
      copyGateRef.current = null;
      unexpose();
      renderer.dispose();
      rendererRef.current = null;
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
    // mayEnter is keyed on deliberately: a confirmed non-owner tears the
    // terminal down. operator and operatorResolved are read through refs.
    // eslint-disable-next-line react-hooks/exhaustive-deps -- refs and setters are stable identities (useRef/useState in the caller); these deps are unchanged from the effect this hook was extracted from
  }, [runId, tokenOnlyMode, refit, mayEnter, refusal, signedOut]);
}
