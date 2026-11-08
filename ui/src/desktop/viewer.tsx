/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The desktop viewer (mock M6: M6-Desktop, M6-States, M6-Phone), served by the UI gateway on its own address
// (internal/api/uigateway_desktop.go). It has no console session: what it needs rides data-wardyn-desktop.

import * as React from "react";
import { Button } from "../app/components/ui/button";
import { Chip } from "../app/components/wardyn/primitives";
import { originWS } from "../app/lib/base-path";
import { DESKTOP } from "../app/components/wardyn/copy/desktop";
import { isEscapeChord, keysymOf } from "./keysym";
import { RfbClient, type RfbDisplay } from "./rfb";

// `machine_kind` is the run's (types.MachineKind); absent when the gateway could not read the run.
// `view_only`: the gateway keeps this session's input from the desktop.
// `background`: this session is a watch of a background run's desktop; it always comes with view_only.
// `run_title` and `run_href`: the run's name and its page, for the way back to it; both or neither.
export type Boot = { state: "open"; expires: number; rfb: string; close: string; machine_kind?: string; view_only?: boolean; background?: boolean; run_title?: string; run_href?: string } | { state: "refused"; said: string };

/** What the gateway wrote on the page, or null when it wrote nothing usable. */
export function readBoot(el: HTMLElement = document.documentElement): Boot | null {
  try {
    const b = JSON.parse(el.dataset.wardynDesktop || "null") as Boot | null;
    return b && (b.state === "open" || b.state === "refused") ? b : null;
  } catch {
    return null;
  }
}

/** The socket the viewer talks RFB over; a WebSocket in the browser. */
export interface DesktopSocket {
  binaryType: BinaryType;
  onopen: ((ev: Event) => void) | null;
  onmessage: ((ev: MessageEvent) => void) | null;
  onclose: ((ev: CloseEvent) => void) | null;
  send(data: Uint8Array): void;
  close(): void;
}

const openSocket = (path: string): DesktopSocket => new WebSocket(originWS(path), ["binary"]);

// The gateway's close codes (uigateway_desktop.go): an ended session or run is final; anything else retries,
// except a socket the gateway refused before it opened (403: the session expired, was closed or was revoked).
const SESSION_ENDED = 4001;
const RUN_ENDED = 4002;
const NOT_AUTHORIZED = 4003;
const WATCH_ENDED = 4005;
// A window opened on the Mac, so the browser desktop only watches (one keyboard at a time). Until the gateway
// sends this code, it closes with SESSION_ENDED and a reason starting WINDOW_OPEN_LEGACY_REASON; both read the same.
export const WINDOW_OPEN = 4006;
export const WINDOW_OPEN_LEGACY_REASON = "A window is open on the Mac";
const UNREACHABLE = 4004;
// One wheel notch per this many pixels of scroll, so a light trackpad flick is not dozens of lines.
const WHEEL_NOTCH_PX = 50;

type Phase = "starting" | "reconnecting" | "connected" | "closed" | "expired" | "run_ended" | "watch_ended" | "window_open" | "refused";

export function DesktopViewer({ boot, connect = openSocket, small }: { boot: Boot; connect?: (path: string) => DesktopSocket; small?: boolean }) {
  if (boot.state === "refused") return <LinkRefused said={boot.said} />;
  return <Desktop boot={boot} connect={connect} viewOnly={boot.view_only === true || (small ?? window.matchMedia?.("(max-width: 640px)").matches ?? false)} />;
}

