/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A small RFB 3.3/3.7/3.8 client (RFC 6143) for the desktop of a Linux VM: QEMU's VNC server on the runner,
// relayed by the UI gateway over one WebSocket. It asks for no authentication (the socket is reachable only
// through the gateway's session), 32-bit true colour, and the Raw and CopyRect encodings plus DesktopSize.
// Cut text from the desktop is read and dropped: copy out is blocked.

/** Where the framebuffer lands. Pixels arrive as R, G, B, unused, per pixel. */
export interface RfbDisplay {
  resize(width: number, height: number): void;
  putRaw(x: number, y: number, width: number, height: number, rgbx: Uint8Array): void;
  copy(srcX: number, srcY: number, x: number, y: number, width: number, height: number): void;
}

export interface RfbEvents {
  connected(name: string, width: number, height: number): void;
  failed(reason: string): void;
}

const ENC_RAW = 0;
const ENC_COPYRECT = 1;
const ENC_DESKTOPSIZE = -223;
const SEC_NONE = 1;

type State = "version" | "security" | "security33" | "result" | "init" | "normal" | "failed";

export class RfbClient {
  private buf = new Uint8Array(0);
  private state: State = "version";
  private minor = 8;
  private rects = 0; // rectangles still to read of the current framebuffer update
  width = 0;
  height = 0;

  constructor(
    private readonly send: (bytes: Uint8Array) => void,
    private readonly display: RfbDisplay,
    private readonly events: RfbEvents,
  ) {}

  /** Feeds bytes from the server; whole messages are handled as soon as they are in. */
  receive(chunk: ArrayBuffer | Uint8Array): void {
    const data = chunk instanceof Uint8Array ? chunk : new Uint8Array(chunk);
    const next = new Uint8Array(this.buf.length + data.length);
    next.set(this.buf);
    next.set(data, this.buf.length);
    this.buf = next;
    while (this.state !== "failed" && this.step()) {
      /* each step consumed one whole unit */
    }
  }

  /** One X11 keysym going down or up. */
  key(keysym: number, down: boolean): void {
    if (this.state !== "normal") return;
    const m = new Uint8Array(8);
    const v = new DataView(m.buffer);
    m[0] = 4;
    m[1] = down ? 1 : 0;
    v.setUint32(4, keysym >>> 0);
    this.send(m);
  }

  /** The pointer at (x, y) in framebuffer pixels, with buttons as RFB's mask (1 left, 2 middle, 4 right, 8/16 wheel). */
  pointer(x: number, y: number, buttons: number): void {
    if (this.state !== "normal") return;
    const m = new Uint8Array(6);
    const v = new DataView(m.buffer);
    m[0] = 5;
    m[1] = buttons & 0xff;
    v.setUint16(2, clamp(x, this.width));
    v.setUint16(4, clamp(y, this.height));
    this.send(m);
  }

  private fail(reason: string): false {
    this.state = "failed";
    this.events.failed(reason);
    return false;
  }

  private take(n: number): Uint8Array | null {
    if (this.buf.length < n) return null;
    const out = this.buf.slice(0, n);
    this.buf = this.buf.subarray(n);
    return out;
  }

  private peek(n: number): DataView | null {
    return this.buf.length < n ? null : new DataView(this.buf.buffer, this.buf.byteOffset, n);
  }

  private step(): boolean {
    switch (this.state) {
      case "version": {
        const b = this.take(12);
        if (!b) return false;
        const m = /^RFB (\d{3})\.(\d{3})\n$/.exec(new TextDecoder().decode(b));
        if (!m || Number(m[1]) !== 3) return this.fail("not an RFB server");
        this.minor = Number(m[2]) >= 8 ? 8 : Number(m[2]) >= 7 ? 7 : 3;
        this.send(new TextEncoder().encode(`RFB 003.00${this.minor}\n`));
        this.state = this.minor === 3 ? "security33" : "security";
        return true;
      }
      case "security33": {
        const v = this.peek(4);
        if (!v) return false;
        const type = v.getUint32(0);
        if (type === 0) return this.reason(4);
        this.take(4);
        if (type !== SEC_NONE) return this.fail("the desktop asks for a password");
        return this.clientInit();
      }
      case "security": {
        const v = this.peek(1);
        if (!v) return false;
        const n = v.getUint8(0);
        if (n === 0) return this.reason(1);
        const b = this.take(1 + n);
        if (!b) return false;
        if (!b.subarray(1).includes(SEC_NONE)) return this.fail("the desktop asks for a password");
        this.send(Uint8Array.of(SEC_NONE));
        if (this.minor === 8) {
          this.state = "result";
          return true;
        }
        return this.clientInit();
      }
      case "result": {
        const v = this.peek(4);
        if (!v) return false;
        if (v.getUint32(0) !== 0) return this.reason(4);
        this.take(4);
        return this.clientInit();
      }
      case "init": {
        const v = this.peek(24);
        if (!v) return false;
        const nameLen = v.getUint32(20);
        const b = this.take(24 + nameLen);
        if (!b) return false;
        const d = new DataView(b.buffer);
        this.width = d.getUint16(0);
        this.height = d.getUint16(2);
        this.send(setPixelFormat());
        this.send(setEncodings([ENC_COPYRECT, ENC_RAW, ENC_DESKTOPSIZE]));
        this.display.resize(this.width, this.height);
        this.state = "normal";
        this.events.connected(new TextDecoder().decode(b.subarray(24)), this.width, this.height);
        this.request(false);
        return true;
      }
      case "normal":
        return this.rects > 0 ? this.rect() : this.message();
      default:
        return false;
    }
  }

