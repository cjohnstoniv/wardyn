/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/**
 * The browser terminal's clipboard gate.
 *
 * Code in the sandbox runs as the tmux owner's UID, so it can emit OSC 52
 * (`tmux set-buffer -w`) whenever it likes, and a pane that asked for mouse
 * tracking receives the user's drag itself, so tmux copies nothing and the pane
 * can answer with a payload of its own. An OSC 52 write is therefore an
 * untrusted clipboard REQUEST, never a copy. It becomes an offer only when its
 * text equals what the user's own drag, double-click or triple-click spanned in
 * the xterm buffer; the user then confirms with an explicit Copy. Nothing the
 * sandbox emits ever reads the clipboard or writes it directly.
 *
 * Measured against tmux: it sends `ESC]52;;<base64>` with an EMPTY selection
 * field (`@xterm/addon-clipboard` passes that through verbatim), so '' and 'c'
 * are the selections accepted.
 */
import type { IDisposable, ITerminalAddon } from "@xterm/xterm";
import { ClipboardAddon } from "@xterm/addon-clipboard";
import { TERMINAL_COPY } from "./wardyn/copy";

export const MAX_COPY_BYTES = 1024 * 1024;
/** A copy gesture authorises an OSC 52 for this long after it ends. tmux's own
 *  double/triple-click copy runs about 0.3s after the click. */
export const GESTURE_WINDOW_MS = 1000;
export const OFFER_TTL_MS = 10_000;

/** macOS gets Option+drag and Cmd+C; every other platform Shift+drag and Ctrl+C. */
export const isMacPlatform = () => /Mac/i.test(navigator.platform);

export interface CopyOffer {
  id: number;
  text: string;
  lineBreaks: number;
  /** Code points the preview must show as ⟨U+XXXX⟩ (format and control characters). */
  invisible: number;
  /** Code points in the text. */
  chars: number;
}

interface CellLike {
  getChars(): string;
  getWidth(): number;
}
interface LineLike {
  isWrapped: boolean;
  getCell(x: number): CellLike | undefined;
}
/** The slice of xterm's Terminal the gate uses; the real Terminal satisfies it. */
export interface GateTerm {
  cols: number;
  rows: number;
  element?: HTMLElement;
  buffer: { active: { viewportY: number; getLine(y: number): LineLike | undefined } };
  loadAddon(addon: ITerminalAddon): void;
  parser?: {
    registerOscHandler(ident: number, cb: (data: string) => boolean | Promise<boolean>): IDisposable;
  };
}

export interface CopyGateOptions {
  term: GateTerm;
  /** True only while THIS socket is the open writer (not an observer, not between sockets). */
  isWriter(): boolean;
  onOffer(offer: CopyOffer | null): void;
  onNotice(message: string | null): void;
  now?: () => number;
}

export interface CopyGate {
  /** Drop a pending offer (the user acted on it, or dismissed it). */
  dismiss(): void;
  /** Connection or role changed: forget the gesture, the offer and the notice. */
  reset(): void;
  dispose(): void;
}

type Cell = { row: number; col: number };
type Snap = { cells: string[]; wrapped: boolean }[];
type Kind = "drag" | "word" | "line";
interface Gesture {
  kind: Kind;
  start: Cell;
  end: Cell;
  at: number;
  used: boolean;
  /** The screen before the gesture and at its end: tmux repaints (copy-mode
   *  indicator, selection) between the two, so either is what was selected. */
  snaps: Snap[];
}

// tmux's word-separators default; whitespace always separates.
const TMUX_SEPARATORS = "!\"#$%&'()*+,-./:;<=>?@[\\]^`{|}~";

/** Trailing whitespace per line and trailing blank lines are not part of a tmux copy. */
export function normalizeCopy(text: string): string {
  return text
    .split("\n")
    .map((l) => l.replace(/[ \t]+$/, ""))
    .join("\n")
    .replace(/\n+$/, "");
}

export function snapshotScreen(term: GateTerm): Snap {
  const buf = term.buffer.active;
  const out: Snap = [];
  for (let r = 0; r < term.rows; r++) {
    const line = buf.getLine(buf.viewportY + r);
    const cells: string[] = [];
    for (let c = 0; c < term.cols; c++) {
      const cell = line?.getCell(c);
      cells.push(!cell ? " " : cell.getWidth() === 0 ? "" : cell.getChars() || " ");
    }
    out.push({ cells, wrapped: !!line?.isWrapped });
  }
  return out;
}

function rowText(snap: Snap, row: number, from = 0, to = Infinity): string {
  return snap[row].cells.slice(from, to + 1).join("");
}

function inOrder(a: Cell, b: Cell): [Cell, Cell] {
  return a.row < b.row || (a.row === b.row && a.col <= b.col) ? [a, b] : [b, a];
}

