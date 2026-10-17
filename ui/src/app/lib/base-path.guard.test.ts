/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// @vitest-environment node

// Guards WARDYN_BASE_PATH: under a sub-path, a root-absolute server URL
// ("/api/v1/…", "/auth/login", "/healthz") or a hand-built ws:// URL skips the
// base and lands on whatever else the reverse proxy serves at the host root.
// Every such URL is built by base-path.ts; this refuses a new literal anywhere
// else in src/. Test files are exempt — they assert on the URLs the helper
// builds, and never ship.
import { readdirSync, readFileSync } from "node:fs";
import { relative, resolve } from "node:path";
import { describe, expect, it } from "vitest";

const SRC = resolve(process.cwd(), "src");
const HELPER = "app/lib/base-path.ts";

const ROOT_ABSOLUTE: RegExp[] = [
  /["'`]\/(api|auth)\//, // the API and the sign-in routes
  /["'`]\/(healthz|readyz|metrics)\b/, // the probes
  /["'`]\/(__wardyn|assets)\//, // the enter route, the bundle's own assets
  /["'`]wss?:["'`]/, // a WebSocket URL assembled by hand
  /location\.origin\s*\}\//, // an origin glued to a root path (`${location.origin}/runs/…`): the base path is skipped
  /location\.origin\s*\+\s*["'`]\//, // the same, by concatenation
  /\bhref=["'`]\/(?!\/)/, // a raw anchor to a root-absolute path (a <Link> gets the router's basename)
];

function walk(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
    const p = resolve(dir, e.name);
    if (e.isDirectory()) return e.name === "test" ? [] : walk(p);
    return /\.tsx?$/.test(e.name) && !/\.test\.tsx?$/.test(e.name) ? [p] : [];
  });
}

// A literal handed straight to a builder is what the guard asks for, and a
// wfetch path is already relative to the API base (core.ts).
const BUILT = /\b(?:appURL|apiURL|wsURL|wfetch)\(\s*["'`]/g;

/** Lines that are code, not a comment (comments cite routes in prose), with built literals taken out. */
function codeLines(text: string): [number, string][] {
  return text
    .split("\n")
    .map((l, i): [number, string] => [i + 1, l.replace(BUILT, "(")])
    .filter(([, l]) => !/^\s*(\/\/|\/\*|\*)/.test(l));
}

describe("base-path guard", () => {
  it("builds every root-absolute client URL in base-path.ts", () => {
    const hits: string[] = [];
    for (const file of walk(SRC)) {
      const rel = relative(SRC, file);
      if (rel === HELPER) continue;
      for (const [n, line] of codeLines(readFileSync(file, "utf8"))) {
        if (ROOT_ABSOLUTE.some((re) => re.test(line))) hits.push(`${rel}:${n}: ${line.trim()}`);
      }
    }
    expect(hits, "build these through appURL/apiURL/wsURL (src/app/lib/base-path.ts)").toEqual([]);
  });

  it("scans the files that used to carry the literals", () => {
    const files = walk(SRC).map((f) => relative(SRC, f));
    for (const f of ["app/lib/api/core.ts", "app/lib/api/health.ts", "app/components/attach-terminal.tsx", "app/components/screens/sign-in.tsx"]) {
      expect(files).toContain(f);
    }
  });
});
