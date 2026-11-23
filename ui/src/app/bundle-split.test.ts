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
import { readFileSync, readdirSync } from "node:fs";
import path from "node:path";
// Rollup's types come via vite's own re-export (`export { rollup as Rollup }`) —
// rollup is a transitive dep, not a direct one, so importing "rollup" here would
// not typecheck.
import { build, type Rollup } from "vite";
import { describe, expect, it } from "vitest";

const uiRoot = path.resolve(__dirname, "../..");

// The entry chunk sat at 1,331 kB (a single un-split chunk) before the split
// and lands ~640 kB with React 19 (its react-dom is ~65 kB larger than 18's);
// 660 kB kept about 20 kB of headroom; ordinary growth used it up by 0.9.4
// (661 kB), so 672 kB restores ~11 kB while any lazy route leaking into the
// eager graph still blows straight through it (the terminal-stack assertion
// below is the primary split guard).
//
// Baseline from one `pnpm build` of this tree: the entry chunk (main-*.js) is 76,267 B (74 kB), because the
// shared libraries sit in their own chunks. The whole eager graph (the entry plus every chunk index.html
// preloads) is 754,414 B in 106 chunks. This test measures the entry chunk alone, so the 672 kB budget above
// leaves about 598 kB of slack; the next budget change should start from 76,267 B.
const ENTRY_BUDGET_BYTES = 672 * 1024;

