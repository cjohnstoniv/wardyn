/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { keysymOf } from "./keysym";
import { RfbClient, type RfbDisplay } from "./rfb";

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
