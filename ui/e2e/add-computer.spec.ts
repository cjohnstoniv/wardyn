/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Add this computer" (mock M-ATT-4, #3322), one test per state of the States board, against the real console. The
// harness keeps runners off and has no runner, so the runner reads and /healthz are answered at the network boundary;
// every locator is scoped to the block (`[data-add-computer]`), never to the page's text, and nothing waits on a timer.
import type { Page } from "@playwright/test";
import { test, expect, asRealMember, gotoConsole, navTo, MEMBER_PRINCIPAL } from "./fixtures";
import { aheadByHours } from "../src/app/lib/test-clock";

const SH = "curl -fsSL https://github.com/cjohnstoniv/wardyn/releases/download/v0.10.0/install-cli.sh | sh";
const PS = "irm https://github.com/cjohnstoniv/wardyn/releases/download/v0.10.0/install-cli.ps1 | iex";
// Playwright's Desktop Chrome preset reports a Windows user agent, so each state names the viewer's system.
const LINUX = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36";
const WINDOWS = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36";
const NO_CLI_SIGN_IN =
  "This server does not offer CLI sign-in yet. Set WARDYN_TOKEN to a personal API token first; a personal token works for setup. A Wardyn admin can invite you to create one, and you claim it under Your account.";

/** /healthz as the daemon answers it, with the release version and CLI sign-in set for the state under test. One real read, then cached (a round trip per match races Playwright disposing the response). */
async function healthz(page: Page, over: Record<string, unknown>) {
  let real: Promise<Record<string, unknown>> | null = null;
  await page.route("**/healthz", async (route) => {
    real ??= route.fetch().then((res) => res.json());
    await route.fulfill({ json: { ...(await real), ...over } });
  });
}

const none = { runs: 0, cpu_millis: 0, memory_mib: 0, disk_mib: 0 };
const mbp = (createdAt: string) => ({
  id: "r-mbp", name: "mbp-dev", state: "claimed", owner: "developer", owner_kind: "person", online: true, machine_name: "mbp-dev", arch: "arm64",
  key_fingerprint: "SHA256:3Jd1d8…k2Q9", key_fingerprint_abbreviated: true, version: "0.10.0", created_at: createdAt, claimed_at: createdAt,
  last_seen_at: createdAt, kind: "self_hosted", pools: [{ id: "p-dev", name: "developer's computers" }],
  usage: { online: true, in_use: none, available: none, disk_capped: true },
});

/** Admin -> Runners with runners on and the given runners; the unused-token list is empty. */
async function adminRunners(page: Page, runners: unknown[] = []) {
  await page.route("**/api/v1/runners/settings", (r) => r.fulfill({ json: { enabled: true } }));
  await page.route("**/api/v1/runners/tokens", (r) => (r.request().method() === "GET" ? r.fulfill({ json: [] }) : r.fallback()));
  await page.route((u) => u.pathname === "/api/v1/runners", (r) => r.fulfill({ json: runners }));
  await gotoConsole(page, "admin");
  await page.goto("/admin/runners");
  const block = page.locator("[data-add-computer]");
  await expect(block.getByRole("heading", { name: "Add this computer" })).toBeVisible();
  return block;
}

