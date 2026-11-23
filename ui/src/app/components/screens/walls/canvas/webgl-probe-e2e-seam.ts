/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/**
 * Test-only WebGL probe seam. Under the e2e build mode only, it puts one function on window that loads the walls view's
 * 3D stack in its own chunk and draws a frame off screen (webgl-probe.ts). The production build folds the mode check to
 * a constant and drops the import with it, which bundle-split.test.ts proves on the built output.
 */
export function exposeWebGLProbeForE2E(): void {
  if (import.meta.env.MODE !== "e2e") return;
  (window as unknown as { __wardynWallsProbe?: () => Promise<unknown> }).__wardynWallsProbe = () =>
    import("./webgl-probe").then((m) => m.runWebGLProbe());
}
