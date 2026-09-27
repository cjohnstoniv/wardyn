/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * LL4 — AWS SSO SIGN-IN LANDS ON ENTRA (docs/LIVE-TESTS.md).
 *
 * Starts the same public device authorization the per-user AWS SSO login
 * starts (register a public client, then StartDeviceAuthorization for the
 * start URL), runs the verification URL through the console's own extractor,
 * and opens it in a fresh, signed-out browser. The identity source is Entra,
 * so the page has to arrive on an Entra sign-in page. Nothing is signed in and
 * the device code is never approved; it expires on its own.
 */

import { test, expect } from "@playwright/test";
import { extractDeviceVerificationUrl } from "../../src/app/components/screens/settings/login-pty-extract";
import { missing, where } from "./harness";

const why = missing("WARDYN_LIVE_AWS_SSO", [
  "WARDYN_LIVE_AWS_SSO_START_URL",
  "WARDYN_LIVE_AWS_SSO_REGION",
]);
test.skip(why !== null, why ?? "");

async function oidc<T>(region: string, path: string, body: object): Promise<T> {
  const r = await fetch(`https://oidc.${region}.amazonaws.com${path}`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
  // Status only: the body of a refusal can echo the start URL.
  if (!r.ok) throw new Error(`AWS SSO OIDC ${path}: HTTP ${r.status}`);
  return (await r.json()) as T;
}

test("the per-user AWS SSO device URL lands on an Entra sign-in page", async ({
  browser,
}) => {
  const region = process.env.WARDYN_LIVE_AWS_SSO_REGION!;
  const client = await oidc<{ clientId: string; clientSecret: string }>(
    region,
    "/client/register",
    {
      clientName: "wardyn-live-local",
      clientType: "public",
    },
  );
  const device = await oidc<{ verificationUriComplete: string }>(
    region,
    "/device_authorization",
    {
      clientId: client.clientId,
      clientSecret: client.clientSecret,
      startUrl: process.env.WARDYN_LIVE_AWS_SSO_START_URL,
    },
  );
  // The extractor wants a terminated URL, as it would see one in the PTY.
  const url = extractDeviceVerificationUrl(
    `${device.verificationUriComplete}\n`,
  );
  expect(
    url !== null && url.includes("user_code="),
    "the console's extractor did not accept the device URL",
  ).toBe(true);

  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  await page.goto(url!);
  await expect
    .poll(() => where(page.url()), {
      message: "the device URL did not reach an Entra sign-in page",
      timeout: 60_000,
    })
    .toBe("entra");
  await ctx.close();
});
