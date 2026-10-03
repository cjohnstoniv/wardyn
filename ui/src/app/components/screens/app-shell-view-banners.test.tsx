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
import { BANNER } from "../wardyn/copy/door";
import { CONSOLE_VIEW } from "../wardyn/copy/console-view";
import { ModelAccessProvider } from "../wardyn/model-access-context";
import { GOVERNED_ADMIN_BANNER } from "../../lib/access-posture-copy";
import { POSTURE_UNENFORCED_BANNER } from "../wardyn/confinement-posture-copy";
import { NO_BARRIER } from "../wardyn/copy";
import { MODEL_PROVIDERS, providerStatus } from "../../lib/test-fixtures";
// Both bands are React.lazy chunks, and the strip's is the slow one (the login
// pane rides in it). Loaded here, each lazy import resolves from the module
// cache on first render, so a positive control proves its neighbour rendered
// too — an absence below is the view rule, not a chunk still in flight.
import "../wardyn/model-access-banner";
import "../wardyn/confinement-posture";
import "../wardyn/governed-admin-banner";

// A person not yet signed in to their claude-code default's AWS sign-in: the
// strip's B1 line, User view only.
const STRIP = BANNER.B1("Claude Code", MODEL_PROVIDERS.bedrock.name);
const STATUS = providerStatus([{ provider: MODEL_PROVIDERS.bedrock, defaultFor: ["claude-code"] }], {
  harnesses: [{ id: "claude-code", display: "Claude Code", has_gateway: false, has_login: true }],
});

const ADMIN_ME = {
  principal: "admin@corp.example",
  method: "sso",
  operator: true,
  security_operator: true,
  role: "admin",
};

afterEach(() => vi.unstubAllGlobals());

function renderShellAt(
  path: string,
  me: Record<string, unknown>,
  healthExtra: Record<string, unknown> = {},
  noBarrier?: boolean,
  status: typeof STATUS = STATUS,
) {
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
          status={status}
          onRefresh={() => {}}
        >
          <Routes>
            <Route
              path="*"
              element={
                <AppShell pendingApprovals={0} attentionCount={0} onSignOut={() => {}} noBarrier={noBarrier} />
              }
            >
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
    renderShellAt("/admin/runs", ADMIN_ME, { runner: "k8s", network_policy: "unenforced" });
    // Positive control first: the posture band is Admin-view only, so seeing it
    // proves /me resolved to the Admin view.
    expect(await screen.findByText(POSTURE_UNENFORCED_BANNER.TITLE)).toBeInTheDocument();
    expect(screen.queryByText(STRIP)).toBeNull();
  });

  it("the same admin sees it once switched to the User view", async () => {
    // user_view: true is the session clamp the switch flips (§2.2) — an SSO
    // admin who has NOT switched stays in the Admin view regardless of the
    // URL (currentView), so this is what actually puts the session's
    // resolved view at "user".
    renderShellAt("/runs", { ...ADMIN_ME, user_view: true });
    expect(await screen.findByText(STRIP)).toBeInTheDocument();
  });
});

// Constrained-admin mode (mock M10): the governed-admin band is Admin view
// only, and reads the session's resolved view like the posture band.
describe("the governed-admin band follows the resolved view (mock M10)", () => {
  const GOVERNED = {
    ...STATUS,
    auth: { mode: "sso", local_loopback: false, govern_admin_runs: true },
  } as typeof STATUS;

  it("an admin in /admin/runs sees it", async () => {
    renderShellAt("/admin/runs", ADMIN_ME, {}, undefined, GOVERNED);
    expect(await screen.findByText(GOVERNED_ADMIN_BANNER.TITLE)).toBeInTheDocument();
  });

  it("the same admin in the User view does not", async () => {
    renderShellAt("/runs", { ...ADMIN_ME, user_view: true }, {}, undefined, GOVERNED);
    // Positive control: the User view's own per-user strip proves the shell resolved.
    expect(await screen.findByText(STRIP)).toBeInTheDocument();
    expect(screen.queryByText(GOVERNED_ADMIN_BANNER.TITLE)).toBeNull();
  });

  it("is silent with the switch off", async () => {
    renderShellAt("/admin/runs", ADMIN_ME, { runner: "k8s", network_policy: "unenforced" });
    expect(await screen.findByText(POSTURE_UNENFORCED_BANNER.TITLE)).toBeInTheDocument();
    expect(screen.queryByText(GOVERNED_ADMIN_BANNER.TITLE)).toBeNull();
  });
});

describe("the confinement posture band is absent in the User view (§4.2, M-3)", () => {
  it("an unenforced posture warns an admin in /admin/runs", async () => {
    renderShellAt("/admin/runs", ADMIN_ME, { runner: "k8s", network_policy: "unenforced" });
    expect(await screen.findByText(POSTURE_UNENFORCED_BANNER.TITLE)).toBeInTheDocument();
  });

  it("the same unenforced posture stays silent for a user in /runs", async () => {
    renderShellAt("/runs", { principal: "m@corp.example", method: "sso", role: "user", operator: false, security_operator: false }, {
      runner: "k8s",
      network_policy: "unenforced",
    });
    // Positive control first: the user's own per-user strip proves /me
    // resolved to the User view.
    expect(await screen.findByText(STRIP)).toBeInTheDocument();
    expect(screen.queryByText(POSTURE_UNENFORCED_BANNER.TITLE)).toBeNull();
  });
});

