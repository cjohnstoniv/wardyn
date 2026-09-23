/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// §4.2 (admin-member-modes-design.md, packet M-3, issue #635) — proves the
// SHELL wires the session's resolved view (useShellView), not the raw URL,
// into the two banners that differ by view. app-shell-model-access.test.tsx
// pins the strip's place in the stack; model-access-banner.test.tsx and
// confinement-posture.test.tsx pin each band's own view rule directly. This
// file is the one seam that proves the real pipeline end to end: an SSO
// principal's role from /me, through useShellView, into the mounted band.
import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { AppShell } from "./app-shell";
import { ThemeProvider } from "../wardyn/theme-provider";
import { MODEL_ACCESS_BANNER } from "../wardyn/model-access-copy";
import { ModelAccessProvider } from "../wardyn/model-access-context";
import { POSTURE_UNENFORCED_BANNER } from "../wardyn/confinement-posture-copy";
import { baseStatus } from "../../lib/test-fixtures";

const PER_USER_ROW = {
  id: "claude-code",
  display: "claude-code",
  has_gateway: false,
  has_login: true,
  enabled: true,
  mechanism: "bedrock_sso",
  credential_source: "per_user",
};

const ADMIN_ME = {
  principal: "admin@corp.example",
  method: "sso",
  operator: true,
  security_operator: true,
  role: "admin",
};

afterEach(() => vi.unstubAllGlobals());

function renderShellAt(path: string, me: Record<string, unknown>, healthExtra: Record<string, unknown> = {}) {
  vi.stubGlobal(
    "fetch",
    vi.fn((url: RequestInfo | URL) => {
      const u = String(url);
      if (u.endsWith("/healthz"))
        return Promise.resolve({
          ok: true,
          json: async () => ({ trust_domain: "wardyn.local", identity_provider: "embedded", ...healthExtra }),
        });
      if (u.endsWith("/api/v1/me")) return Promise.resolve({ ok: true, json: async () => me });
      return Promise.resolve({ ok: true, json: async () => ({}) });
    }) as unknown as typeof fetch,
  );
  return render(
    <MemoryRouter initialEntries={[path]}>
      <ThemeProvider>
        <ModelAccessProvider
          status={baseStatus({ model_access: { state: "not_configured" }, harnesses: [PER_USER_ROW] })}
          onRefresh={() => {}}
        >
          <Routes>
            <Route path="*" element={<AppShell pendingApprovals={0} attentionCount={0} onSignOut={() => {}} />}>
              <Route path="*" element={<div>screen</div>} />
            </Route>
          </Routes>
        </ModelAccessProvider>
      </ThemeProvider>
    </MemoryRouter>,
  );
}

describe("the model-access strip is absent in the Admin view (§4.2, M-3)", () => {
  it("a per-user deployment's strip does not reach an admin in /admin/runs", async () => {
    renderShellAt("/admin/runs", ADMIN_ME);
    await waitFor(() => expect(screen.getByRole("status")).toBeInTheDocument());
    expect(screen.queryByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN)).toBeNull();
  });

  it("the same admin sees it once switched to the User view", async () => {
    // member_mode: true is the session clamp the switch flips (§2.2) — an SSO
    // admin who has NOT switched stays in the Admin view regardless of the
    // URL (currentView), so this is what actually puts the session's
    // resolved view at "user".
    renderShellAt("/runs", { ...ADMIN_ME, member_mode: true });
    expect(await screen.findByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN)).toBeInTheDocument();
  });
});

describe("the confinement posture band is absent in the User view (§4.2, M-3)", () => {
  it("an unenforced posture warns an admin in /admin/runs", async () => {
    renderShellAt("/admin/runs", ADMIN_ME, { runner: "k8s", network_policy: "unenforced" });
    expect(await screen.findByText(POSTURE_UNENFORCED_BANNER.TITLE)).toBeInTheDocument();
  });

  it("the same unenforced posture stays silent for a user in /runs", async () => {
    renderShellAt("/runs", { principal: "m@corp.example", method: "sso", role: "member", operator: false, security_operator: false }, {
      runner: "k8s",
      network_policy: "unenforced",
    });
    await waitFor(() => expect(screen.getByRole("status")).toBeInTheDocument());
    expect(screen.queryByText(POSTURE_UNENFORCED_BANNER.TITLE)).toBeNull();
  });
});