// A run of rows joined where the screen marks the next row as a wrap of this one.
function joinRows(snap: Snap, pieces: { row: number; text: string }[], joinWrapped: boolean): string {
  let out = "";
  pieces.forEach((p, i) => {
    if (i > 0) out += joinWrapped && snap[p.row].wrapped ? "" : "\n";
    out += p.text;
  });
  return out;
}

function wordClass(ch: string): number {
  if (ch === " " || ch === "") return 0;
  return TMUX_SEPARATORS.includes(ch) ? 1 : 2;
}

/** Every text a tmux copy of this gesture can equal, from one screen snapshot. */
function candidates(g: Gesture, snap: Snap): string[] {
  const out: string[] = [];
  const cols = snap[0]?.cells.length ?? 0;
  if (g.kind === "drag") {
    const [first, last] = inOrder(g.start, g.end);
    // Measured against tmux 3.4: a drag released on cell N copies up to N-1, so
    // the span without its last (or first) cell is a copy of the same drag.
    const before = (c: Cell): Cell => (c.col > 0 ? { row: c.row, col: c.col - 1 } : { row: c.row - 1, col: cols - 1 });
    const after = (c: Cell): Cell => (c.col < cols - 1 ? { row: c.row, col: c.col + 1 } : { row: c.row + 1, col: 0 });
    for (const [a, b] of [[first, last], [first, before(last)], [after(first), last]]) {
      if (a.row < 0 || b.row >= snap.length || inOrder(a, b)[0] !== a) continue;
      const pieces: { row: number; text: string }[] = [];
      for (let r = a.row; r <= b.row; r++) {
        pieces.push({ row: r, text: rowText(snap, r, r === a.row ? a.col : 0, r === b.row ? b.col : cols - 1) });
      }
      out.push(joinRows(snap, pieces, false), joinRows(snap, pieces, true));
    }
  } else if (g.kind === "line") {
    let top = g.start.row;
    let bottom = g.start.row;
    while (top > 0 && snap[top].wrapped) top--;
    while (bottom < snap.length - 1 && snap[bottom + 1].wrapped) bottom++;
    const pieces: { row: number; text: string }[] = [];
    for (let r = top; r <= bottom; r++) pieces.push({ row: r, text: rowText(snap, r) });
    out.push(joinRows(snap, pieces, false), joinRows(snap, pieces, true));
  } else {
    const chars = snap[g.start.row].cells;
    const at = g.start.col;
    // tmux's classes (word / separator run), and plain whitespace-delimited.
    for (const cls of [(ch: string) => wordClass(ch), (ch: string) => (wordClass(ch) === 0 ? 0 : 2)]) {
      const k = cls(chars[at] ?? " ");
      let from = at;
      let to = at;
      while (from > 0 && cls(chars[from - 1]) === k) from--;
      while (to < cols - 1 && cls(chars[to + 1]) === k) to++;
      out.push(rowText(snap, g.start.row, from, to));
    }
  }
  return out;
}

const utf8Bytes = (s: string) => new TextEncoder().encode(s).length;
// C0 controls other than \n and \t.
const BAD_CONTROL = /[\x00-\x08\x0b-\x1f]/;

