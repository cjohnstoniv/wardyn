/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, gotoConsole, navTo } from "./fixtures";
import type { Page, Locator } from "@playwright/test";

// Corporate network — Getting Started step 2 of 10 (steps.ts PHASES; see
// getting-started.spec.ts for the 10-step rail this sits inside), right
// before Integrations. Two sub-tabs: Host proxy (evidence read from the
// host's env vars, over the config sandboxes actually use, over a real Test
// probe) and Egress redirection (From -> To rows; an ecosystem-recognized
// source also gets a per-tool config file, anything else is "network only").
//
// corp-network-step.test.tsx already covers every render permutation against
// a mocked API; this spec proves the real wiring instead:
//   - GET /api/v1/setup/status carries the host's proxy-detection evidence
//     (HostProxyDetection). scripts/e2e-backend.sh never sets
//     WARDYN_HOST_PROXY_B64, so the seeded backend always reports "nothing
//     detected" — "detection found something" is spliced onto the REAL
//     response (route.fetch() + patch host_proxy + refulfill) instead of
//     faking the whole payload, so every other field (barrier readiness,
//     runs, …) the rest of the funnel reads stays genuine.
//   - POST .../test-proxy and .../test-redirect each launch a real throwaway
//     sandbox server-side; the seeded backend runs `-runner none`, so it can
//     never deterministically produce "reached" vs "blocked". Those two POSTs
//     are stubbed outright, the same reason workspace-wizard.spec.ts stubs a
//     workspace scan rather than racing the real thing.
//   - Everything else (typing, saving a plain URL, saving a credential as a
//     secret, adding an egress redirect) hits the REAL seeded backend, same
//     as every other spec in this suite.
//
// Shared-backend note: e2e-backend.sh resets the schema once per SPEC FILE,
// not per test. Tests below run serially (run-ui-e2e.sh's --workers=1) and
// are ordered so a save one test makes (e.g. "Use this" persisting a plain
// proxy URL) never invalidates a precondition a LATER test needs — see the
// comment on the "Use this" test.
//
// The "From" combobox (FromCombobox in corp-network-step.tsx) has no
// CommandInput — it is select-only from EGRESS_SUGGEST, there is no way to
// type free text into it today despite the empty-state copy saying "or type
// anything". A "network only" row is therefore reached the same way the
// component's own unit tests reach it: pick a "container images" suggestion
// (ecosystem is nulled for that group in AddRedirectForm.add()), not by
// typing an arbitrary host.

function main(page: Page): Locator {
  return page.getByRole("main");
}

// Fresh tour every test (matches getting-started.spec.ts): walks Environment
// -> Corporate network (one Next click) and returns the step body's `main`
// landmark, Host proxy tab active (the default).
async function openCorpNetworkStep(page: Page): Promise<Locator> {
  await gotoConsole(page);
  await navTo(page, "Getting started");
  await page.getByRole("button", { name: /get started|finish setup/i }).click();
  const m = main(page);
  await expect(m.getByRole("heading", { name: /pick your barrier/i })).toBeVisible();
  await page.getByRole("button", { name: /^Next:/i }).click();
  await expect(m.getByRole("heading", { name: /corporate network/i })).toBeVisible();
  return m;
}