function Desktop({ boot, connect, viewOnly }: { boot: Extract<Boot, { state: "open" }>; connect: (path: string) => DesktopSocket; viewOnly: boolean }) {
  const canvas = React.useRef<HTMLCanvasElement>(null);
  const client = React.useRef<RfbClient | null>(null);
  const socket = React.useRef<DesktopSocket | null>(null);
  const closing = React.useRef(false);
  const [phase, setPhase] = React.useState<Phase>("starting");
  const [fit, setFit] = React.useState(true);
  const [closeFailed, setCloseFailed] = React.useState(false);
  const [said, setSaid] = React.useState<string | null>(null); // the gateway's reason, once a dial has failed twice
  const [focused, setFocused] = React.useState(false);
  const pressed = React.useRef(new Map<string, number>()); // the keysym each held key went down with
  const wheel = React.useRef({ x: 0, y: 0 }); // scroll pixels not yet sent as a notch

  React.useEffect(() => {
    let retry: ReturnType<typeof setTimeout> | undefined;
    let delay = 1000;
    let live = true;
    let connectedOnce = false;
    let fails = 0;
    let reason = "";
    const dial = () => {
      const ws = connect(boot.rfb);
      socket.current = ws;
      ws.binaryType = "arraybuffer";
      let opened = false;
      ws.onopen = () => {
        opened = true;
      };
      const rfb = new RfbClient((b) => ws.send(b), canvasDisplay(canvas), {
        connected: () => {
          delay = 1000;
          connectedOnce = true;
          fails = 0;
          setSaid(null);
          setPhase("connected");
        },
        failed: () => ws.close(),
      });
      client.current = rfb;
      ws.onmessage = (ev) => rfb.receive(ev.data as ArrayBuffer);
      ws.onclose = (ev) => {
        client.current = null;
        pressed.current.clear(); // the next connection starts with nothing held
        if (!live) return;
        if (closing.current || ev.code === NOT_AUTHORIZED) setPhase("closed");
        else if (ev.code === WINDOW_OPEN || (ev.code === SESSION_ENDED && ev.reason?.startsWith(WINDOW_OPEN_LEGACY_REASON))) setPhase("window_open");
        else if (ev.code === SESSION_ENDED) setPhase("expired");
        else if (ev.code === RUN_ENDED) setPhase("run_ended");
        else if (ev.code === WATCH_ENDED) setPhase("watch_ended");
        else {
          // Booting, restarting or briefly unreachable: the same tab connects on its own. A browser hides a
          // refused handshake's status, so a socket that never opened asks the gateway once whether it was 403.
          setPhase(connectedOnce ? "reconnecting" : "starting");
          fails++;
          if (ev.code === UNREACHABLE && ev.reason) reason = ev.reason;
          setSaid(fails >= 2 && reason ? reason : null);
          const again = () => {
            if (!live) return;
            retry = setTimeout(dial, delay);
            delay = Math.min(delay * 2, 10_000);
          };
          if (opened) return again();
          void fetch(boot.rfb, { credentials: "same-origin" })
            .then((r) => (r.status === 403 ? live && setPhase("refused") : again()))
            .catch(again);
        }
      };
    };
    dial();
    return () => {
      live = false;
      clearTimeout(retry);
      socket.current?.close();
    };
  }, [boot.rfb, connect]);

  // The desktop stays open until the gateway answers 204: any other answer (503: the Close was not stored)
  // leaves the session valid, so the person is told and can try again. closing is set only then, so a socket
  // that drops while the Close is in flight reconnects as any other drop does.
  const close = async () => {
    setCloseFailed(false);
    const r = await fetch(boot.close, { method: "POST", credentials: "same-origin" }).catch(() => null);
    if (r?.status !== 204) {
      setCloseFailed(true);
      return;
    }
    closing.current = true;
    socket.current?.close();
    setPhase("closed");
    window.close();
  };

  // Held keys must not outlive the focus: the VM would keep a modifier down while the person is in another window.
  React.useEffect(() => {
    if (viewOnly) return;
    const release = () => releaseAll(client, pressed.current);
    const hidden = () => document.visibilityState === "hidden" && release();
    window.addEventListener("blur", release);
    document.addEventListener("visibilitychange", hidden);
    return () => {
      window.removeEventListener("blur", release);
      document.removeEventListener("visibilitychange", hidden);
    };
  }, [viewOnly]);

  const input = viewOnly || phase !== "connected" ? {} : inputHandlers(canvas, client, pressed.current, wheel.current);
  const ended = phase === "closed" || phase === "expired" || phase === "run_ended" || phase === "refused";
  return (
    <div className="flex h-dvh flex-col bg-background text-foreground">
      <header className="flex flex-wrap items-center gap-3 border-b border-border px-4 py-2 text-xs">
        {phase === "connected" && <Chip tone="success" dot>{DESKTOP.connected}</Chip>}
        <span className="font-medium">{DESKTOP.title}</span>
        {boot.run_title && boot.run_href && /^https?:\/\//.test(boot.run_href) && (
          <a href={boot.run_href} className="text-muted-foreground underline underline-offset-2">{DESKTOP.backToRun(boot.run_title)}</a>
        )}
        <span className="flex-1" />
        <div className="inline-flex rounded-[9px] border border-border bg-surface-2 p-[3px]">
          {[[true, DESKTOP.fit], [false, DESKTOP.actualSize]].map(([value, label]) => (
            <button key={String(value)} type="button" aria-pressed={fit === value} onClick={() => setFit(value as boolean)}
              className={fit === value ? "rounded-md bg-card px-3 py-[5px] shadow-sm" : "rounded-md px-3 py-[5px] text-muted-foreground"}>
              {label as string}
            </button>
          ))}
        </div>
        {!ended && <Button size="sm" variant="outline" onClick={() => void close()}>{DESKTOP.close}</Button>}
      </header>
      {closeFailed && !ended && (
        <p role="alert" className="border-b border-border px-4 py-2 text-xs">{DESKTOP.closeFailed}</p>
      )}
      {viewOnly && (
        <p className="border-b border-border px-4 py-2 text-xs">
          <span className="font-medium">{boot.view_only ? DESKTOP.viewOnlyTitle : DESKTOP.phoneTitle}</span>{" "}
          <span className="text-muted-foreground">{boot.background ? DESKTOP.backgroundWatchBody : boot.view_only ? DESKTOP.viewOnlyBody : DESKTOP.phoneBody}</span>
        </p>
      )}
      {!viewOnly && phase === "connected" && (
        <p className="border-b border-border px-4 py-2 text-xs text-muted-foreground">
          {boot.machine_kind === "macos_vm" ? DESKTOP.noClipboardMac : DESKTOP.noClipboard}
          {focused && <span className="block">{DESKTOP.escapeHint}</span>}
        </p>
      )}
      <main className={fit ? "relative flex min-h-0 flex-1 items-center justify-center bg-black" : "relative min-h-0 flex-1 overflow-auto bg-black"}>
        <canvas
          ref={canvas}
          data-testid="desktop-canvas"
          tabIndex={viewOnly ? -1 : 0}
          aria-label={DESKTOP.canvasLabel}
          className={fit ? "max-h-full max-w-full object-contain" : undefined}
          hidden={phase !== "connected"}
          onContextMenu={(e) => e.preventDefault()}
          onFocus={() => setFocused(true)}
          onBlur={() => {
            setFocused(false);
            releaseAll(client, pressed.current);
          }}
          {...input}
        />
        {phase !== "connected" && <Notice phase={phase} macOS={boot.machine_kind === "macos_vm"} said={said} />}
      </main>
    </div>
  );
}