export function createCopyGate(opts: CopyGateOptions): CopyGate {
  const { term, isWriter, onOffer, onNotice } = opts;
  const now = opts.now ?? (() => performance.now());

  let gesture: Gesture | null = null;
  let offer: CopyOffer | null = null;
  let offerId = 0;
  let offerTimer: ReturnType<typeof setTimeout> | null = null;
  const cleanups: Array<() => void> = [];

  const dismiss = () => {
    if (offerTimer) clearTimeout(offerTimer);
    offerTimer = null;
    if (offer) {
      offer = null;
      onOffer(null);
    }
  };
  const drop = (why: string, withNotice: boolean) => {
    console.debug(`terminal clipboard: OSC 52 dropped: ${why}`);
    if (withNotice) onNotice(TERMINAL_COPY.BLOCKED);
  };

  // The provider: reads are refused outright; a write is only ever a request.
  // It is checked against the two gesture-time snapshots only. The live buffer
  // is never evidence: a pane can repaint cells to make its own payload true.
  const provider = {
    readText: () => "",
    writeText: (selection: string, text: string) => request(selection, text),
  };

  function request(selection: string, text: string) {
    if (!isWriter()) return; // an observer never gets an offer
    const g = gesture;
    const live = !!g && !g.used && now() - g.at <= GESTURE_WINDOW_MS;
    if (!live || !g) return drop("no copy gesture in the last second", false);
    if (selection !== "" && selection !== "c") return drop(`selection "${selection}"`, true);
    if (text === "") return drop("empty payload", false);
    if (utf8Bytes(text) > MAX_COPY_BYTES) return drop("payload over 1 MiB", true);
    if (BAD_CONTROL.test(text)) return drop("control bytes in payload", true);
    const want = normalizeCopy(text);
    const matches = g.snaps.some((s) =>
      candidates(g, s).some((c) => normalizeCopy(c) === want),
    );
    if (!matches) return drop("payload differs from the user's selection", true);
    if (offer) {
      // The card already on screen is the notice; M11 words no second one.
      return console.debug("terminal clipboard: OSC 52 dropped: an offer is already pending");
    }
    g.used = true;
    const parts = visibleParts(text);
    offer = {
      id: ++offerId,
      text,
      lineBreaks: text.split("\n").length - 1,
      invisible: parts.filter((p) => p.hidden).length,
      chars: [...text].length,
    };
    onNotice(null);
    onOffer(offer);
    offerTimer = setTimeout(dismiss, OFFER_TTL_MS);
  }

  // The addon decodes OSC 52 and hands the text to the provider. It also answers
  // a QUERY (`?`) by typing a reply into the PTY, even when the provider returns
  // "". A handler registered after the addon runs first, so swallow queries here.
  term.loadAddon(new ClipboardAddon(undefined, provider));
  const queryGuard = term.parser?.registerOscHandler(52, (data) => data.split(";")[1] === "?");
  if (queryGuard) cleanups.push(() => queryGuard.dispose());

  // Gesture record: only the user's own pointer over the grid counts. Typing,
  // and anything the pane prints, never does.
  const screen = term.element?.querySelector<HTMLElement>(".xterm-screen");
  if (screen) {
    const cellAt = (e: MouseEvent): Cell => {
      const rect = screen.getBoundingClientRect();
      const col = Math.floor(((e.clientX - rect.left) / (rect.width || 1)) * term.cols);
      const row = Math.floor(((e.clientY - rect.top) / (rect.height || 1)) * term.rows);
      return { col: Math.min(term.cols - 1, Math.max(0, col)), row: Math.min(term.rows - 1, Math.max(0, row)) };
    };
    // Shift/Option+drag is xterm's own selection, which tmux never sees.
    const native = (e: MouseEvent) => e.shiftKey || (e.altKey && isMacPlatform());
    let down: { cell: Cell; snap: Snap } | null = null;
    const onUp = (e: MouseEvent) => {
      window.removeEventListener("mouseup", onUp, true);
      const d = down;
      down = null;
      if (!d || e.button !== 0 || !isWriter()) return;
      const end = cellAt(e);
      const kind: Kind | null =
        e.detail >= 3 ? "line" : e.detail === 2 ? "word" : d.cell.row !== end.row || d.cell.col !== end.col ? "drag" : null;
      if (!kind) return;
      gesture = { kind, start: d.cell, end: kind === "drag" ? end : d.cell, at: now(), used: false, snaps: [d.snap, snapshotScreen(term)] };
    };
    const onDown = (e: MouseEvent) => {
      if (e.button !== 0 || native(e)) return;
      down = { cell: cellAt(e), snap: snapshotScreen(term) };
      window.addEventListener("mouseup", onUp, true);
    };
    screen.addEventListener("mousedown", onDown, true);
    cleanups.push(() => {
      screen.removeEventListener("mousedown", onDown, true);
      window.removeEventListener("mouseup", onUp, true);
    });
  }

  const reset = () => {
    gesture = null;
    dismiss();
    onNotice(null);
  };
  return {
    dismiss,
    reset,
    dispose: () => {
      reset();
      cleanups.splice(0).forEach((f) => f());
    },
  };
}

export interface PreviewPart {
  text: string;
  /** A ⟨U+XXXX⟩ marker for a character that would otherwise not show. */
  hidden: boolean;
}

/** The payload as the offer card shows it (M11 D2): ↵ at a line end, → for a tab,
 *  and ⟨U+XXXX⟩ for each control, format (Unicode Cf) or byte-order-mark code
 *  point. Never truncates. */
export function visibleParts(text: string): PreviewPart[] {
  const out: PreviewPart[] = [];
  const plain = (t: string) => {
    const last = out[out.length - 1];
    if (last && !last.hidden) last.text += t;
    else out.push({ text: t, hidden: false });
  };
  for (const ch of text) {
    const cp = ch.codePointAt(0)!;
    if (ch === "\n") plain("↵\n");
    else if (ch === "\t") plain("→");
    else if (cp < 0x20 || (cp >= 0x7f && cp <= 0x9f) || cp === 0xfeff || /^\p{Cf}$/u.test(ch))
      out.push({ text: `⟨U+${cp.toString(16).toUpperCase().padStart(4, "0")}⟩`, hidden: true });
    else plain(ch);
  }
  return out;
}

export const renderVisible = (text: string): string =>
  visibleParts(text)
    .map((p) => p.text)
    .join("");
