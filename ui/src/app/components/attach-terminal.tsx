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
// A full JetBrains Mono build, self-hosted (same-origin, no external load), so the
// TUI gets true fixed-advance cells and its box-drawing glyphs come from the font.
import "../../styles/terminal-font.css";
import { getToken, HttpError } from "../lib/api/core";
import { runs } from "../lib/api/runs";
import type { AttachHolder } from "../lib/types/runs";
import type { PolicyRef } from "../lib/api/health";
import { PolicyRemedy } from "./wardyn/policy-remedy";
import { getErrorMessage } from "../lib/format";
import { Eye, Loader2, TriangleAlert, Maximize2, Minimize2, RotateCw } from "lucide-react";
import { cn } from "./ui/utils";
import { Button } from "./ui/button";
import { TakeoverConfirmDialog } from "./attach-takeover-dialog";
import { LinkConfirmDialog } from "./attach-link-dialog";
import { TerminalConnectionStatus } from "./attach-terminal-status";
import { CopyBlockedNotice, CopyOfferToast } from "./attach-terminal-copy-offer";
import {
  readRendererPref,
  writeRendererPref,
  type RendererControl,
  type RendererPref,
  type RendererState,
} from "./attach-terminal-renderer";
import { RendererFellBackNotice, RendererMenu } from "./attach-terminal-renderer-menu";
import { isMacPlatform, type CopyGate, type CopyOffer } from "./attach-terminal-clipboard";
import { RUN_COCKPIT, TERMINAL, TERMINAL_COPY } from "./wardyn/copy";
import { useOperator, useOperatorResolved, usePrincipal } from "./wardyn/operator-context";
import { entryErrorMessage, mayEnterRun, RUN_OWNER_ONLY } from "../lib/run-entry";
import { OPERATOR_ONLY_REASON } from "./wardyn/copy";
import { useTerminalFullscreen } from "./use-attach-terminal-fullscreen";
import { useAttachSession, MAX_RECONNECT_ATTEMPTS, type ConnState } from "./use-attach-session";
import { useSignedOut } from "../lib/use-signed-out";

// Auth-mode detection
// api.ts stores the admin token in localStorage under this key.  When the
// token is present AND there is no valid OIDC session (we can't read
// HttpOnly cookies from JS, but we know the UI only uses a token when the
// OIDC flow is not active), the WS handshake cannot carry the bearer — so we
// mint a single-use attach ticket via the normal authenticated REST surface
// and present it as ?ticket= instead.
function isAdminTokenOnlyMode(): boolean {
  return getToken() !== null;
}

// Component
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
   * The run's creator (AgentRun.created_by), for a mount site that has a run
   * object to read it from. Compared against the signed-in principal so a
   * member can attach to a run THEY own — see the operator gate below.
   * OMIT it to mean "the caller created this run; the server decides": the
   * login pane, record-pane and demo-runner all mount on a run they launched
   * one round trip earlier. Unknown ownership is not "not yours" — the gate
   * lets it through to the ticket lane, which is the enforcement point.
   */
  createdBy?: string;
  /** The run belongs to no person (an operator-owned service or local run): an
   *  admin may enter it. Absent on a person's run, where entry is the owner's
   *  alone (#1476). */
  operatorOwned?: boolean;
  /** The run's own policy (GET /runs/{id}), for the Request access remedy
   *  beside a refusal. Never an error envelope. */
  policy?: PolicyRef;
}

export interface AttachTerminalHandle {
  /** Write text straight to the PTY stdin (e.g. a pasted code + "\r"). */
  sendText: (text: string) => void;
}