test.describe("Admin -> Runners: Add this computer", () => {
  test.use({ userAgent: LINUX });
  test("the three steps replace the header action; the other OS and MDM are folded", async ({ page }) => {
    await healthz(page, { version: "0.10.0", cli_sign_in: true });
    const block = await adminRunners(page);
    await expect(block.getByText("Run these three commands in a terminal on the computer you are adding. It becomes a runner for your own runs.")).toBeVisible();
    await expect(block.getByRole("heading", { level: 3 })).toHaveText(["Install the CLI", "Sign in", "Set up the runner"]);
    await expect(block.getByText(SH, { exact: true })).toBeVisible();
    await expect(block.getByText(`wardyn login ${new URL(page.url()).origin}`, { exact: true })).toBeVisible();
    await expect(block.getByText("A browser tab opens. Approve the sign-in there once.")).toBeVisible();
    // On desktop the button stays in the same row as its code, inside the row's border.
    const row = block.locator("[data-cmd-box]", { hasText: "wardyn runner setup" }).locator("..");
    const rowBox = await row.boundingBox();
    const btn = await block.getByRole("button", { name: "Copy setup command" }).boundingBox();
    expect(rowBox && btn && btn.y >= rowBox.y && btn.y + btn.height <= rowBox.y + rowBox.height).toBe(true);
    await expect(block.getByText("wardyn runner setup", { exact: true })).toBeVisible();
    await expect(block.getByText("It checks this computer, registers and claims the runner, starts it, and waits up to 60 seconds for it to connect. Run it again to upgrade.")).toBeVisible();
    // The header keeps Turn off runners and loses the token action.
    await expect(page.getByRole("button", { name: "Turn off runners" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Create a registration token" })).toHaveCount(0);
    // Nothing on the block shows a start command, a proxy image, a token value or a token assignment.
    const text = (await block.textContent()) ?? "";
    expect(text).not.toMatch(/\bserve\b/);
    for (const banned of ["--proxy-image", "wdn_", "WARDYN_TOKEN="]) expect(text).not.toContain(banned);

    // The other operating system is folded under its own title.
    const other = block.locator("details", { hasText: "Another operating system" });
    await expect(other).not.toHaveAttribute("open", "");
    await other.getByText("Another operating system").click();
    await expect(other.getByText("Windows (PowerShell)")).toBeVisible();
    await expect(other.getByText(PS, { exact: true })).toBeVisible();

    const mdm = block.locator("details", { hasText: "Managed install (MDM)" }).last();
    await expect(mdm).not.toHaveAttribute("open", "");
    await mdm.getByText("Managed install (MDM)").click();
    await expect(mdm.getByText("For computers set up by device management: create a registration token for a person and deliver it to their computer.")).toBeVisible();
    await expect(mdm.getByText("The token registers one computer for this person. Only they can claim it, from their own account — you can't claim it for them.")).toBeVisible();
  });

  test("with CLI sign-in off, step 2 carries the WARDYN_TOKEN sentence", async ({ page }) => {
    await healthz(page, { version: "0.10.0", cli_sign_in: false });
    const block = await adminRunners(page);
    const step = block.locator("li", { has: page.getByRole("heading", { name: "Sign in" }) });
    await expect(step.getByText(NO_CLI_SIGN_IN)).toBeVisible();
    await expect(step.getByText("A browser tab opens. Approve the sign-in there once.")).toHaveCount(0);
  });

  test("a deployment that reports no release version says there is no install line", async ({ page }) => {
    await healthz(page, { version: "0.10.0+abc1234", cli_sign_in: true });
    const block = await adminRunners(page);
    await expect(block.getByText("This deployment didn't report its version, so there is no install line here. See Other ways to install.")).toBeVisible();
    await expect(block.getByRole("link", { name: "Other ways to install" })).toBeVisible();
  });

  test("Managed install (MDM): Create token shows the token once and the setup command, with no start command", async ({ page }) => {
    await healthz(page, { version: "0.10.0", cli_sign_in: true });
    await page.route("**/api/v1/runners/tokens", (r) =>
      r.request().method() === "POST"
        ? r.fulfill({
            status: 201,
            json: { id: "t1", owner: "dev@example.com", owner_kind: "person", minted_by_kind: "human", break_glass: false, minted_by: "admin", token: "wdr_3kFj9aQm7TxP2vLd8NcRwYe1HbZu4Sg6", created_at: aheadByHours(0), expires_at: aheadByHours(1), org_url: "https://wardyn.example.com", proxy_image: "ghcr.io/example/wardyn-proxy:0.10.0" },
          })
        : r.fallback(),
    );
    const block = await adminRunners(page);
    const mdm = block.locator("details", { hasText: "Managed install (MDM)" }).last();
    await mdm.getByText("Managed install (MDM)").click();
    await mdm.getByRole("textbox").fill("dev@example.com");
    await mdm.getByRole("button", { name: "Create token" }).click();
    await expect(mdm.getByRole("heading", { name: "Registration token for dev@example.com" })).toBeFocused();
    await expect(mdm.getByText("Shown once. Copy it now and deliver it to their computer as a file only they can read — for example with your device management tool.")).toBeVisible();
    await expect(mdm.getByText("wdr_3kFj9aQm7TxP2vLd8NcRwYe1HbZu4Sg6", { exact: true })).toBeVisible();
    await expect(mdm.getByText("On their computer, the runner registers with:")).toBeVisible();
    await expect(mdm.getByText("wardyn-runnerd setup --org https://wardyn.example.com --token-file <file>", { exact: true })).toBeVisible();
    await expect(mdm.getByText("Their computer then waits for dev@example.com to claim it in Your account → My runner.")).toBeVisible();
    const text = (await mdm.textContent()) ?? "";
    expect(text).not.toMatch(/\bserve\b/);
    expect(text).not.toContain("--proxy-image");
  });

  test("after setup, the runner is in the list online and says it registered just now", async ({ page }) => {
    await healthz(page, { version: "0.10.0", cli_sign_in: true });
    await adminRunners(page, [mbp(new Date(Date.now() - 5_000).toISOString())]);
    const card = page.getByRole("list", { name: "Runners" }).getByRole("listitem", { name: "mbp-dev" });
    await expect(card.getByText("Online", { exact: true })).toBeVisible();
    await expect(card.getByText("Registered just now")).toBeVisible();
  });

  test("a runner that never connected shows the never-connected notice (D2a) under the block", async ({ page }) => {
    await healthz(page, { version: "0.10.0", cli_sign_in: true });
    const never = { ...mbp(aheadByHours(-1)), online: false, last_seen_at: undefined, usage: { online: false, in_use: none, available: none, disk_capped: true } };
    await adminRunners(page, [never]);
    const card = page.getByRole("list", { name: "Runners" }).getByRole("listitem", { name: "mbp-dev" });
    await expect(card.getByText("Never connected", { exact: true })).toBeVisible();
    await expect(card.locator("[data-offline-note]")).toContainText("This runner registered but has never connected to this server.");
  });

  test.describe("viewing from Windows", () => {
    test.use({ userAgent: WINDOWS });
    test("the block says the runner runs in WSL2 and shows only the shell line", async ({ page }) => {
      await healthz(page, { version: "0.10.0", cli_sign_in: true });
      const block = await adminRunners(page);
      await expect(block.getByText("Runners run in WSL2. Open your WSL2 distribution and run all three commands there.")).toBeVisible();
      await expect(block.getByText(SH, { exact: true })).toBeVisible();
      await expect(block.getByText("Another operating system")).toHaveCount(0);
      await expect(block.getByText(PS, { exact: true })).toHaveCount(0);
    });
  });

  test.describe("on a phone", () => {
    test("the steps stack, each copy button and fold is 44 px tall, and the page does not scroll sideways", async ({ page }) => {
      await healthz(page, { version: "0.10.0", cli_sign_in: true });
      // The console's own sign-in landing is walked at desktop width (its nav collapses on a phone), then the page narrows.
      const block = await adminRunners(page);
      await page.setViewportSize({ width: 390, height: 844 });
      await expect(block.getByText(SH, { exact: true })).toBeVisible();
      for (const name of ["Copy install line for macOS, Linux and WSL2", "Copy sign-in command", "Copy setup command"]) {
        const box = await block.getByRole("button", { name }).boundingBox();
        expect(box?.height).toBeGreaterThanOrEqual(44);
      }
      // Each Copy button is full width under its code box, and "Other ways to install" is a 44 px row.
      const wide = await block.getByRole("button", { name: "Copy sign-in command" }).boundingBox();
      // The bordered box is the code's wrapper; the button is a row below it, not inside its border.
      const box = await block.locator("[data-cmd-box]", { hasText: "wardyn login" }).boundingBox();
      expect(wide && box && wide.y >= box.y + box.height).toBe(true);
      expect(wide?.width).toBeGreaterThan(250);
      expect((await block.getByRole("link", { name: "Other ways to install" }).boundingBox())?.height).toBeGreaterThanOrEqual(44);
      for (const title of ["Another operating system", "Managed install (MDM)"]) {
        const box = await block.locator("summary", { hasText: title }).boundingBox();
        expect(box?.height).toBeGreaterThanOrEqual(44);
      }
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    });
  });
});

test.describe("Your account -> My runner: Add this computer", () => {
  test.use({ userAgent: LINUX });
  test("with one runner and the card open, a second computer that registers appears without a reload", async ({ page }) => {
    await healthz(page, { version: "0.10.0", cli_sign_in: true });
    const none = { runs: 0, cpu_millis: 0, memory_mib: 0, disk_mib: 0 };
    const mine = (id: string, name: string) => ({ ...mbp(aheadByHours(-2)), id, name, owner: MEMBER_PRINCIPAL, usage: { online: true, in_use: none, available: none, disk_capped: true } });
    let list = [mine("r-one", "desk-1")];
    await page.route("**/api/v1/me/runners*", (r) => (r.request().method() === "GET" ? r.fulfill({ json: list }) : r.fallback()));
    await asRealMember(page);
    await gotoConsole(page, "user");
    await navTo(page, "Your account");
    await page.getByRole("button", { name: /^My runner/ }).click();
    await expect(page.locator("[data-add-computer]")).toBeVisible();
    const mineCard = page.locator("#my-runner");
    await expect(mineCard.getByRole("heading", { name: "desk-1", level: 4 })).toBeVisible();
    list = [...list, mine("r-two", "desk-2")];
    // The card re-reads on its own 5 s poll while open; the new row is awaited, never slept for.
    await expect(mineCard.getByRole("heading", { name: "desk-2", level: 4 })).toBeVisible({ timeout: 15_000 });
  });

  test("with no runner, the same three steps stand in place of Register a runner, without the MDM section", async ({ page }) => {
    await healthz(page, { version: "0.10.0", cli_sign_in: true });
    await page.route("**/api/v1/me/runners*", (r) => (r.request().method() === "GET" ? r.fulfill({ json: [] }) : r.fallback()));
    await asRealMember(page);
    await gotoConsole(page, "user");
    await navTo(page, "Your account");
    await page.getByRole("button", { name: /^My runner/ }).click();
    const block = page.locator("[data-add-computer]");
    await expect(page.getByText("You haven't registered a runner.")).toBeVisible();
    await expect(block.getByRole("heading", { level: 5 })).toHaveText(["Install the CLI", "Sign in", "Set up the runner"]);
    await expect(block.getByText(SH, { exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: "Register a runner" })).toHaveCount(0);
    await expect(block.getByText("Managed install (MDM)")).toHaveCount(0);
  });
});
