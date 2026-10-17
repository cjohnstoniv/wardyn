// Every PUT, POST and PATCH in lib/api that carries something a person composed names the person it
// was composed under (`asPerson: true`), so a cookie another tab switched to someone else refuses the
// save instead of landing it on the wrong account. wfetch strips the option before sending, so the call
// sites are the only place to pin it: a new write without the flag fails here unless it is on
// the list below with its reason. DELETE composes nothing and is not checked.
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
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
  '"/runs/preflight"': "preview of a launch, writes nothing",
  '"/runs/policy-preview"': "preview of a policy, writes nothing",
  '"/policies/grade"': "read-only grading by POST",
  "/profile/synthesize": "action on a run, no composed body",
  "/kill": "action on a run, no composed body",
  "/revive": "action on a run, no composed body",
  "/resume": "action on a run, no composed body",
  "/attach/ticket": "mints a single-use ticket, no composed body",
  "/attach/takeover": "action on a run, no composed body",
  '"/admin/runs/restart"': "action on named runs, no composed text",
  "/org-check": "probe, saves nothing",
  "/sign-in`, { method": "starts a sign-in flow, no body",
  "/governance/changes/${encodeURIComponent(id)}/approve": "one-click approval of a held change, no body",
  "/build`, { method": "action on a workspace, no composed body",
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

const files = readdirSync(__dirname).filter((f) => f.endsWith(".ts") && !f.endsWith(".test.ts"));
const used = new Set<string>();

it.each(files)("%s names the person on every composed write", (file) => {
  const src = readFileSync(join(__dirname, file), "utf8");
  const bare: string[] = [];
  for (const m of src.matchAll(WRITE)) {
    if (literalAround(src, m.index!).includes("asPerson: true")) continue;
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