async function build_(mode: "production" | "e2e" = "production"): Promise<(Rollup.OutputChunk | Rollup.OutputAsset)[]> {
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
      mode,
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
// The e2e build (scripts/e2e-backend.sh builds it with --mode e2e) is the one the end-to-end checks load.
let builtE2E: ReturnType<typeof build_> | undefined;
const buildE2EOnce = () => (builtE2E ??= build_("e2e"));
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

// New Run (0.9.0): its copy modules and flow directory are lazy-only, and the barrier art ships as hashed
// files. The entry's static imports load eagerly, so this walks them rather than trusting `isEntry` alone.
const NEW_RUN_MODULE = /\/(?:wardyn\/copy\/new-run|new-run-flow)\//;
const NEW_RUN_IMPORT = /(?:from|import\()\s*["'][^"']*(?:copy\/new-run|new-run-flow)\/[^"']*["']/;
const BARRIER_ART_IMPORT = /from\s+["'][^"']*barrier-(?:art|compare-table)["']/;

// True when a non-test source file outside `skip` imports a matching module: from then on the build must
// contain it, so "absent from the eager chunks" cannot pass by the module being dropped altogether.
function appImports(pattern: RegExp, skip: RegExp): boolean {
  const walk = (dir: string): boolean =>
    readdirSync(dir, { withFileTypes: true }).some((e) => {
      const file = path.join(dir, e.name);
      if (e.isDirectory()) return walk(file);
      if (!/\.tsx?$/.test(e.name) || /\.test\.tsx?$/.test(e.name) || skip.test(file)) return false;
      return pattern.test(readFileSync(file, "utf8"));
    });
  return walk(path.join(uiRoot, "src"));
}

const modulesOf = (chunks: Rollup.OutputChunk[]) => chunks.flatMap((c) => Object.keys(c.modules));

describe("New Run copy and flow stay out of the eager chunks", () => {
  it("keeps copy/new-run/ and new-run-flow/ in lazy chunks only", async () => {
    const chunks = chunksOf(await buildOnce());
    const eager = new Set(chunks.filter((c) => c.isEntry).map((c) => c.fileName));
    for (const name of eager) for (const imported of chunks.find((c) => c.fileName === name)?.imports ?? []) eager.add(imported);
    const inEager = modulesOf(chunks.filter((c) => eager.has(c.fileName))).filter((m) => NEW_RUN_MODULE.test(m));
    expect(inEager, `New Run modules leaked into an eager chunk: ${inEager.join(", ")}`).toEqual([]);

    if (appImports(NEW_RUN_IMPORT, NEW_RUN_MODULE)) {
      const inLazy = modulesOf(chunks.filter((c) => !eager.has(c.fileName))).filter((m) => NEW_RUN_MODULE.test(m));
      expect(inLazy.length, "the app imports New Run modules but no lazy chunk carries them").toBeGreaterThan(0);
    }
  }, 180_000);
});

// Vite inlines an asset under assetsInlineLimit (4096 B) as a data: URI, and the CSP's default-src 'self'
// blocks data: images. Only the 128 and 256 px art may ship; smaller files use the SVG TierIllustration.
describe("barrier art ships as files, never inlined", () => {
  it("emits no data:image/png in any chunk or asset of the app build", async () => {
    const inlined = (await buildOnce())
      .filter((o) => (o.type === "chunk" ? o.code : String(o.source)).includes("data:image/png"))
      .map((o) => o.fileName);
    expect(inlined, "PNG inlined as a data: URI").toEqual([]);
  }, 180_000);

  it("emits all six barrier PNGs as asset files once a screen imports the art", async () => {
    const build = await buildOnce();
    const emitted = build.filter((o) => o.type === "asset" && /barrier-(fence|wall|vault)-(128|256)-.*\.png$/.test(o.fileName));
    // Wired once the art is in the built graph: an importer the app itself never reaches (New Run steps
    // before the route cutover) leaves it out of the build.
    const wired = appImports(BARRIER_ART_IMPORT, /barrier-(art|compare-table)\.tsx$/) && modulesOf(chunksOf(build)).some((m) => /barrier-art\.tsx$/.test(m));
    expect(emitted.length).toBe(wired ? 6 : 0);
  }, 180_000);

  it("builds the art component alone with six PNG files and no data: URI", async () => {
    const result = (await build({
      configFile: path.join(uiRoot, "vite.config.ts"),
      root: uiRoot,
      logLevel: "silent",
      mode: "production",
      build: {
        write: false,
        // Not `lib`: library mode inlines every asset, which would hide the very thing this checks.
        rollupOptions: { input: path.join(uiRoot, "src/app/components/wardyn/barrier-art.tsx") },
      },
    })) as Rollup.RollupOutput | Rollup.RollupOutput[];
    const out = (Array.isArray(result) ? result[0] : result).output;
    const pngs = out.filter((o) => o.type === "asset" && o.fileName.endsWith(".png"));
    expect(pngs.map((o) => /barrier-(?:fence|wall|vault)-(?:128|256)/.exec(o.fileName)?.[0]).sort()).toEqual([
      "barrier-fence-128",
      "barrier-fence-256",
      "barrier-vault-128",
      "barrier-vault-256",
      "barrier-wall-128",
      "barrier-wall-256",
    ]);
    const inlined = out.filter((o) => (o.type === "chunk" ? o.code : String(o.source)).includes("data:image/png"));
    expect(inlined.map((o) => o.fileName)).toEqual([]);
  }, 180_000);
});

// The walls view's 3D stack: three (with its addons), React Three Fiber and camera-controls. Anchored on the package
// directory, so a sibling such as three-mesh-bvh never matches, and checked one package at a time, so a build that
// dropped one cannot pass on another's match.
const STACK_3D: Record<string, RegExp> = {
  three: /\/node_modules\/three\//,
  "@react-three/fiber": /\/node_modules\/@react-three\/fiber\//,
  "camera-controls": /\/node_modules\/camera-controls\//,
};
const ANY_3D = /\/node_modules\/(?:three|@react-three\/fiber|camera-controls)\//;
// A source import of the stack. The e2e probe and its seam are skipped: production must drop them.
const STACK_3D_IMPORT = /(?:from|import\()\s*["'](?:three|three\/addons\/[^"']+|@react-three\/fiber|camera-controls)["']/;
const PROBE_FILES = /\/screens\/walls\/canvas\/webgl-probe(?:-e2e-seam)?\.ts$/;
// What a 3D chunk must never carry: string evaluation, a worker, a blob or image data: URL (the CSP allows neither),
// the GPU's identity, or a CDN host.
const STACK_3D_FORBIDDEN =
  /\bnew Function\s*\(|\beval\s*\(|\bnew (?:Shared)?Worker\s*\(|createObjectURL|data:image\/|WEBGL_debug_renderer_info|cdn\.jsdelivr\.net|unpkg\.com|fonts\.googleapis\.com|fonts\.gstatic\.com|raw\.githubusercontent\.com/;
// EST, raw bytes of every chunk that carries the stack. The first green run's figure goes in the lane report.
const STACK_3D_BUDGET_BYTES = 1600 * 1024;

// Every entry chunk plus everything it statically imports: what a page loads before any lazy import runs.
function eagerOf(chunks: Rollup.OutputChunk[]): Set<string> {
  const eager = new Set(chunks.filter((c) => c.isEntry).map((c) => c.fileName));
  for (const name of eager) for (const imported of chunks.find((c) => c.fileName === name)?.imports ?? []) eager.add(imported);
  return eager;
}

describe("the walls 3D stack loads only in lazy chunks", () => {
  it("ships three, React Three Fiber and camera-controls lazily in the e2e build, clean and under budget", async () => {
    const chunks = chunksOf(await buildE2EOnce());
    const eager = eagerOf(chunks);
    const inEager = modulesOf(chunks.filter((c) => eager.has(c.fileName))).filter((m) => ANY_3D.test(m));
    expect(inEager, `3D stack in an eager chunk: ${inEager.join(", ")}`).toEqual([]);

    const lazy = chunks.filter((c) => !eager.has(c.fileName));
    for (const [name, pattern] of Object.entries(STACK_3D)) {
      expect(modulesOf(lazy).filter((m) => pattern.test(m)).length, `${name} is in no lazy chunk`).toBeGreaterThan(0);
    }

    const stackChunks = lazy.filter((c) => Object.keys(c.modules).some((m) => ANY_3D.test(m)));
    const hits = stackChunks.flatMap((c) => {
      const hit = STACK_3D_FORBIDDEN.exec(c.code);
      return hit ? [`${c.fileName}: ${c.code.slice(Math.max(0, hit.index - 60), hit.index + 60)}`] : [];
    });
    expect(hits, "a 3D chunk carries eval, a worker, a blob or data: URL, the GPU identity or a CDN host").toEqual([]);

    const bytes = stackChunks.reduce((n, c) => n + Buffer.byteLength(c.code, "utf8"), 0);
    expect(bytes, `the 3D chunks are ${bytes} B`).toBeLessThan(STACK_3D_BUDGET_BYTES);
  }, 180_000);

  it("drops the e2e probe from the production build", async () => {
    const out = await buildOnce();
    const seam = out
      .filter((o) => (o.type === "chunk" ? o.code : String(o.source)).includes("__wardynWallsProbe"))
      .map((o) => o.fileName);
    expect(seam, "the e2e-only probe seam ships in the production build").toEqual([]);

    const chunks = chunksOf(out);
    const eager = eagerOf(chunks);
    const inEager = modulesOf(chunks.filter((c) => eager.has(c.fileName))).filter((m) => ANY_3D.test(m));
    expect(inEager, `3D stack in an eager chunk: ${inEager.join(", ")}`).toEqual([]);
    const inLazy = modulesOf(chunks.filter((c) => !eager.has(c.fileName))).filter((m) => ANY_3D.test(m));
    if (appImports(STACK_3D_IMPORT, PROBE_FILES)) {
      expect(inLazy.length, "the app imports the 3D stack but no lazy chunk carries it").toBeGreaterThan(0);
    } else {
      expect(inLazy, "only the e2e probe imports the 3D stack, so production carries none of it").toEqual([]);
    }
  }, 180_000);
});
