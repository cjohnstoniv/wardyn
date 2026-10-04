/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Terminal } from "@xterm/xterm";

const addons: { lose: () => void; dispose: ReturnType<typeof vi.fn> }[] = [];
vi.mock("@xterm/addon-webgl", () => ({
  WebglAddon: class {
    private cb: () => void = () => {};
    dispose = vi.fn();
    constructor() {
      addons.push({ lose: () => this.cb(), dispose: this.dispose });
    }
    onContextLoss(cb: () => void) {
      this.cb = cb;
    }
  },
}));

import { createRenderer, readRendererPref, writeRendererPref } from "./attach-terminal-renderer";

const term = { loadAddon: vi.fn() } as unknown as Terminal;

beforeEach(() => {
  addons.length = 0;
  localStorage.clear();
  vi.mocked(term.loadAddon).mockReset();
  HTMLCanvasElement.prototype.getContext = vi.fn(() => ({})) as never;
});

describe("renderer preference", () => {
  it("defaults to auto and round-trips the stored choice", () => {
    expect(readRendererPref()).toBe("auto");
    writeRendererPref("gpu");
    expect(readRendererPref()).toBe("gpu");
    writeRendererPref("auto");
    expect(localStorage.getItem("wardyn.terminal.renderer")).toBeNull();
  });
});

describe("createRenderer", () => {
  it("loads the GPU addon on auto when WebGL2 exists, and falls back on context loss", () => {
    const states: unknown[] = [];
    createRenderer(term, "auto", (s) => states.push(s));
    expect(term.loadAddon).toHaveBeenCalledTimes(1);
    addons[0].lose();
    expect(addons[0].dispose).toHaveBeenCalled();
    expect(states).toEqual([
      { active: "gpu", fellBack: false },
      { active: "compatible", fellBack: true },
    ]);
  });

  it("never loads the addon on compatible", () => {
    const states: unknown[] = [];
    createRenderer(term, "compatible", (s) => states.push(s));
    expect(term.loadAddon).not.toHaveBeenCalled();
    expect(states).toEqual([{ active: "compatible", fellBack: false }]);
  });

  it("switches live, and reports a fall-back when the addon cannot load", () => {
    const states: unknown[] = [];
    const ctl = createRenderer(term, "compatible", (s) => states.push(s));
    vi.mocked(term.loadAddon).mockImplementation(() => {
      throw new Error("no webgl");
    });
    ctl.set("gpu");
    expect(states.at(-1)).toEqual({ active: "compatible", fellBack: true });
  });
});
