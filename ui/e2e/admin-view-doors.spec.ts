/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { test, expect, asRealMember, consoleAPI } from "./fixtures";

// Admin and member views: the writes the member view must not allow.
//
// What the Admin view closes and leaves open belongs to an SSO admin session: it is refused 409
// `admin_view` at the three launch doors (POST /runs, /runs/preflight, /runs/policy-preview), and only
// there; Record, the site-config probes and the model provider sign-in stay open to it. That session
// needs an OIDC install, so those arms are pinned in Go
// (internal/api/admin_view_launch_exceptions_test.go) and walked on the kind SSO estate. A plain admin
// token never carries the Admin view, so a test with it could not fail and is not written. This backend
// can prove the other half: a real member is refused every admin write and may still launch its own run.

const RUN = { agent: "claude-code", task: "t" };

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
  // The run was this test's own: end it so the shared backend keeps no stray run.
  const { id } = JSON.parse(own.text) as { id: string };
  const killed = await consoleAPI(page, "POST", `/api/v1/runs/${id}/kill`, {});
  expect(killed.status, killed.text).toBe(202);
});
