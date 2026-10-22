/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { keysymOf } from "./keysym";
import { MAX_CUT_TEXT, MAX_DIM, MAX_RAW_BYTES, MAX_TEXT, RfbClient, type RfbDisplay } from "./rfb";

const ascii = (s: string) => new TextEncoder().encode(s);
const u16 = (n: number) => [n >> 8, n & 0xff];
const u32 = (n: number) => [(n >>> 24) & 0xff, (n >>> 16) & 0xff, (n >>> 8) & 0xff, n & 0xff];
const s32 = (n: number) => u32(n >>> 0);

function rig() {
  const sent: number[][] = [];
  const calls: string[] = [];
  const events: string[] = [];
  const display: RfbDisplay = {
    resize: (w, h) => calls.push(`resize ${w}x${h}`),
    putRaw: (x, y, w, h, px) => calls.push(`raw ${x},${y} ${w}x${h} ${[...px].join(".")}`),
    copy: (sx, sy, x, y, w, h) => calls.push(`copy ${sx},${sy} -> ${x},${y} ${w}x${h}`),
  };
  const rfb = new RfbClient((b) => sent.push([...b]), display, {
    connected: (name, w, h) => events.push(`connected ${name} ${w}x${h}`),
    failed: (why) => events.push(`failed ${why}`),
  });
  return { rfb, sent, calls, events };
}

const SERVER_INIT = [...u16(2), ...u16(1), ...new Array(16).fill(0), ...u32(3), ...ascii("vm1")];

describe("RfbClient", () => {
  it("handshakes without a password, asks for true colour and Raw/CopyRect, and requests the whole screen", () => {
    const { rfb, sent, events } = rig();
    rfb.receive(ascii("RFB 003.008\n"));
    expect(new TextDecoder().decode(Uint8Array.from(sent[0]))).toBe("RFB 003.008\n");
    rfb.receive(Uint8Array.of(2, 2, 1)); // VNC auth and None offered
    expect(sent[1]).toEqual([1]);
    rfb.receive(Uint8Array.of(...u32(0)));
    expect(sent[2]).toEqual([1]); // ClientInit, shared
    rfb.receive(Uint8Array.from(SERVER_INIT));
    expect(events).toEqual(["connected vm1 2x1"]);
    expect(sent[3]).toEqual([0, 0, 0, 0, 32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 0, 8, 16, 0, 0, 0]);
    expect(sent[4]).toEqual([2, 0, ...u16(3), ...s32(1), ...s32(0), ...s32(-223)]);
    expect(sent[5]).toEqual([3, 0, 0, 0, 0, 0, ...u16(2), ...u16(1)]);
  });

  it("draws Raw and CopyRect rectangles however the bytes are split, drops the desktop's cut text, and asks for more", () => {
    const { rfb, sent, calls } = rig();
    rfb.receive(Uint8Array.from([...ascii("RFB 003.008\n"), 1, 1, ...u32(0), ...SERVER_INIT]));
    const update = [0, 0, ...u16(2),
      ...u16(0), ...u16(0), ...u16(2), ...u16(1), ...s32(0), 1, 2, 3, 0, 4, 5, 6, 0,
      ...u16(1), ...u16(0), ...u16(1), ...u16(1), ...s32(1), ...u16(0), ...u16(0)];
    const cut = [3, 0, 0, 0, ...u32(5), ...ascii("hello")];
    const bytes = Uint8Array.from([...cut, ...update]);
    calls.length = 0;
    const before = sent.length;
    for (const b of bytes) rfb.receive(Uint8Array.of(b));
    expect(calls).toEqual(["raw 0,0 2x1 1.2.3.0.4.5.6.0", "copy 0,0 -> 1,0 1x1"]);
    expect(sent.slice(before)).toEqual([[3, 1, 0, 0, 0, 0, ...u16(2), ...u16(1)]]);
  });

  it("follows a desktop resize", () => {
    const { rfb, calls } = rig();
    rfb.receive(Uint8Array.from([...ascii("RFB 003.008\n"), 1, 1, ...u32(0), ...SERVER_INIT]));
    rfb.receive(Uint8Array.from([0, 0, ...u16(1), ...u16(0), ...u16(0), ...u16(800), ...u16(600), ...s32(-223)]));
    expect(calls.at(-1)).toBe("resize 800x600");
    expect(rfb.width).toBe(800);
  });

  it("speaks 3.3 to an older server, and gives up on a desktop that wants a password or refuses", () => {
    const old = rig();
    old.rfb.receive(Uint8Array.from([...ascii("RFB 003.003\n"), ...u32(1), ...SERVER_INIT]));
    expect(new TextDecoder().decode(Uint8Array.from(old.sent[0]))).toBe("RFB 003.003\n");
    expect(old.events).toEqual(["connected vm1 2x1"]);

    const pw = rig();
    pw.rfb.receive(Uint8Array.from([...ascii("RFB 003.008\n"), 1, 2]));
    expect(pw.events).toEqual(["failed the desktop asks for a password"]);

    const no = rig();
    no.rfb.receive(Uint8Array.from([...ascii("RFB 003.008\n"), 0, ...u32(4), ...ascii("busy")]));
    expect(no.events).toEqual(["failed busy"]);
  });

  it("sends keys and the pointer only once connected, inside the screen", () => {
    const { rfb, sent } = rig();
    rfb.key(0x61, true);
    expect(sent).toEqual([]);
    rfb.receive(Uint8Array.from([...ascii("RFB 003.008\n"), 1, 1, ...u32(0), ...SERVER_INIT]));
    const before = sent.length;
    rfb.key(0xff0d, true);
    rfb.pointer(5, -3, 1);
    expect(sent.slice(before)).toEqual([[4, 1, 0, 0, ...u32(0xff0d)], [5, 1, ...u16(1), ...u16(0)]]);
  });
});

