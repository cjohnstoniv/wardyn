/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { randomUUID } from "node:crypto";
import { test, expect, ADMIN_TOKEN, asRealMember, consoleAPI } from "./fixtures";

// Admin and member views: the doors the Admin view must not close, and the writes
// the member view must not allow.
//
// An SSO admin who is in the Admin view is refused 409 `admin_view` at the three
// launch doors (POST /runs, /runs/preflight, /runs/policy-preview), and only there:
// Record, the site-config probes and the model provider sign-in are operator controls
// and stay open to them. That session needs an OIDC install, so those arms are pinned
// in Go (internal/api/admin_view_launch_exceptions_test.go) and walked on the kind SSO
// estate. What this backend can prove is the other half: a plain admin token (a
// single-operator install) is never refused as the Admin view at any of those doors, and a real
// member is refused every admin write.

const auth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
const RUN = { agent: "claude-code", task: "t" };

test.describe("the admin token is never refused as the Admin view", () => {
  const doors: { method: "POST"; path: string; body: unknown }[] = [
    { method: "POST", path: "/api/v1/runs/preflight", body: RUN },
    { method: "POST", path: "/api/v1/runs/policy-preview", body: RUN },
    { method: "POST", path: "/api/v1/runs", body: RUN },
    { method: "POST", path: `/api/v1/workspaces/${randomUUID()}/record`, body: { name: "build" } },
    { method: "POST", path: "/api/v1/site-config/test-proxy", body: {} },
    { method: "POST", path: "/api/v1/site-config/test-redirect", body: { from: "registry.npmjs.org", to: "artifactory.corp/npm" } },
    { method: "POST", path: "/api/v1/model-providers/no-such-provider/sign-in", body: {} },
  ];
  for (const door of doors) {
    test(`${door.method} ${door.path.replace(/[0-9a-f-]{36}/, ":id")}`, async ({ page }) => {
      const res = await page.request.fetch(door.path, { method: door.method, headers: auth, data: door.body });
      // Some of these doors answer 409 for their own reasons; none answers the Admin view's.
      const text = await res.text();
      expect(text, `${res.status()}`).not.toContain("admin_view");
    });
  }
});

test("a real member is refused the admin writes, and the same session may still launch its own run", async ({ page }) => {
  await asRealMember(page);
  for (const [method, path, body] of [
    ["PUT", "/api/v1/site-config", {}],
    ["PUT", "/api/v1/workspace-providers", {}],
    ["POST", "/api/v1/site-config/test-proxy", {}],
  ] as const) {
    const refused = await consoleAPI(page, method, path, body);
    expect(refused.status, `${method} ${path}: ${refused.text}`).toBe(403);
  }
  const own = await consoleAPI(page, "POST", "/api/v1/runs", RUN);
  expect(own.status, own.text).toBe(201);
});
