// Overnight staging helper: signs in as the SSO admin and resets the
// permissions state the 04c/12b takes need fresh (enforcement OFF, no grants).
// Usage: node stage-reset.mjs [--keep-enforced]
import { chromium } from "@playwright/test";

const BASE = process.env.WARDYN_DEMO_BASE_URL || "http://localhost:8280";
const keepEnforced = process.argv.includes("--keep-enforced");

const browser = await chromium.launch();
const page = await browser.newPage();
await page.goto(BASE + "/");
const sso = page.getByRole("link", { name: "Sign in with SSO" }).or(page.getByRole("button", { name: "Sign in with SSO" })).first();
await sso.click();
await page.locator('input[type="password"]').waitFor({ timeout: 30000 });
await page.locator('input[type="text"], input[name="login"]').first().fill("admin@wardyn.local");
await page.locator('input[type="password"]').fill("password");
await page.getByRole("button", { name: /log ?in/i }).click();
await page.waitForURL(/8280/, { timeout: 30000 });
await page.waitForTimeout(1000);

const out = await page.evaluate(async ({ keepEnforced }) => {
  const perms = await (await fetch("/api/v1/permissions")).json();
  const result = { before: perms, deleted: [], enforcement: null, killed: [] };
  // Reap every non-terminal run: iteration rounds otherwise pile up live
  // pods until the daemon's k8s client rate-limits and sandbox creates fail.
  const runsResp = await (await fetch("/api/v1/runs?limit=1000")).json();
  const runs = Array.isArray(runsResp) ? runsResp : (runsResp.items || []);
  const TERMINAL = new Set(["COMPLETED", "FAILED", "KILLED", "STOPPED", "ARCHIVED"]);
  for (const r of runs) {
    if (!TERMINAL.has(r.state)) {
      const k = await fetch(`/api/v1/runs/${r.id}/kill`, { method: "POST" });
      result.killed.push({ id: r.id.slice(0, 8), status: k.status });
    }
  }
  for (const g of perms.grants || []) {
    const r = await fetch(`/api/v1/permissions/grants/${g.id}`, { method: "DELETE" });
    result.deleted.push({ id: g.id, status: r.status });
  }
  if (!keepEnforced) {
    const enf = { ...(perms.enforcement || {}) };
    for (const k of Object.keys(enf)) enf[k] = false;
    const r = await fetch("/api/v1/permissions/enforcement", {
      method: "PUT",
      headers: { "content-type": "application/json", ...(perms.enforcement_etag ? { "If-Match": perms.enforcement_etag } : {}) },
      body: JSON.stringify(enf),
    });
    result.enforcement = { status: r.status, body: (await r.text()).slice(0, 200) };
  }
  return result;
}, { keepEnforced });
console.log(JSON.stringify(out, null, 2).slice(0, 1500));
await browser.close();