describe("a clamped admin at an /admin/* path reads the resolved view, not the URL (§4.2, M-3)", () => {
  it("a clamped admin's posture band stays silent at /admin/runs", async () => {
    // The interstitial case the PR description calls out: an SSO admin who
    // has switched to the User view (user_view: true) but still has an
    // /admin/* path in the address bar (e.g. a stale tab). useShellView
    // resolves this session to the User view regardless of the URL, so the
    // two bands must follow that resolved view, not viewOfPath(pathname) —
    // which would still say "admin" here.
    renderShellAt("/admin/runs", { ...ADMIN_ME, user_view: true }, { runner: "k8s", network_policy: "unenforced" });
    // Positive control first: the User view's own title proves the shell
    // resolved this session to the User view.
    await waitFor(() => expect(document.title).toBe(CONSOLE_VIEW.TITLE_USER));
    expect(screen.queryByText(POSTURE_UNENFORCED_BANNER.TITLE)).toBeNull();
  });
});

// #214 (#1328 review round 2, R2-1) — the shell's own global route to the
// fix, present in BOTH views (unlike the two view-scoped bands above). Only
// /admin/setup?step=environment ever reaches the Environment step (the plain
// /setup path always renders the read-only member recap regardless of
// `?step=`, onboarding-screen.tsx's GettingStarted) — so the CTA renders only
// for a caller who can actually get there: an operator, or a session-user
// via ViewGate's own "to-admin" click. Everyone else reads the reason with
// no link at all.
describe("the #214 no-barrier banner: present in both views, CTA gated on who can reach it", () => {
  it("a member in the User view: the reason shows, with no CTA at all", async () => {
    renderShellAt(
      "/runs",
      { principal: "m@corp.example", method: "sso", role: "user", operator: false, security_operator: false },
      {},
      true,
    );
    expect(await screen.findByText(NO_BARRIER.BANNER_TITLE)).toBeInTheDocument();
    expect(screen.getByText(NO_BARRIER.BANNER_BODY)).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: NO_BARRIER.CTA })).toBeNull();
  });

  it("an operator in the Admin view: the banner routes into /admin/setup, and the top-bar link is absent (no New run there)", async () => {
    renderShellAt("/admin/runs", ADMIN_ME, {}, true);
    expect(await screen.findByText(NO_BARRIER.BANNER_TITLE)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: NO_BARRIER.CTA })).toHaveAttribute(
      "href",
      "/admin/setup?step=environment",
    );
    expect(screen.queryByRole("button", { name: "New run" })).toBeNull();
  });

  // R2-1's central fix: an admin who switched to the User view is clamped by
  // the server exactly like a plain member (role "user", operator false —
  // internal/api/me.go's own doc), so `meta.operator` alone cannot tell them
  // apart. `access === "session-user"` is what does: the CTA still renders,
  // routed at the SAME /admin/setup?step=environment — ViewGate answers with
  // the "to-admin" interstitial (never a refusal) the moment it's clicked,
  // and that click's own target already carries the query string.
  it("an SSO admin who switched to the User view (session-user): the CTA still offers /admin/setup, from both the banner and the top bar", async () => {
    renderShellAt(
      "/runs",
      { principal: "admin-in-member@corp.example", method: "sso", role: "user", operator: false, security_operator: false, user_view: true, user_view_super_admin: true },
      {},
      true,
    );
    expect(await screen.findByText(NO_BARRIER.BANNER_TITLE)).toBeInTheDocument();
    const links = screen.getAllByRole("link", { name: NO_BARRIER.CTA });
    expect(links).toHaveLength(2);
    for (const link of links) {
      expect(link).toHaveAttribute("href", "/admin/setup?step=environment");
    }
  });

  // #1335 — a security admin in the User view is session-user too, but
  // /admin/setup would refuse them: no link from the banner or the top bar.
  it("a security admin in the User view: the reason shows, with no CTA at either site", async () => {
    renderShellAt(
      "/runs",
      { principal: "sec-in-view@corp.example", method: "sso", role: "user", operator: false, security_operator: false, user_view: true, user_view_super_admin: false },
      {},
      true,
    );
    expect(await screen.findByText(NO_BARRIER.BANNER_TITLE)).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: NO_BARRIER.CTA })).toBeNull();
  });

  it("a security admin (not a full operator) in the Admin view: no CTA — /admin/setup would only refuse them", async () => {
    renderShellAt(
      "/admin/runs",
      { principal: "sec@corp.example", method: "sso", role: "security_admin", operator: false, security_operator: true },
      {},
      true,
    );
    expect(await screen.findByText(NO_BARRIER.BANNER_TITLE)).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: NO_BARRIER.CTA })).toBeNull();
  });

  it("stays silent when the deployment has a barrier", async () => {
    renderShellAt("/runs", { principal: "m@corp.example", method: "sso", role: "user", operator: false, security_operator: false }, {}, false);
    await screen.findByText("screen");
    expect(screen.queryByText(NO_BARRIER.BANNER_TITLE)).toBeNull();
    expect(screen.queryByRole("link", { name: NO_BARRIER.CTA })).toBeNull();
  });
});