function Notice({ phase, macOS, said }: { phase: Exclude<Phase, "connected">; macOS: boolean; said: string | null }) {
  const [title, body] =
    phase === "starting" ? [macOS ? DESKTOP.startingMacOS : DESKTOP.starting, DESKTOP.startingBody]
    : phase === "reconnecting" ? [DESKTOP.reconnecting, DESKTOP.reconnectingBody]
    : phase === "window_open" ? [DESKTOP.windowOpenTitle, DESKTOP.windowOpenBody]
    : phase === "expired" ? [DESKTOP.sessionEnded, DESKTOP.sessionEndedBody]
    : phase === "run_ended" ? [DESKTOP.runEnded, undefined]
    : phase === "watch_ended" ? [DESKTOP.sessionEnded, DESKTOP.watchEndedBody]
    : phase === "refused" ? [DESKTOP.sessionEnded, DESKTOP.refusedBody]
    : [DESKTOP.sessionEnded, undefined];
  return (
    <div role="status" className="absolute inset-0 flex flex-col items-center justify-center gap-2 p-6 text-center">
      <p className="text-sm font-medium text-white">{title}</p>
      {body && <p className="max-w-md text-xs text-neutral-400">{body}</p>}
      {said && (phase === "starting" || phase === "reconnecting") && <p className="max-w-md text-xs text-neutral-400">{DESKTOP.stillTrying(said)}</p>}
    </div>
  );
}

function LinkRefused({ said }: { said: string }) {
  const rows: [string, string][] = [
    [DESKTOP.whyWall, DESKTOP.whyWallBody],
    [DESKTOP.whySetBy, DESKTOP.whySetByBody],
    [DESKTOP.whyEnforced, DESKTOP.whyEnforcedBody],
    [DESKTOP.whyCanChange, DESKTOP.whyCanChangeBody],
  ];
  return (
    <main className="flex min-h-dvh items-center justify-center bg-background p-4 text-foreground">
      <section aria-labelledby="desktop-refused" className="w-full max-w-lg rounded-[11px] border border-border bg-card p-5 text-sm">
        <div className="flex flex-wrap items-center gap-2">
          <h1 id="desktop-refused" className="text-base font-semibold">{DESKTOP.linkBlocked}</h1>
          <Chip tone="danger">{DESKTOP.linkRefused}</Chip>
        </div>
        <p className="mt-2 text-muted-foreground">{DESKTOP.linkBody}</p>
        <dl className="mt-4 grid grid-cols-[110px_minmax(0,1fr)] gap-x-3 gap-y-2 text-xs">
          {rows.map(([k, v]) => (
            <React.Fragment key={k}>
              <dt className="text-muted-foreground">{k}</dt>
              <dd>{v}</dd>
            </React.Fragment>
          ))}
        </dl>
        <p className="mt-4 text-xs text-muted-foreground">{DESKTOP.gatewaySaid}</p>
        <p data-testid="desktop-refused-said" className="mt-1 rounded-md bg-surface-2 px-2 py-1.5 font-mono text-xs">{said}</p>
        <Button size="sm" variant="outline" className="mt-4" onClick={() => void navigator.clipboard?.writeText(said)}>
          {DESKTOP.copyDetails}
        </Button>
      </section>
    </main>
  );
}