export const AttachTerminal = React.forwardRef<AttachTerminalHandle, AttachTerminalProps>(function AttachTerminal(
  { runId, onClose, autoRun, onOutput, ptyCols, heightClass = "h-[70vh]", fill, createdBy, operatorOwned, policy },
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
  // Whether `operator` is the SERVER'S answer or the fail-open default (connect() below).
  const operatorResolved = useOperatorResolved();
  const signedOut = useSignedOut(); // #483: no socket at all while signed out mid-page
  const principal = usePrincipal();
  // Unknown ownership asks the server (P1) — see createdBy's doc above. So does
  // an unresolved /me (R4-F110): the ticket lane is the enforcement point, and
  // refusing on a principal we do not know yet would turn away the real owner.
  // Otherwise the one entry rule (#1476): the run's person, or an admin on a
  // run no person owns.
  const mayEnter =
    createdBy === undefined ||
    !operatorResolved ||
    mayEnterRun({ created_by: createdBy, operator_owned: operatorOwned }, principal, operator);
  // This pane's own error line: the terminal-error wording of the owner-only rule.
  const refusal = operatorOwned ? OPERATOR_ONLY_REASON : RUN_OWNER_ONLY;
  const containerRef = React.useRef<HTMLDivElement>(null);
  // The whole panel (title bar + grid) — the element handed to the native
  // Fullscreen API below.
  const panelRef = React.useRef<HTMLDivElement>(null);
  const termRef = React.useRef<Terminal | null>(null);
  const fitAddonRef = React.useRef<FitAddon | null>(null);
  const wsRef = React.useRef<WebSocket | null>(null);
  const [connState, setConnState] = React.useState<ConnState>("connecting");
  const [errorMsg, setErrorMsg] = React.useState<string>("");
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
  // #216 — mirrors the connect effect's `reconnectAttempts` var for render.
  const [reconnectAttempt, setReconnectAttempt] = React.useState(0);
  const [reconnectExhausted, setReconnectExhausted] = React.useState(false);
  // The clipboard gate's verified copy offer and its quiet "blocked" notice.
  const [copyOffer, setCopyOffer] = React.useState<CopyOffer | null>(null);
  const [copyNotice, setCopyNotice] = React.useState<string | null>(null);
  // The offer card takes focus; when it goes, the terminal gets it back.
  const hadOffer = React.useRef(false);
  React.useEffect(() => {
    if (hadOffer.current && !copyOffer) termRef.current?.focus();
    hadOffer.current = !!copyOffer;
  }, [copyOffer]);
  const selectHint =TERMINAL_COPY.SELECT_HINT(TERMINAL_COPY.NATIVE_CHORD(isMacPlatform()));
  const copyGateRef = React.useRef<CopyGate | null>(null);
  // A clicked terminal link awaiting the confirm dialog.
  const [linkTarget, setLinkTarget] = React.useState<URL | null>(null);
  // Renderer: the per-browser choice, the live control, and what is actually drawing.
  const [rendererPref, setRendererPref] = React.useState<RendererPref>(readRendererPref);
  const rendererPrefRef = React.useRef(rendererPref);
  const rendererRef = React.useRef<RendererControl | null>(null);
  const [renderer, setRenderer] = React.useState<RendererState>({ active: "compatible", fellBack: false });
  const [fellBackSeen, setFellBackSeen] = React.useState(false);
  React.useEffect(() => {
    if (renderer.fellBack) setFellBackSeen(true);
  }, [renderer.fellBack]);
  const pickRenderer = (pref: RendererPref) => {
    writeRendererPref(pref);
    rendererPrefRef.current = pref;
    setRendererPref(pref);
    setFellBackSeen(false);
    rendererRef.current?.set(pref);
    termRef.current?.focus();
  };

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
  // #216 — Reconnect button's handler; assigned by the connect effect below.
  const manualReconnectRef = React.useRef<() => void>(() => {});

  // Token-only mode routes the WS handshake through a minted attach ticket
  // (the browser cannot put the bearer on the handshake itself).
  const tokenOnlyMode = React.useMemo(() => isAdminTokenOnlyMode(), []);

  // Refit the terminal to its (current) container size and tell the PTY. With
  // ptyCols set, the visual grid is pinned to that width (rows still follow the
  // container) so the PTY and xterm always agree — see the ptyCols doc.
  // refit(force) — measure the container, resize the local grid, tell the PTY.
  //
  // A resize frame goes out only when {socket, cols, rows} differs from the last
  // one sent, so a same-size refit is silent and a fresh socket still gets its
  // first size. A zero-size box (hidden pane) is skipped, not measured.
  //
  // `force` bypasses that dedup and sends a ONE-COLUMN-SMALLER size first, then
  // the real one. That looks pointless and is not: the session is tmux, and
  // tmux (3.5a defaults to `window-size latest`) sizes the shared window from a
  // client's most recent size, re-evaluating on a client size CHANGE.
  // So when a second client (a `wardyn run attach` from another terminal) attaches
  // small, the browser's grid fills with tmux's `·` filler — and when that
  // client leaves, the filler can STAY, because the browser's own size never
  // changed and a same-size resize frame is a no-op tmux ignores.
  //
  // Measured: 0 dots before a second client, 1001 while attached, still 1001
  // after it detached, and 0 again the moment the viewport actually changed
  // size. The nudge manufactures that change on demand.
  const lastSentRef = React.useRef<{ ws: WebSocket; cols: number; rows: number } | null>(null);
  // An observer's grid is pinned to the writer's (attach-mode holder size).
  const observerPinRef = React.useRef<{ cols: number; rows: number } | null>(null);
  const refit = React.useCallback((force = false) => {
    const fit = fitAddonRef.current;
    const term = termRef.current;
    const ws = wsRef.current;
    if (!fit || !term) return;
    const box = term.element?.parentElement;
    if (box && (box.clientWidth === 0 || box.clientHeight === 0)) return;
    const pin = observerPinRef.current;
    try {
      if (pin) {
        term.resize(pin.cols, pin.rows);
      } else if (ptyColsRef.current) {
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
    // A pinned observer's resize frames are dropped server-side; send none.
    if (!pin && ws && ws.readyState === WebSocket.OPEN && term.cols > 0 && term.rows > 0) {
      const last = lastSentRef.current;
      if (!force && last && last.ws === ws && last.cols === term.cols && last.rows === term.rows) return;
      if (force && term.cols > 1) {
        ws.send(JSON.stringify({ type: "resize", cols: term.cols - 1, rows: term.rows }));
      }
      ws.send(JSON.stringify({ type: "resize", cols: term.cols, rows: term.rows }));
      lastSentRef.current = { ws, cols: term.cols, rows: term.rows };
    }
  }, []);

  useAttachSession({
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
    observerPinRef,
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
  });

  // Fullscreen (native API, Escape fallback, refit-on-toggle) — see
  // use-attach-terminal-fullscreen.ts for the reasoning; split into its own
  // hook purely to keep this file under its line cap.
  // Focus the terminal only when the page has nothing better to give focus to:
  // nothing focused, or focus already inside this panel. Never steals from a
  // control elsewhere on the page (Q-T6; observers included).
  const focusTermIfFree = React.useCallback(() => {
    const a = document.activeElement;
    if (a && a !== document.body && !panelRef.current?.contains(a)) return;
    termRef.current?.focus();
  }, []);
  const { fullscreen, toggleFullscreen } = useTerminalFullscreen(panelRef, refit, focusTermIfFree);

  // Holder / take-over
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
    let promoted = false;
    try {
      ({ promoted } = await runs.takeoverAttach(runId));
    } catch (e) {
      // A 409 means the server says NOBODY holds it — the holder left while we
      // sat here as an observer, and nothing told us: past connect, an
      // attach-mode frame (read_only:false) comes only with a promotion, and
      // ours did not happen. Without this branch the panel was a dead end,
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
      setTakeoverErr(entryErrorMessage(e, getErrorMessage));
      return;
    }
    setTakenOverBy(null);
    // promoted: this principal's FIRST observer socket was flipped to writer IN
    // PLACE, its notice on the way (see lastReadOnly above); reconnecting would
    // close it and hand the slot to the oldest bystander, landing the taker
    // read-only (#507). That socket may be another tab's, so only a still-open
    // one can be ours — a displaced or exhausted panel has none, and reclaims.
    if (promoted && wsRef.current?.readyState === WebSocket.OPEN) return;
    setMode(null);
    reclaimRef.current(); // evict-then-reconnect; see reclaimRef's assignment
  }, [runId]);

  // Mounting note: this panel is safe to embed anywhere — a page, a card, a
  // dialog — because fullscreen goes through the native API (see
  // toggleFullscreen) rather than a `fixed inset-0` overlay that any ancestor
  // could capture. Portaling on toggle would NOT have been safe: the xterm setup
  // effect is keyed on [runId, tokenOnlyMode, refit, mayEnter] and not on
  // fullscreen, so React would rebuild this container under the new parent
  // without re-running term.open() and leave a permanently blank terminal.
  return (
    <div
      ref={panelRef}
      // R4-F144: the chord's landing pad. -1 keeps it out of the tab ORDER (it
      // is not a control) while making it focusable programmatically, so Tab
      // after the chord continues in document order from here.
      tabIndex={-1}
      className={cn(
        "flex flex-col overflow-hidden border border-border bg-[#0d1117] focus:outline-none",
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
          {/* #216 — was `[closed] {runId}` / `[error] {runId}`; state now
              lives in TerminalConnectionStatus below, so the bar just names
              the run. */}
          {(connState === "closed" || connState === "error") && runId}
        </span>
        {/* State chip (design board 2d). Only ever rendered from what the
            SERVER said: "driving" needs an attach-mode frame with
            read_only=false, so a daemon that never sends one shows no chip
            rather than a claim we cannot back. */}
        {(readOnly || displaced) && holderPrincipal && (
          <span className="inline-flex shrink-0 items-center gap-1 rounded border border-info/25 bg-info-subtle px-1.5 py-0.5 font-mono text-meta text-info">
            <Eye className="size-3" />
            {RUN_COCKPIT.heldBy(holderPrincipal)}
          </span>
        )}
        {!readOnly && !displaced && mode && connState === "open" && (
          <span className="inline-flex shrink-0 items-center gap-1 rounded border border-success/25 bg-success-subtle px-1.5 py-0.5 font-mono text-meta text-success">
            <span className="size-1.5 rounded-full bg-current" />
            {RUN_COCKPIT.driving}
          </span>
        )}
        <div className="ml-auto flex items-center gap-2">
          {/* Live geometry — the grid this client actually has, post-refit. */}
          {geom && (
            <span className="font-mono text-meta text-muted-foreground">
              {geom.cols}×{geom.rows}
            </span>
          )}
          {/* R4-F144: 2.1.2 wants the exit advised BEFORE it is needed, so it
              is in the chrome — not a tooltip or a help page a trapped keyboard
              user cannot reach. Hidden below sm (the readout already wraps
              there); the grid's aria-description carries it at every width. */}
          <span className="hidden font-mono text-meta text-muted-foreground sm:inline">
            {TERMINAL.ESCAPE_CHORD_HINT}
          </span>
          {/* M11 S1: the native-selection hint, for observers too. Below lg it
              lives in the grid's aria-description only. */}
          <span className="hidden font-mono text-meta text-muted-foreground lg:inline">· {selectHint}</span>
          {(connState === "connecting" || connState === "reconnecting") && (
            <Loader2 className="size-3.5 animate-spin text-muted-foreground" />
          )}
          {connState === "open" && (
            <span className="inline-flex size-2 rounded-full bg-success" title="Connected" />
          )}
          <RendererMenu pref={rendererPref} active={renderer.active} onPick={pickRenderer} />
          {/* Redraw. The browser cannot observe another client detaching, so it
              cannot know the tmux window is still clamped to a size that left —
              see refit's note. One click forces the size change that clears it. */}
          <button
            type="button"
            onMouseDown={(e) => e.preventDefault()}
            onClick={() => {
              refit(true);
              termRef.current?.focus();
            }}
            title="Redraw (fixes a terminal left clamped by another attached client)"
            aria-label="Redraw terminal"
            className="rounded p-1 text-muted-foreground hover:bg-muted hover:text-foreground"
          >
            <RotateCw className="size-3.5" />
          </button>
          <button
            type="button"
            onMouseDown={(e) => e.preventDefault()}
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
          <p>
            {errorMsg} <PolicyRemedy policy={policy} className="block" />
          </p>
        </div>
      )}

      {/* xterm container — flex-grows to fill the panel / fullscreen viewport.
          A pinned-width grid (ptyCols) renders wider than the pane; scroll it
          horizontally rather than clipping the right edge. The wrapper exists
          so the read-only badge can be positioned over the grid without being
          a CHILD of the element xterm owns. */}
      <div className="relative flex min-h-0 flex-1 flex-col">
        <div
          data-testid="run-terminal-wrapper"
          className={cn("min-h-0 flex-1 p-1", (ptyCols || (readOnly && mode?.holder?.cols)) && "overflow-x-auto")}
          // D3: xterm only focuses itself on a click that lands exactly on its
          // own `.xterm-screen` canvas layer. A click on this wrapper's padding
          // or in the dead space below the last row would otherwise take the
          // browser's own focus step, which moves focus to the tabIndex={-1}
          // panel. preventDefault cancels that step and we focus the terminal
          // ourselves, so any click in the terminal area focuses it. Kept on
          // THIS wrapper, not the panel root: the root also holds the title-bar
          // buttons and the footer's Take-over button, and the tabIndex={-1}
          // landing pad the escape chord targets.
          onMouseDown={(e) => {
            e.stopPropagation();
            e.preventDefault();
            termRef.current?.focus();
          }}
        >
          {/* FitAddon measures the PARENT of `.xterm`; under border-box sizing a
              padded parent over-counts rows and clips the last one. The mount
              is therefore an unpadded child of the padded wrapper above. */}
          <div
            ref={containerRef}
            className="h-full min-h-0"
            // R4-F144: the same sentence the title bar shows, for the reader who
            // cannot see it — 2.1.2's "advised on entry" has to hold for a screen
            // reader landing in the grid, not only for a sighted user.
            aria-description={`${TERMINAL.ESCAPE_CHORD_HINT} · ${selectHint}`}
          />
        </div>
        {copyOffer ? (
          <CopyOfferToast key={copyOffer.id} offer={copyOffer} onDone={() => copyGateRef.current?.dismiss()} />
        ) : null}
        {readOnly && (
          // pointer-events-none: this is a label, not a shield. The input it
          // describes is dropped SERVER-side; blocking clicks here would also
          // block selecting and copying the output, which a spectator can do.
          <div className="pointer-events-none absolute bottom-3 left-3 inline-flex items-center gap-2 rounded-lg border border-info/35 bg-info/15 px-2.5 py-1.5">
            <Eye className="size-3.5 text-info" />
            <span className="font-mono text-meta text-info">{RUN_COCKPIT.watchingReadOnly}</span>
          </div>
        )}
      </div>

      {/* #216 — connection state, outside the scrollback, so it can't scroll
          away. `displaced` is excluded: it has its own footer below, with its
          own action (Take over, not Reconnect). */}
      {connState === "reconnecting" && (
        <TerminalConnectionStatus
          state="reconnecting"
          attempt={reconnectAttempt}
          maxAttempts={MAX_RECONNECT_ATTEMPTS}
          onReconnect={() => manualReconnectRef.current()}
        />
      )}
      {connState === "closed" && reconnectExhausted && !displaced && (
        <TerminalConnectionStatus
          state="closed"
          attempt={reconnectAttempt}
          maxAttempts={MAX_RECONNECT_ATTEMPTS}
          onReconnect={() => manualReconnectRef.current()}
        />
      )}

      {copyNotice && <CopyBlockedNotice onDismiss={() => setCopyNotice(null)} />}
      {fellBackSeen && <RendererFellBackNotice onDismiss={() => setFellBackSeen(false)} />}

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

      <TakeoverConfirmDialog
        open={confirmTakeover}
        holderPrincipal={holderPrincipal}
        onOpenChange={(o) => !o && setConfirmTakeover(false)}
        onConfirm={() => void doTakeover()}
        onCloseFocus={() => termRef.current?.focus()}
      />
      <LinkConfirmDialog
        url={linkTarget}
        onClose={() => setLinkTarget(null)}
        onCloseFocus={() => termRef.current?.focus()}
      />
    </div>
  );
});
