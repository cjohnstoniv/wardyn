/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Every PUT, POST and PATCH sent through wfetch that carries something a person composed names the
// person it was composed under (`asPerson: true`, or `person ?? true` when a save of several requests passes the
// person it began under), so a cookie another tab switched to someone else
// refuses the save instead of landing it on the wrong account. wfetch strips the option before sending,
// so the call sites are the only place to pin it: a new write without the flag fails here unless it is
// on the list below with its reason. That is every module of lib/api, and every other module of the
// app that calls wfetch itself (a screen's own api file). DELETE composes nothing and is not checked.
import { readdirSync, readFileSync } from "node:fs";
import { join, relative } from "node:path";
import { expect, it } from "vitest";

// A substring of the call that opens the write -> why that write composes nothing to name a person for.
const NOT_A_DRAFT: Record<string, string> = {
  '"/auth/logout"': "ends the session; nothing composed",
  '"/me/view"': "per-session view switch, not a saved draft",
  '"/sessions/revoke"': "one-click revoke of a named person's sessions",
  '"/site-config/test-proxy"': "probe, saves nothing",
  '"/site-config/test-redirect"': "probe, saves nothing",
  '"/setup/onboarding-complete"': "one-click marker, no body",
  "/preview": "preview of a draft, writes nothing",
  '"/governance/check"': "dry run of a claim set, writes nothing",
  '"/runs/preflight"': "preview of a launch, writes nothing",
  '"/templates/import"': "reads one document and says what is wrong with it, stores nothing",
  '"/runs/policy-preview"': "preview of a policy, writes nothing",
  '"/policies/grade"': "read-only grading by POST",
  "/profile/synthesize": "action on a run, no composed body",
  "/kill": "action on a run, no composed body",
  "/revive": "action on a run, no composed body",
  "/resume": "action on a run, no composed body",
  "/attach/ticket": "mints a single-use ticket, no composed body",
  "/ticket`, { method": "mints a single-use ticket for one inbound app, no composed body",
  "/desktop/watch-ticket`, { method": "mints a single-use ticket for watching a background run's desktop, no composed body",
  "/attach/takeover": "action on a run, no composed body",
  "/desktop/window`, { method": "action on a run (opens its window on the caller's Mac), no composed body",
  "}/restart": "action on a workload, no composed body",
  "/org-check": "probe, saves nothing",
  "/sign-in`, { method": "starts a sign-in flow, no body",
  "/build`, {": "action on a workspace (Rebuild sends only refresh: true), no composed body",
  "/env-as-code/write": "action on a workspace, no composed body",
  "/scan`": "action on a workspace, no composed body",
};

const WRITE = /method:\s*"(POST|PUT|PATCH)"/g;

// The object literal that holds `method`, from its opening brace to its closing one.
function literalAround(src: string, at: number): string {
  let start = at;
  for (let depth = 0; start > 0; start--) {
    if (src[start] === "}") depth++;
    if (src[start] === "{" && depth-- === 0) break;
  }
  let end = at;
  for (let depth = 0; end < src.length; end++) {
    if (src[end] === "{") depth++;
    if (src[end] === "}" && depth-- === 0) break;
  }
  return src.slice(start, end + 1);
}

const appDir = join(__dirname, "..", "..");
const isSource = (f: string) => /\.tsx?$/.test(f) && !/\.test\.tsx?$/.test(f);
const libFiles = readdirSync(__dirname).filter(isSource).map((f) => join(__dirname, f));
const screenFiles = (readdirSync(appDir, { recursive: true }) as string[])
  .map((f) => join(appDir, f))
  .filter((f) => isSource(f) && !f.startsWith(__dirname) && readFileSync(f, "utf8").includes("wfetch("));
const files = [...libFiles, ...screenFiles].map((f) => relative(appDir, f));
const used = new Set<string>();

it("reaches the screens' own api files", () => {
  expect(files).toEqual(
    expect.arrayContaining([
      "components/screens/templates/templates-api.ts",
      "components/screens/new-run-flow/review/save-as-dialog.api.ts",
      "components/screens/new-run-flow/document/import-door.ts",
    ]),
  );
});

it.each(files)("%s names the person on every composed write", (file) => {
  const src = readFileSync(join(appDir, file), "utf8");
  const bare: string[] = [];
  for (const m of src.matchAll(WRITE)) {
    if (/asPerson: (person \?\? )?true\b/.test(literalAround(src, m.index!))) continue;
    const call = src.slice(Math.max(0, src.lastIndexOf("wfetch(", m.index!)), m.index! + 20);
    const reason = Object.keys(NOT_A_DRAFT).find((k) => call.includes(k));
    if (reason) used.add(reason);
    else bare.push(src.slice(src.lastIndexOf("\n", m.index!) + 1, src.indexOf("\n", m.index!)).trim());
  }
  expect(bare, `${file}: writes without asPerson: true (add it, or list the write with a reason)`).toEqual([]);
});

it("keeps no list entry for a write that is gone", () => {
  expect(Object.keys(NOT_A_DRAFT).filter((k) => !used.has(k))).toEqual([]);
});

// A secret save names the person wfetch reads when setSecret is called, so the call comes before the handler's
// first wait: after one, the person read is whoever this tab has confirmed by then, and the server would store
// the value for them. A handler that must wait first reads getExpectedPrincipal() at the click instead.
it("every secret save calls setSecret before its handler waits on anything", () => {
  const sources = (readdirSync(appDir, { recursive: true }) as string[]).filter(isSource);
  const saves: string[] = [];
  const late: string[] = [];
  for (const file of sources) {
    const src = readFileSync(join(appDir, file), "utf8");
    for (const m of src.matchAll(/\b(?:secrets|secretsApi)\.setSecret\(/g)) {
      saves.push(file);
      const head = src.slice(Math.max(0, src.lastIndexOf("async ", m.index!)), m.index!);
      // The `await` this call sits in (on its own line) is not a wait before it.
      const own = head.lastIndexOf("await");
      const before = own >= 0 && !head.slice(own).includes("\n") ? head.slice(0, own) : head;
      // A save that names the person read at the click (`person`, from getExpectedPrincipal) may wait first.
      const args = src.slice(m.index!, src.indexOf(";", m.index!));
      if (/\bperson\b/.test(args) && head.includes("getExpectedPrincipal()")) continue;
      if (/\bawait\b/.test(before) || head.includes(".then(")) late.push(file);
    }
  }
  expect(new Set(saves).size).toBeGreaterThanOrEqual(4);
  expect(late, "setSecret called after a wait: the save would name whoever is confirmed by then").toEqual([]);
});
