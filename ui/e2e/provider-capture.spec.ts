/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { Page, WebSocketRoute } from "@playwright/test";
import { test, expect, gotoConsole, mockMemberRole, navToRoute } from "./fixtures";
import { attachModeFrame, stubAttachSocket, stubAttachTicket } from "./attach-stub";
import { MODEL_ACCESS_BANNER } from "../src/app/components/wardyn/model-access-copy";
import { CONNECTIONS } from "../src/app/components/wardyn/copy/door";
import { AGENTS } from "../src/app/lib/workspace-providers-copy";

// A provider's AWS sign-in, proven by the provider row's own source_run_id
// (#993), and the strip carrying the server's action for a session another
// account/role signed (#993).
//
// Hermetic for signin-door-aws.spec.ts's reason: `-runner none` never starts a
// login run, so the provider's launch, the run's reads, its kill, the attach
// ticket and socket are stubbed, and /setup/status is spliced. What is real is
// the console's door, its pane and its corroboration of the capture.

const BEDROCK = {
  id: "bedrock-prod",
  name: "Bedrock (prod)",
  kind: "bedrock_sso",
  harnesses: ["claude-code"],
  default_for: ["claude-code"],
  host: "bedrock-runtime.us-east-1.amazonaws.com",
};
const RUN = "3f1b7c26-0000-4000-8000-00000000c993";

/** Serve /setup/status from one real read, with `access` as the provider row. */
async function statusWith(page: Page, access: () => Record<string, unknown>): Promise<void> {
  let base: Record<string, unknown> | null = null;
  await page.route("**/api/v1/setup/status*", async (route) => {
    if (!base) base = (await (await route.fetch()).json()) as Record<string, unknown>;
    await route.fulfill({ json: { ...base, model_providers: [BEDROCK], provider_access: [access()] } });
  });
}

test.describe("a provider's AWS sign-in capture (#993)", () => {
  test("the door completes when the capture's audit row is spooled", async ({ page }) => {
    await mockMemberRole(page);
    let captured = false;
    await statusWith(page, () =>
      captured
        ? { provider: BEDROCK.id, state: "live", source_run_id: RUN }
        : { provider: BEDROCK.id, state: "not_configured", action: AGENTS.SIGN_IN_AWS },
    );
    await page.route(`**/api/v1/model-providers/${BEDROCK.id}/sign-in`, async (route) => {
      if (route.request().method() !== "POST") return route.fallback();
      await stubAttachTicket(page, RUN);
      await route.fulfill({ json: { run_id: RUN, state: "PENDING" } });
    });
    await page.route(new RegExp(`/api/v1/runs/${RUN}$`), (route) =>
      route.fulfill({ json: { id: RUN, task: "harness login", interactive: true, state: "RUNNING" } }),
    );
    await page.route(new RegExp(`/api/v1/runs/${RUN}/kill$`), (route) => route.fulfill({ status: 202, json: {} }));
    // The capture row never reached the sink: /audit answers nothing for the run.
    await page.route("**/api/v1/audit*", (route) => route.fulfill({ json: [] }));
    let resolveSocket: (ws: WebSocketRoute) => void = () => {};
    const socket = new Promise<WebSocketRoute>((r) => (resolveSocket = r));
    await stubAttachSocket(page, (_n, ws) => {
      ws.send(attachModeFrame(false));
      resolveSocket(ws);
    });

    await gotoConsole(page);
    await navToRoute(page, "/runs");
    await page.getByRole("button", { name: AGENTS.SIGN_IN_AWS }).click();
    await expect(page.getByRole("dialog", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toContainText("For Bedrock (prod)");

    captured = true;
    (await socket).send(Buffer.from("wardyn: aws sso credential captured\r\n"));
    await expect(page.getByText(MODEL_ACCESS_BANNER.SIGNED_IN_TOAST)).toBeVisible();
    await expect(page.getByRole("dialog")).toHaveCount(0);
  });

  test("the strip shows the server's action for a session another account/role signed", async ({ page }) => {
    const action =
      "Your stored AWS session is for account 999999999999 / role Wrong; this row now allows 123456789012 / BedrockUser — sign in again.";
    await mockMemberRole(page);
    await statusWith(page, () => ({ provider: BEDROCK.id, state: "expired_signin", action, source_run_id: RUN }));

    await gotoConsole(page);
    await navToRoute(page, "/runs");
    await expect(page.getByText(CONNECTIONS.C6_LINE("Bedrock (prod)"))).toBeVisible();
    await expect(page.getByText(action)).toBeVisible();
    await expect(page.getByRole("button", { name: AGENTS.SIGN_IN_AWS, exact: true })).toBeVisible();
  });
});
