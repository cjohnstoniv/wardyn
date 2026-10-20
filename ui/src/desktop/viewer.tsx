/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The desktop viewer (mock M6: M6-Desktop, M6-States, M6-Phone), served by the UI gateway on its own address
// (internal/api/uigateway_desktop.go). It has no console session: what it needs rides data-wardyn-desktop.

import * as React from "react";
import { Button } from "../app/components/ui/button";
import { Chip } from "../app/components/wardyn/primitives";
import { DESKTOP } from "../app/components/wardyn/copy/desktop";
import { keysymOf } from "./keysym";
import { RfbClient, type RfbDisplay } from "./rfb";

export type Boot = { state: "open"; expires: number; rfb: string; close: string } | { state: "refused"; said: string };

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

const openSocket = (path: string): DesktopSocket =>
  new WebSocket(`${location.protocol === "https:" ? "wss:" : "ws:"}//${location.host}${path}`, ["binary"]);

// The gateway's close codes (uigateway_desktop.go): an ended session or run is final; anything else retries.
const SESSION_ENDED = 4001;
const RUN_ENDED = 4002;
const NOT_AUTHORIZED = 4003;

type Phase = "starting" | "connected" | "closed" | "expired" | "run_ended";

export function DesktopViewer({ boot, connect = openSocket, small }: { boot: Boot; connect?: (path: string) => DesktopSocket; small?: boolean }) {
  if (boot.state === "refused") return <LinkRefused said={boot.said} />;
  return <Desktop boot={boot} connect={connect} viewOnly={small ?? window.matchMedia?.("(max-width: 640px)").matches ?? false} />;
}

function Desktop({ boot, connect, viewOnly }: { boot: Extract<Boot, { state: "open" }>; connect: (path: string) => DesktopSocket; viewOnly: boolean }) {
  const canvas = React.useRef<HTMLCanvasElement>(null);
  const client = React.useRef<RfbClient | null>(null);
  const socket = React.useRef<DesktopSocket | null>(null);
  const closing = React.useRef(false);
  const [phase, setPhase] = React.useState<Phase>("starting");
  const [fit, setFit] = React.useState(true);
  const pressed = React.useRef(new Map<string, number>()); // the keysym each held key went down with

  React.useEffect(() => {
    let retry: ReturnType<typeof setTimeout> | undefined;
    let delay = 1000;
    let live = true;
    const dial = () => {
      const ws = connect(boot.rfb);
      socket.current = ws;
      ws.binaryType = "arraybuffer";
      const rfb = new RfbClient((b) => ws.send(b), canvasDisplay(canvas), {
        connected: () => {
          delay = 1000;
          setPhase("connected");
        },
        failed: () => ws.close(),
      });
      client.current = rfb;
      ws.onmessage = (ev) => rfb.receive(ev.data as ArrayBuffer);
      ws.onclose = (ev) => {
        client.current = null;
        if (!live) return;
        if (closing.current || ev.code === NOT_AUTHORIZED) setPhase("closed");
        else if (ev.code === SESSION_ENDED) setPhase("expired");
        else if (ev.code === RUN_ENDED) setPhase("run_ended");
        else {
          // Booting, restarting or briefly unreachable: the same tab connects on its own.
          setPhase("starting");
          retry = setTimeout(dial, delay);
          delay = Math.min(delay * 2, 10_000);
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

  const close = async () => {
    closing.current = true;
    socket.current?.close();
    setPhase("closed");
    await fetch(boot.close, { method: "POST", credentials: "same-origin" }).catch(() => undefined);
    window.close();
  };

  const input = viewOnly || phase !== "connected" ? {} : inputHandlers(canvas, client, pressed.current);
  const ended = phase === "closed" || phase === "expired" || phase === "run_ended";
  return (
    <div className="flex h-dvh flex-col bg-background text-foreground">
      <header className="flex flex-wrap items-center gap-3 border-b border-border px-4 py-2 text-xs">
        {phase === "connected" && <Chip tone="success" dot>{DESKTOP.connected}</Chip>}
        <span className="font-medium">{DESKTOP.title}</span>
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
      {viewOnly && (
        <p className="border-b border-border px-4 py-2 text-xs">
          <span className="font-medium">{DESKTOP.phoneTitle}</span> <span className="text-muted-foreground">{DESKTOP.phoneBody}</span>
        </p>
      )}
      <main className={fit ? "relative flex min-h-0 flex-1 items-center justify-center bg-black" : "relative min-h-0 flex-1 overflow-auto bg-black"}>
        <canvas
          ref={canvas}
          data-testid="desktop-canvas"
          tabIndex={viewOnly ? -1 : 0}
          className={fit ? "max-h-full max-w-full object-contain" : undefined}
          hidden={phase !== "connected"}
          onContextMenu={(e) => e.preventDefault()}
          {...input}
        />
        {phase !== "connected" && <Notice phase={phase} />}
      </main>
    </div>
  );
}

function Notice({ phase }: { phase: Exclude<Phase, "connected"> }) {
  const [title, body] =
    phase === "starting" ? [DESKTOP.starting, DESKTOP.startingBody]
    : phase === "expired" ? [DESKTOP.sessionEnded, DESKTOP.sessionEndedBody]
    : phase === "run_ended" ? [DESKTOP.runEnded, undefined]
    : [DESKTOP.sessionEnded, undefined];
  return (
    <div role="status" className="absolute inset-0 flex flex-col items-center justify-center gap-2 p-6 text-center">
      <p className="text-sm font-medium text-white">{title}</p>
      {body && <p className="max-w-md text-xs text-neutral-400">{body}</p>}
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

function inputHandlers(canvas: React.RefObject<HTMLCanvasElement | null>, client: React.RefObject<RfbClient | null>, pressed: Map<string, number>) {
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
  return {
    onPointerMove: pointer,
    onPointerDown: (e: React.PointerEvent) => {
      canvas.current?.focus();
      pointer(e);
    },
    onPointerUp: pointer,
    onWheel: (e: React.WheelEvent) => {
      const [x, y] = at(e);
      const wheel = e.deltaY < 0 ? 8 : 16;
      client.current?.pointer(x, y, rfbButtons(e.buttons) | wheel);
      client.current?.pointer(x, y, rfbButtons(e.buttons));
    },
    onKeyDown: (e: React.KeyboardEvent) => {
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