  /** A failure reason (u32 length, then text) after `skip` bytes, which ends the session. */
  private reason(skip: number): boolean {
    const v = this.peek(skip + 4);
    if (!v) return false;
    const len = v.getUint32(skip);
    const b = this.take(skip + 4 + len);
    if (!b) return false;
    return this.fail(new TextDecoder().decode(b.subarray(skip + 4)) || "the desktop refused the connection");
  }

  private clientInit(): true {
    this.send(Uint8Array.of(1)); // shared: another viewer of the same desktop is not cut off
    this.state = "init";
    return true;
  }

  private request(incremental: boolean): void {
    const m = new Uint8Array(10);
    const v = new DataView(m.buffer);
    m[0] = 3;
    m[1] = incremental ? 1 : 0;
    v.setUint16(6, this.width);
    v.setUint16(8, this.height);
    this.send(m);
  }

  private message(): boolean {
    const v = this.peek(1);
    if (!v) return false;
    switch (v.getUint8(0)) {
      case 0: {
        const h = this.take(4);
        if (!h) return false;
        this.rects = new DataView(h.buffer).getUint16(2);
        if (this.rects === 0) this.request(true);
        return true;
      }
      case 1: {
        const h = this.peek(6);
        if (!h) return false;
        return this.take(6 + h.getUint16(4) * 6) !== null;
      }
      case 2:
        this.take(1);
        return true;
      case 3: {
        const h = this.peek(8);
        if (!h) return false;
        return this.take(8 + h.getUint32(4)) !== null; // the desktop's cut text, dropped
      }
      default:
        return this.fail("the desktop sent a message this viewer does not know");
    }
  }

  private rect(): boolean {
    const h = this.peek(12);
    if (!h) return false;
    const [x, y, w, ht, enc] = [h.getUint16(0), h.getUint16(2), h.getUint16(4), h.getUint16(6), h.getInt32(8)];
    const size = enc === ENC_RAW ? w * ht * 4 : enc === ENC_COPYRECT ? 4 : enc === ENC_DESKTOPSIZE ? 0 : -1;
    if (size < 0) return this.fail("the desktop sent an encoding this viewer did not ask for");
    const b = this.take(12 + size);
    if (!b) return false;
    if (enc === ENC_RAW) this.display.putRaw(x, y, w, ht, b.subarray(12));
    else if (enc === ENC_COPYRECT) {
      const d = new DataView(b.buffer, 12, 4);
      this.display.copy(d.getUint16(0), d.getUint16(2), x, y, w, ht);
    } else {
      this.width = w;
      this.height = ht;
      this.display.resize(w, ht);
    }
    this.rects--;
    if (this.rects === 0) this.request(true);
    return true;
  }
}

const clamp = (n: number, max: number) => Math.max(0, Math.min(Math.round(n), Math.max(0, max - 1)));

/** 32 bits per pixel, depth 24, little-endian, true colour, red in the lowest byte: R, G, B, unused in memory. */
function setPixelFormat(): Uint8Array {
  const m = new Uint8Array(20);
  const v = new DataView(m.buffer);
  m.set([32, 24, 0, 1], 4);
  v.setUint16(8, 255);
  v.setUint16(10, 255);
  v.setUint16(12, 255);
  m.set([0, 8, 16], 14);
  return m;
}

function setEncodings(encodings: number[]): Uint8Array {
  const m = new Uint8Array(4 + encodings.length * 4);
  const v = new DataView(m.buffer);
  m[0] = 2;
  v.setUint16(2, encodings.length);
  encodings.forEach((e, i) => v.setInt32(4 + i * 4, e));
  return m;
}
