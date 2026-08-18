/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * The recording's voice.
 *
 * Speaks the captions the driver already puts on screen, so the finished video
 * explains what it is doing and why. OFF unless WARDYN_DEMO_VOICE=1 — with the
 * flag unset every export here is a no-op and the recorder behaves exactly as
 * it did before.
 *
 * HOW IT FITS: overlay.ts's caption()/chapter() call speak() with the same text
 * they render. Nothing else changes, and — deliberately — walkthrough.spec.ts is
 * not touched at all: captions added there are narrated automatically, with no
 * list to keep in sync. That matters here specifically, because this repo has
 * two hand-synced caption duplicates that have ALREADY drifted (FUNNEL_DEMOS vs
 * demo-catalog.ts, TASK.md vs DEMO_TASK), and a keyed narration module would
 * have been a third.
 *
 * TIMING: clips are not played during the take — the browser is recording
 * itself and has no speakers in the loop. Each spoken line is appended to a
 * timeline as {file, tMs} relative to narration zero, and scripts/record-demo.sh
 * mixes them onto the finished video at those offsets. Zero is set when
 * installOverlay runs, which is a few tens of ms after the context (and so
 * recordVideo) starts — small enough not to matter for lines held for seconds.
 *
 * FAILURE POSTURE: narration is cosmetic and must never be able to fail a
 * recording. A missing model, a dead renderer, a bad line — every one of them
 * degrades to a silent caption, never an exception.
 */

import { spawn, type ChildProcessWithoutNullStreams } from "node:child_process";
import { existsSync, mkdirSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const REPO = path.resolve(HERE, "../../..");
const VENV_PY = path.join(process.env.HOME ?? "", ".cache/wardyn-narrate/venv/bin/python");
const SERVER = path.join(REPO, "scripts/narrate-server.py");

/** Where the mux step looks for the timeline; beside console.webm. */
export const NARRATION_DIR =
  process.env.WARDYN_DEMO_WORK_DIR || path.join(REPO, "ui/test-results/demo-video");
const TIMELINE = path.join(NARRATION_DIR, "narration.json");

export interface Cue {
  /** Absolute path to the rendered wav. */
  file: string;
  /** Milliseconds from narration zero. */
  tMs: number;
  /** Clip length, so the mux can warn about overlap. */
  durMs: number;
  /** Kept for debugging a take that narrates the wrong thing. */
  text: string;
}

const on = () => process.env.WARDYN_DEMO_VOICE === "1";

let proc: ChildProcessWithoutNullStreams | undefined;
let dead = false;
let zero = 0;
let buffer = "";
const waiting: Array<(r: Record<string, unknown>) => void> = [];
const cues: Cue[] = [];

/**
 * Start the take's clock. Called by installOverlay; safe to call twice.
 *
 * Deliberately NOT gated on WARDYN_DEMO_VOICE: overlay.ts's fast-forward spans
 * are measured from this same zero and a --silent take still fast-forwards, so
 * the clock has to run even when nothing will ever be spoken.
 */
export function narrationZero(): void {
  if (zero === 0) zero = Date.now();
}

/** The instant every timeline in this take is measured from. 0 before install. */
export function narrationZeroMs(): number {
  return zero;
}

function ensureProc(): ChildProcessWithoutNullStreams | undefined {
  if (dead) return undefined;
  if (proc) return proc;
  if (!existsSync(VENV_PY) || !existsSync(SERVER)) {
    // No renderer installed: say so once, then stay quiet for the whole run.
    console.warn(`[narrator] disabled — no renderer at ${VENV_PY}`);
    dead = true;
    return undefined;
  }
  try {
    const p = spawn(VENV_PY, [SERVER], { stdio: ["pipe", "pipe", "pipe"] });
    p.stdout.setEncoding("utf8");
    p.stdout.on("data", (chunk: string) => {
      buffer += chunk;
      // One JSON reply per line, in request order.
      for (let nl = buffer.indexOf("\n"); nl >= 0; nl = buffer.indexOf("\n")) {
        const line = buffer.slice(0, nl).trim();
        buffer = buffer.slice(nl + 1);
        const resolve = waiting.shift();
        if (!resolve) continue;
        try {
          resolve(JSON.parse(line));
        } catch {
          resolve({ error: `unparseable reply: ${line.slice(0, 80)}` });
        }
      }
    });
    // The server reports its engine choice on stderr — worth seeing once.
    p.stderr.setEncoding("utf8");
    p.stderr.on("data", (d: string) => {
      const s = d.trim();
      if (s) console.warn(`[narrator] ${s}`);
    });
    p.on("error", () => {
      dead = true;
      waiting.splice(0).forEach((r) => r({ error: "renderer died" }));
    });
    p.on("exit", () => {
      dead = true;
      waiting.splice(0).forEach((r) => r({ error: "renderer exited" }));
    });
    proc = p;
    return p;
  } catch {
    dead = true;
    return undefined;
  }
}

/**
 * Render `text`, schedule it at the current moment, and return how long it runs.
 *
 * The caller holds its caption for at least this long so speech never runs over
 * the next visual. Returns 0 when narration is off or unavailable, which every
 * caller already treats as "use the normal beat".
 */
export async function speak(text: string): Promise<number> {
  if (!on() || !text.trim()) return 0;
  const p = ensureProc();
  if (!p) return 0;
  if (zero === 0) narrationZero();

  const tMs = Date.now() - zero;
  const reply = await new Promise<Record<string, unknown>>((resolve) => {
    // A hung renderer must not hang the take.
    const timer = setTimeout(() => {
      const i = waiting.indexOf(resolve);
      if (i >= 0) waiting.splice(i, 1);
      resolve({ error: "render timeout" });
    }, 30_000);
    waiting.push((r) => {
      clearTimeout(timer);
      resolve(r);
    });
    try {
      p.stdin.write(`${JSON.stringify({ text })}\n`);
    } catch {
      clearTimeout(timer);
      resolve({ error: "write failed" });
    }
  }).catch(() => ({ error: "render failed" }) as Record<string, unknown>);

  const file = typeof reply.file === "string" ? reply.file : "";
  const durMs = typeof reply.durMs === "number" ? reply.durMs : 0;
  if (!file || !durMs) {
    if (reply.error) console.warn(`[narrator] ${String(reply.error)} :: ${text.slice(0, 60)}`);
    return 0;
  }

  cues.push({ file, tMs, durMs, text });
  flush();
  return durMs;
}

/**
 * Persist the timeline. Written after EVERY cue rather than once at the end:
 * a take that dies in act 5 should still leave a muxable timeline for acts 1-4,
 * and there is no afterAll hook here to rely on (this module deliberately does
 * not touch the spec).
 */
function flush(): void {
  try {
    mkdirSync(NARRATION_DIR, { recursive: true });
    writeFileSync(TIMELINE, JSON.stringify({ zero, cues }, null, 2));
  } catch {
    /* the take matters more than the timeline */
  }
}

/** Close the renderer. Optional — the process exits with the test runner anyway. */
export function narrationEnd(): void {
  try {
    proc?.stdin.end();
  } catch {
    /* ignore */
  }
}
