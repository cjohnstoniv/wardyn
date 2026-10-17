/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/**
 * The terminal's renderer: the GPU (WebGL2) addon where it works, xterm's DOM
 * renderer otherwise. The choice is per browser (renderer capability belongs to
 * the device), kept in localStorage; "auto" takes the GPU only when WebGL2 is
 * there. A lost GPU context disposes the addon, which hands drawing back to the
 * DOM renderer with the terminal, its scrollback and its socket untouched.
 */
import type { Terminal } from "@xterm/xterm";
import { WebglAddon } from "@xterm/addon-webgl";
import { lsGet, lsSet } from "../lib/storage";

export type RendererPref = "auto" | "gpu" | "compatible";
export type ActiveRenderer = "gpu" | "compatible";

export interface RendererState {
  active: ActiveRenderer;
  /** True once a lost context, or a stored GPU choice without WebGL2, forced Compatible. */
  fellBack: boolean;
}

const KEY = "wardyn.terminal.renderer";

export function readRendererPref(): RendererPref {
  const v = lsGet(KEY);
  return v === "gpu" || v === "compatible" ? v : "auto";
}

export function writeRendererPref(pref: RendererPref): void {
  lsSet(KEY, pref === "auto" ? null : pref);
}

let webgl2: boolean | null = null;
export function webgl2Available(): boolean {
  if (webgl2 === null) {
    try {
      webgl2 = !!document.createElement("canvas").getContext("webgl2");
    } catch {
      webgl2 = false;
    }
  }
  return webgl2;
}

export interface RendererControl {
  /** Apply a preference to the live terminal. */
  set: (pref: RendererPref) => void;
  dispose: () => void;
}

/** Call after `term.open`. `onState` fires on every change, and once at the start. */
export function createRenderer(
  term: Terminal,
  initial: RendererPref,
  onState: (s: RendererState) => void,
): RendererControl {
  let addon: WebglAddon | null = null;

  const unload = () => {
    const a = addon;
    addon = null;
    try {
      a?.dispose();
    } catch {
      /* already disposed by the context-loss path */
    }
  };

  const set = (pref: RendererPref) => {
    unload();
    if (pref === "compatible") return onState({ active: "compatible", fellBack: false });
    if (!webgl2Available()) return onState({ active: "compatible", fellBack: pref === "gpu" });
    try {
      const a = new WebglAddon();
      a.onContextLoss(() => {
        if (addon !== a) return;
        unload();
        onState({ active: "compatible", fellBack: true });
      });
      term.loadAddon(a);
      addon = a;
      onState({ active: "gpu", fellBack: false });
    } catch {
      unload();
      onState({ active: "compatible", fellBack: true });
    }
  };

  set(initial);
  return { set, dispose: unload };
}