test.describe("Corporate network step", () => {
  test("reaches Corporate network between Environment and Integrations, and both tabs switch", async ({ page }) => {
    const m = await openCorpNetworkStep(page);

    // Host proxy is the default tab.
    await expect(m.getByText("What Wardyn found on this host")).toBeVisible();
    await expect(m.getByRole("button", { name: /^test proxy$/i })).toBeVisible();

    await m.getByRole("tab", { name: "Egress redirection" }).click();
    await expect(m.getByText(/point outbound traffic at an internal mirror/i)).toBeVisible();
    await expect(m.getByRole("button", { name: /\+ add redirect/i })).toBeVisible();

    await m.getByRole("tab", { name: "Host proxy" }).click();
    await expect(m.getByText("What Wardyn found on this host")).toBeVisible();

    // Confirms the order from steps.ts's PHASES: corp_network sits directly
    // before integrations.
    await page.getByRole("button", { name: /^Next:/i }).click();
    await expect(m.getByRole("heading", { name: /connect what's outside wardyn/i })).toBeVisible();
  });

  // Must run before any test that saves a plain upstream_proxy_url/secret_ref
  // — "Use this" only renders while the proxy is unconfigured, and this
  // file's backend is shared across every test here (fresh once per FILE,
  // not per test; see the header comment).
  test("detected host-proxy evidence renders, and its 'Use this' button populates the Proxy URL field", async ({ page }) => {
    await page.route("**/api/v1/setup/status", async (route) => {
      const response = await route.fetch();
      const json = await response.json();
      json.host_proxy = {
        has_credentials: false,
        http_proxy: { value: "http://proxy.corp.acme.com:8080", source: "env", has_credentials: false },
      };
      await route.fulfill({ response, json });
    });

    const m = await openCorpNetworkStep(page);

    await expect(m.getByText("HTTP_PROXY")).toBeVisible();
    await expect(m.getByText("http://proxy.corp.acme.com:8080").first()).toBeVisible();

    await m.getByRole("button", { name: "Use this" }).click();
    await expect(m.getByLabel(/proxy url/i)).toHaveValue("http://proxy.corp.acme.com:8080");
  });

  test("typing a proxy URL with user:pass@ flips the field to the masked, stored-secret treatment", async ({ page }) => {
    const m = await openCorpNetworkStep(page);
    const field = m.getByLabel(/proxy url/i);
    await expect(field).toHaveAttribute("type", "text");

    await field.fill("http://ops-egress:secret123@proxy.corp.acme.com:8080");
    await expect(field).toHaveAttribute("type", "password");
    await expect(m.getByText(/username and password/i)).toBeVisible();
    await expect(m.getByText(/write-only/i)).toBeVisible();

    // The unit suite mocks secretsApi.setSecret entirely; only an e2e proves
    // the real round trip — stored as a secret, never as a plain URL.
    await m.getByRole("button", { name: /^save$/i }).click();
    await expect(m.getByText("upstream-proxy-url")).toBeVisible();
    await expect(m.getByText(/chaining through the url in secret/i)).toBeVisible();
  });

  test("Test proxy fires a real request and renders reached vs. blocked distinctly", async ({ page }) => {
    let state: "reached" | "blocked" = "reached";
    await page.route("**/api/v1/site-config/test-proxy", (route) =>
      route.fulfill({
        json: {
          state,
          detail: state === "reached" ? "42ms round trip through the corporate proxy." : "Connection refused after 3 attempts.",
        },
      }),
    );

    const m = await openCorpNetworkStep(page);
    const testBtn = m.getByRole("button", { name: /^test proxy$/i });

    await testBtn.click();
    await expect(m.getByText("Reached", { exact: true })).toBeVisible();
    await expect(m.getByText(/42ms round trip/)).toBeVisible();

    state = "blocked";
    await testBtn.click();
    await expect(m.getByText("Blocked", { exact: true })).toBeVisible();
    await expect(m.getByText(/connection refused/i)).toBeVisible();
    // Proves the chip actually re-rendered rather than just appending.
    await expect(m.getByText("Reached", { exact: true })).toHaveCount(0);
  });

  test("egress redirection: a suggested source is ecosystem-tagged, a container-registry one is network-only, and a row's Test disables + relabels while running", async ({ page }) => {
    let resolveTest!: (v: { state: string; detail: string }) => void;
    await page.route("**/api/v1/site-config/test-redirect", async (route) => {
      const result = await new Promise<{ state: string; detail: string }>((r) => {
        resolveTest = r;
      });
      await route.fulfill({ json: result });
    });

    const m = await openCorpNetworkStep(page);
    await m.getByRole("tab", { name: "Egress redirection" }).click();
    await expect(m.getByText("None. Outbound traffic goes to the public endpoints.")).toBeVisible();

    // A recognized ecosystem source (npm) — gets a per-tool config file too,
    // so it must NOT carry the "network only" chip.
    await m.getByRole("combobox").click();
    await expect(page.getByText("https://registry.npmjs.org", { exact: true })).toBeVisible();
    await page.getByText("https://registry.npmjs.org", { exact: true }).click();
    await m.getByPlaceholder(/artifactory\.corp\.internal/i).fill("https://artifactory.corp.internal/api/npm/npm-remote");
    await m.getByRole("button", { name: /\+ add redirect/i }).click();

    const npmRow = m.getByTitle(/^https:\/\/registry\.npmjs\.org →/);
    await expect(npmRow).toBeVisible();
    await expect(npmRow.getByText("network only")).toHaveCount(0);

    // A container-registry suggestion (ghcr.io) — no .npmrc-style config file
    // exists for it, so it's network-only despite also coming from the list.
    await m.getByRole("combobox").click();
    await expect(page.getByText("https://ghcr.io", { exact: true })).toBeVisible();
    await page.getByText("https://ghcr.io", { exact: true }).click();
    await m.getByPlaceholder(/artifactory\.corp\.internal/i).fill("https://registry.corp.internal/ghcr-remote");
    await m.getByRole("button", { name: /\+ add redirect/i }).click();

    const ghcrRow = m.getByTitle(/^https:\/\/ghcr\.io →/);
    await expect(ghcrRow).toBeVisible();
    await expect(ghcrRow.getByText("network only")).toBeVisible();

    // Per-row Test: disables + relabels to "Testing…" while running, leaves
    // the sibling row untouched, then renders its own verdict chip.
    await npmRow.getByRole("button", { name: /^test$/i }).click();
    const runningBtn = npmRow.getByRole("button", { name: /^testing…$/i });
    await expect(runningBtn).toBeDisabled();
    await expect(ghcrRow.getByRole("button", { name: /^test$/i })).toBeEnabled();

    resolveTest({ state: "reached", detail: "Reached artifactory.corp.internal through the mirror." });
    await expect(npmRow.getByText("Reached", { exact: true })).toBeVisible();
    await expect(npmRow.getByRole("button", { name: /^test$/i })).toBeEnabled();
  });
});
