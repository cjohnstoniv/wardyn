/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// @vitest-environment node

// Guards the route-level code-splitting in App.tsx.
//
// The split is one-import fragile: any module reachable from the EAGER entry
// graph (App -> AppShell -> RunsScreen) that statically imports a lazy screen
// silently collapses the whole thing back into one chunk, with no test failure
// and no visible bug — just a 3x slower first load. That has already happened
// twice: App.tsx imported the setup funnel's helpers straight from
// setup-screen.tsx (which reaches xterm via harness-login-pane), and runs.tsx
// imported the run wizard eagerly.
//
// So this asserts the OUTPUT, not the source: the real production build must
// still be split, and the terminal stack must not be in the entry chunk.
import path from "node:path";
// Rollup's types come via vite's own re-export (`export { rollup as Rollup }`) —
// rollup is a transitive dep, not a direct one, so importing "rollup" here would
// not typecheck.
import { build, type Rollup } from "vite";
import { describe, expect, it } from "vitest";

const uiRoot = path.resolve(__dirname, "../..");

// Vite's own default warning threshold. The entry chunk sat at 1,331 kB (a
// single un-split chunk) before the split and lands ~450 kB after; 500 kB keeps
// the budget honest without being so tight that ordinary feature work trips it.
const ENTRY_BUDGET_BYTES = 500 * 1024;

async function buildOnce(): Promise<Rollup.OutputChunk[]> {
  // Vite only defaults NODE_ENV to "production" for a build when it is UNSET,
  // and vitest has already set it to "test". Left alone, `process.env.NODE_ENV`
  // inlines as "test", React resolves its development bundle, and the entry
  // measures ~699kB — an artifact we never ship. Forcing it here makes this
  // build byte-identical to `pnpm build` (450,501 B at the time of writing).
  const prevNodeEnv = process.env.NODE_ENV;
  process.env.NODE_ENV = "production";
  try {
    const result = (await build({
      configFile: path.join(uiRoot, "vite.config.ts"),
      root: uiRoot,
      logLevel: "silent",
      mode: "production",
      // In-memory: never clobber the real dist/ a dev or the e2e lane is using.
      build: { write: false },
    })) as Rollup.RollupOutput | Rollup.RollupOutput[];
    const out = Array.isArray(result) ? result[0] : result;
    return out.output.filter((o): o is Rollup.OutputChunk => o.type === "chunk");
  } finally {
    process.env.NODE_ENV = prevNodeEnv;
  }
}

describe("UI bundle is route-code-split", () => {
  it("keeps the terminal stack out of the entry chunk and the entry under budget", async () => {
    const chunks = await buildOnce();
    const entry = chunks.find((c) => c.isEntry);
    expect(entry, "no entry chunk in build output").toBeDefined();

    // 1. The build is actually split (pre-fix this was a single chunk).
    expect(chunks.length).toBeGreaterThan(1);

    // 2. xterm and asciinema-player are the two heavy deps. They belong to the
    //    run-detail / recordings / demos / setup routes, never the entry.
    const entryModules = Object.keys(entry!.modules);
    const heavyInEntry = entryModules.filter((m) => /@xterm\/|asciinema-player/.test(m));
    expect(heavyInEntry, `heavy terminal deps leaked into the entry chunk: ${heavyInEntry.join(", ")}`).toEqual([]);

    // 3. ...and they are present SOMEWHERE, so a build that simply dropped them
    //    (or a regex that stopped matching) can't make this test vacuously pass.
    const heavyAnywhere = chunks
      .filter((c) => !c.isEntry)
      .flatMap((c) => Object.keys(c.modules))
      .filter((m) => /@xterm\/|asciinema-player/.test(m));
    expect(heavyAnywhere.length).toBeGreaterThan(0);

    // 4. Entry stays under Vite's own chunk-size warning threshold.
    const entryBytes = Buffer.byteLength(entry!.code, "utf8");
    expect(
      entryBytes,
      `entry chunk ${Math.round(entryBytes / 1024)}kB exceeds the ${ENTRY_BUDGET_BYTES / 1024}kB budget — ` +
        `something in the eager App -> AppShell -> RunsScreen graph is statically importing a lazy route`,
    ).toBeLessThan(ENTRY_BUDGET_BYTES);
  }, 180_000);
});
