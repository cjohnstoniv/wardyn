/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import type { Page } from "@playwright/test";
import { test, expect, ADMIN_TOKEN, gotoConsole, navTo } from "./fixtures";
import { RUN_SIGN_IN } from "../src/app/components/wardyn/copy/run-sign-in";
import { AWS_SSO_LOGIN_AGENT, HARNESS_LOGIN_TASK } from "../src/app/components/screens/run-detail/sign-in-run";

// The run page's Open button on an AWS sign-in sandbox, for the regional sign-in page. The strip
// draws it only when the server's sign-in read answers waiting, which it does for the regional
// sign-in page newer IAM Identity Center sends (https://<region>.signin.aws/platform/…, no
// user_code=, the code on its own line) once that code line has printed. The answer served here is the server's own for that
// pane, from the shared cases its parser is pinned on (internal/api/testdata/sign_in_pane_fixtures.json).
//
// HERMETIC: the daemon runs `-runner none`, so a seeded run is dressed as a RUNNING aws-sso login
// run owned by the console's principal (runs-detail.spec.ts's strip case does the same), and the
// sign-in read is routed.

const { cases } = JSON.parse(readFileSync(join(process.cwd(), "../internal/api/testdata/sign_in_pane_fixtures.json"), "utf8")) as {
  cases: { name: string; go: { state: string; verification_url?: string; user_code?: string } }[];
};
const PAGE_CASE = "the sign-in page link AWS IAM Identity Center sends";

async function dressAsAwsSignIn(page: Page): Promise<void> {
  const me = await (await page.request.get("/api/v1/me", { headers: { Authorization: `Bearer ${ADMIN_TOKEN}` } })).json();
  await page.route("**/api/v1/runs/*", async (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    const response = await route.fetch();
    const json = await response.json();
    if (json.task === "e2e fixture 6") {
      json.task = HARNESS_LOGIN_TASK;
      json.agent = AWS_SSO_LOGIN_AGENT;
      json.state = "RUNNING";
      json.created_by = me.principal;
    }
    await route.fulfill({ response, json });
  });
}

test.describe("the run page's AWS sign-in strip on the regional sign-in page", () => {
  test("Open opens the signin.aws link in a new tab, beside the pane's code", async ({ page }) => {
    const c = cases.find((x) => x.name.startsWith(PAGE_CASE));
    expect(c, `no shared case ${PAGE_CASE}`).toBeDefined();
    const answer = c!.go;
    expect(answer.state).toBe("waiting");
    const link = answer.verification_url ?? "";
    expect(link).toMatch(/^https:\/\/us-east-1\.signin\.aws\/platform\/authorize\?/);

    await gotoConsole(page);
    await dressAsAwsSignIn(page);
    await page.route("**/api/v1/runs/*/sign-in", (route) => route.fulfill({ json: answer }));
    await navTo(page, "Runs");
    await page.getByText("e2e fixture 6").click();

    const strip = page.getByTestId("run-sign-in-strip");
    await expect(strip).toBeVisible({ timeout: 15_000 });
    const open = strip.getByRole("link", { name: RUN_SIGN_IN.OPEN });
    await expect(open).toHaveAttribute("href", link);
    await expect(open).toHaveAttribute("target", "_blank");
    await expect(open).toHaveAttribute("rel", "noopener noreferrer");
    await expect(strip.getByLabel(RUN_SIGN_IN.CODE_LABEL)).toHaveText(answer.user_code ?? "");
    await expect(strip).toContainText(RUN_SIGN_IN.OPENS("us-east-1.signin.aws"));
  });
});
