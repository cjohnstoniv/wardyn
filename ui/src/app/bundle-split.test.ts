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

// The entry chunk sat at 1,331 kB (a single un-split chunk) before the split
// and lands ~508 kB after the 0.7 dependency bumps; 560 kB keeps roughly the
// original ~50 kB of headroom so ordinary feature work and routine dependency
// maintenance don't trip it, while any lazy route leaking into the eager graph
// still blows straight through it (the terminal-stack assertion below is the
// primary split guard).
const ENTRY_BUDGET_BYTES = 560 * 1024;

async function build_(): Promise<(Rollup.OutputChunk | Rollup.OutputAsset)[]> {
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
    return out.output;
  } finally {
    process.env.NODE_ENV = prevNodeEnv;
  }
}

// One build for every assertion below: it is the slow part.
let built: ReturnType<typeof build_> | undefined;
const buildOnce = () => (built ??= build_());
const chunksOf = (out: Awaited<ReturnType<typeof build_>>) =>
  out.filter((o): o is Rollup.OutputChunk => o.type === "chunk");

describe("UI bundle is route-code-split", () => {
  it("keeps the terminal stack out of the entry chunk and the entry under budget", async () => {
    const chunks = chunksOf(await buildOnce());
    const entry = chunks.find((c) => c.isEntry);
    expect(entry, "no entry chunk in build output").toBeDefined();

    // 1. The build is actually split (without it, this is a single chunk).
    expect(chunks.length).toBeGreaterThan(1);

    // 2. xterm and asciinema-player are the two heavy deps. They belong to the
    //    run-detail / recordings / demos / setup routes, never the entry.
    const entryModules = Object.keys(entry!.modules);
    const heavyInEntry = entryModules.filter((m) => /@xterm\/|asciinema-player/.test(m));
    expect(heavyInEntry, `heavy terminal deps leaked into the entry chunk: ${heavyInEntry.join(", ")}`).toEqual([]);

    // The policy source parser belongs to the editors (Policies, Governance,
    // New Run), all lazy routes. Static chunk imports load eagerly too;
    // extracting a shared chunk must not let a parser dependency bypass this
    // guard.
    const eagerChunks = new Set([entry!.fileName]);
    for (const fileName of eagerChunks) {
      for (const imported of chunks.find((chunk) => chunk.fileName === fileName)?.imports ?? []) eagerChunks.add(imported);
    }
    const yamlInEntry = chunks
      .filter((chunk) => eagerChunks.has(chunk.fileName))
      .flatMap((chunk) => Object.keys(chunk.modules))
      .filter((module) => /node_modules\/yaml\//.test(module));
    expect(yamlInEntry, `YAML parser leaked into the entry chunk: ${yamlInEntry.join(", ")}`).toEqual([]);
    // ...and it does ship, in a chunk the entry does not load: an editor that
    // stopped importing it (or a regex that stopped matching) cannot pass this.
    const yamlLazy = chunks
      .filter((chunk) => !eagerChunks.has(chunk.fileName))
      .flatMap((chunk) => Object.keys(chunk.modules))
      .filter((module) => /node_modules\/yaml\//.test(module));
    expect(yamlLazy.length, "the YAML parser is in no lazy chunk").toBeGreaterThan(0);
    // The policy document's own modules, its copy and the YAML display emitter ride lazy chunks too.
    const policyDocumentEager = chunks
      .filter((chunk) => eagerChunks.has(chunk.fileName))
      .flatMap((chunk) => Object.keys(chunk.modules))
      .filter((module) => /wardyn\/(policy-document\/|copy\/policy-document|segmented|policy-panel|yaml-block)/.test(module));
    expect(policyDocumentEager, `policy document modules leaked into the entry chunk: ${policyDocumentEager.join(", ")}`).toEqual([]);

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

// WARDYN_BASE_PATH: one bundle serves at the host root or under a sub-path,
// so no URL the build emits may be root-absolute. index.html's are relative
// ("./"), and the daemon rewrites exactly those to the base it serves under
// (internal/api's serveIndex); the chunks and stylesheets resolve theirs
// against their own URL. A "/assets/…" anywhere would skip the base.
describe("UI bundle carries no root-absolute asset URL", () => {
  it("emits every index.html asset URL relative, and none rooted in a chunk or stylesheet", async () => {
    const out = await buildOnce();
    const index = out.find((o): o is Rollup.OutputAsset => o.type === "asset" && o.fileName === "index.html");
    expect(index, "no index.html in build output").toBeDefined();
    const html = String(index!.source);
    const urls = [...html.matchAll(/\b(?:src|href)="([^"]*)"/g)].map((m) => m[1]);
    expect(urls.length).toBeGreaterThan(1);
    expect(urls.filter((u) => !u.startsWith("./")), "index.html asset URLs the daemon cannot rebase").toEqual([]);
    expect(html).toContain('data-wardyn-base=""');

    const rooted = out
      .filter((o) => o.fileName !== "index.html")
      .filter((o) => /["'(]\/assets\//.test(o.type === "chunk" ? o.code : String(o.source)))
      .map((o) => o.fileName);
    expect(rooted, "root-absolute /assets/ URLs in the bundle").toEqual([]);
  }, 180_000);
});