// Each cap fails the connection on the header alone: the oversized body is never awaited, so nothing is held.
describe("RfbClient bounds what a desktop can make it hold", () => {
  const connected = (init = SERVER_INIT) => {
    const r = rig();
    r.rfb.receive(Uint8Array.from([...ascii("RFB 003.008\n"), 1, 1, ...u32(0), ...init]));
    return r;
  };
  const filler = new Uint8Array(64 << 10);
  const init = (w: number, h: number, name: number[] = []) => [...u16(w), ...u16(h), ...new Array(16).fill(0), ...u32(name.length), ...name];

  it("keeps received chunks without re-joining them, and handles a message split across them", () => {
    const { rfb, calls } = connected();
    const update = [0, 0, ...u16(1), ...u16(0), ...u16(0), ...u16(2), ...u16(1), ...s32(0), 1, 2, 3, 0, 4, 5, 6, 0];
    rfb.receive(Uint8Array.from(update.slice(0, 5)));
    const middle = Uint8Array.from(update.slice(5, 19));
    rfb.receive(middle);
    expect(rfb.buffered).toBe(15);
    middle[13] = 9; // the chunk is held as it came, not copied into a joined buffer
    rfb.receive(Uint8Array.from(update.slice(19)));
    expect(calls.at(-1)).toBe("raw 0,0 2x1 1.2.9.0.4.5.6.0");
    expect(rfb.buffered).toBe(0);
  });

  it("refuses a Raw rectangle outside the negotiated screen, or over the byte budget", () => {
    const out = connected();
    out.rfb.receive(Uint8Array.from([0, 0, ...u16(1), ...u16(1), ...u16(0), ...u16(2), ...u16(1), ...s32(0)]));
    out.rfb.receive(filler);
    expect(out.events.at(-1)).toBe("failed the desktop sent a rectangle outside its screen");
    expect(out.rfb.buffered).toBe(0);

    const big = connected(init(MAX_DIM, MAX_DIM));
    const side = Math.ceil(Math.sqrt(MAX_RAW_BYTES / 4)) + 1; // inside the screen, over the budget
    big.rfb.receive(Uint8Array.from([0, 0, ...u16(1), ...u16(0), ...u16(0), ...u16(side), ...u16(side), ...s32(0)]));
    big.rfb.receive(filler);
    expect(big.events.at(-1)).toBe("failed the desktop sent a rectangle outside its screen");
    expect(big.rfb.buffered).toBe(0);
  });

  it("refuses a desktop name over MAX_TEXT", () => {
    const r = rig();
    r.rfb.receive(Uint8Array.from([...ascii("RFB 003.008\n"), 1, 1, ...u32(0), ...u16(2), ...u16(1), ...new Array(16).fill(0), ...u32(MAX_TEXT + 1)]));
    r.rfb.receive(filler);
    expect(r.events).toEqual([`failed the desktop's name is over ${MAX_TEXT} bytes`]);
    expect(r.rfb.buffered).toBe(0);
  });

  it("refuses cut text over MAX_CUT_TEXT, and an extended-clipboard length", () => {
    for (const len of [MAX_CUT_TEXT + 1, -8 >>> 0]) {
      const r = connected();
      r.rfb.receive(Uint8Array.from([3, 0, 0, 0, ...u32(len)]));
      r.rfb.receive(filler);
      expect(r.events.at(-1)).toBe(`failed the desktop sent over ${MAX_CUT_TEXT} bytes of cut text`);
      expect(r.rfb.buffered).toBe(0);
    }
  });

  it("refuses a screen over MAX_DIM a side, in ServerInit and in DesktopSize", () => {
    const r = rig();
    r.rfb.receive(Uint8Array.from([...ascii("RFB 003.008\n"), 1, 1, ...u32(0), ...init(MAX_DIM + 1, 1)]));
    expect(r.events).toEqual([`failed the desktop is larger than ${MAX_DIM} pixels a side`]);

    const d = connected();
    d.rfb.receive(Uint8Array.from([0, 0, ...u16(1), ...u16(0), ...u16(0), ...u16(1), ...u16(MAX_DIM + 1), ...s32(-223)]));
    expect(d.events.at(-1)).toBe(`failed the desktop is larger than ${MAX_DIM} pixels a side`);
    expect(d.calls).not.toContain(`resize 1x${MAX_DIM + 1}`);
  });
});

describe("keysymOf", () => {
  it.each([
    [{ key: "a", location: 0 }, 0x61],
    [{ key: "é", location: 0 }, 0xe9],
    [{ key: "€", location: 0 }, 0x010020ac],
    [{ key: "Enter", location: 0 }, 0xff0d],
    [{ key: "F12", location: 0 }, 0xffc9],
    [{ key: "Shift", location: 2 }, 0xffe2],
    [{ key: "Control", location: 1 }, 0xffe3],
    [{ key: "Dead", location: 0 }, undefined],
  ])("%o -> %s", (e, want) => expect(keysymOf(e)).toBe(want));
});