function canvasDisplay(ref: React.RefObject<HTMLCanvasElement | null>): RfbDisplay {
  const ctx = () => ref.current?.getContext("2d") ?? null;
  return {
    resize(w, h) {
      if (ref.current) [ref.current.width, ref.current.height] = [w, h];
    },
    putRaw(x, y, w, h, rgbx) {
      const c = ctx();
      if (!c || w === 0 || h === 0) return;
      const img = c.createImageData(w, h);
      img.data.set(rgbx);
      for (let i = 3; i < img.data.length; i += 4) img.data[i] = 255;
      c.putImageData(img, x, y);
    },
    copy(sx, sy, x, y, w, h) {
      const c = ctx();
      if (c && ref.current) c.drawImage(ref.current, sx, sy, w, h, x, y, w, h);
    },
  };
}

// DOM buttons (1 left, 2 right, 4 middle) to RFB's mask (1 left, 2 middle, 4 right).
const rfbButtons = (b: number) => (b & 1) | (b & 4 ? 2 : 0) | (b & 2 ? 4 : 0);

/** Key-up for every held key, so the VM is left with none down. */
function releaseAll(client: React.RefObject<RfbClient | null>, pressed: Map<string, number>) {
  for (const sym of pressed.values()) client.current?.key(sym, false);
  pressed.clear();
}

function inputHandlers(canvas: React.RefObject<HTMLCanvasElement | null>, client: React.RefObject<RfbClient | null>, pressed: Map<string, number>, wheel: { x: number; y: number }) {
  const at = (e: { clientX: number; clientY: number }) => {
    const el = canvas.current;
    if (!el) return [0, 0];
    const r = el.getBoundingClientRect();
    return [((e.clientX - r.left) * el.width) / (r.width || 1), ((e.clientY - r.top) * el.height) / (r.height || 1)];
  };
  const pointer = (e: React.PointerEvent) => {
    const [x, y] = at(e);
    client.current?.pointer(x, y, rfbButtons(e.buttons));
  };
  // A drag that leaves the canvas (across Fit's border, out of the window) keeps arriving here until release.
  const uncapture = (e: React.PointerEvent) => {
    if (canvas.current?.hasPointerCapture?.(e.pointerId)) canvas.current.releasePointerCapture(e.pointerId);
  };
  return {
    onPointerMove: pointer,
    onPointerDown: (e: React.PointerEvent) => {
      canvas.current?.focus();
      try {
        canvas.current?.setPointerCapture?.(e.pointerId);
      } catch {
        // a pointer that is already gone: nothing to capture
      }
      pointer(e);
    },
    onPointerUp: (e: React.PointerEvent) => {
      pointer(e);
      uncapture(e);
    },
    onPointerCancel: (e: React.PointerEvent) => {
      const [x, y] = at(e);
      client.current?.pointer(x, y, 0);
      uncapture(e);
    },
    onLostPointerCapture: (e: React.PointerEvent) => {
      const [x, y] = at(e);
      client.current?.pointer(x, y, 0);
    },
    onWheel: (e: React.WheelEvent) => {
      const [x, y] = at(e);
      const px = e.deltaMode === 1 ? 16 : e.deltaMode === 2 ? 800 : 1; // lines and pages to pixels
      wheel.x += e.deltaX * px;
      wheel.y += e.deltaY * px;
      const down = Math.trunc(wheel.y / WHEEL_NOTCH_PX);
      const right = Math.trunc(wheel.x / WHEEL_NOTCH_PX);
      wheel.y -= down * WHEEL_NOTCH_PX;
      wheel.x -= right * WHEEL_NOTCH_PX;
      const held = rfbButtons(e.buttons);
      const notch = (n: number, button: number) => {
        for (let i = 0; i < Math.abs(n); i++) {
          client.current?.pointer(x, y, held | button);
          client.current?.pointer(x, y, held);
        }
      };
      notch(down, down < 0 ? 8 : 16);
      notch(right, right < 0 ? 32 : 64);
    },
    onKeyDown: (e: React.KeyboardEvent) => {
      if (isEscapeChord(e)) {
        e.preventDefault();
        releaseAll(client, pressed);
        canvas.current?.blur();
        return;
      }
      const sym = keysymOf(e);
      if (sym === undefined) return;
      e.preventDefault();
      pressed.set(e.code, sym);
      client.current?.key(sym, true);
    },
    onKeyUp: (e: React.KeyboardEvent) => {
      const sym = pressed.get(e.code) ?? keysymOf(e);
      pressed.delete(e.code);
      if (sym === undefined) return;
      e.preventDefault();
      client.current?.key(sym, false);
    },
  };
}
