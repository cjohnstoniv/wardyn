/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The shell's half of the model-access door. Its own file
// because app-shell.test.tsx is at the 1000-line gate
// (scripts/check-file-size.sh) — and because this is a different seam: every
// case here is about the BANNER STACK and the live region around it, not about
// the nav, the account menu or the drive context the sibling file pins.
import { describe, it, expect, vi, afterEach } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { AppShell, SESSION_EXPIRY_COPY } from "./app-shell";
import { ThemeProvider } from "../wardyn/theme-provider";
import { BANNER } from "../wardyn/copy/door";
import { ModelAccessProvider } from "../wardyn/model-access-context";
import { AGENTS } from "../../lib/workspace-providers-copy";
import { MODEL_PROVIDERS, providerStatus } from "../../lib/test-fixtures";
import { aheadByHours } from "../../lib/test-clock";

// The model-access strip's place in the stack.
//
// The band itself is pinned by model-access-banner.test.tsx; what only the
// shell can prove is WHERE it sits and where it is withheld. It renders LAST:
// a dying session, a dead control plane and an unknown identity are each the
// better explanation of what you are looking at, and are read first.
describe("AppShell (the model-access strip)", () => {
  const HARNESSES = [{ id: "claude-code", display: "Claude Code", has_gateway: false, has_login: true }];
  const STRIP = BANNER.B1("Claude Code", MODEL_PROVIDERS.bedrock.name);
  const statusAt = (state: string) =>
    providerStatus([{ provider: MODEL_PROVIDERS.bedrock, defaultFor: ["claude-code"], state }], { harnesses: HARNESSES });

  afterEach(() => vi.unstubAllGlobals());

  /** `me` as a promise lets a case hold /me open — the window where
   *  useOperator() is still the fail-open default. */
  function renderShellAt(path: string, me: Record<string, unknown> | Promise<Record<string, unknown>>) {
    vi.stubGlobal(
      "fetch",
      vi.fn((url: RequestInfo | URL) => {
        const u = String(url);
        if (u.endsWith("/healthz"))
          return Promise.resolve({
            ok: true,
            json: async () => ({ trust_domain: "wardyn.local", identity_provider: "embedded" }),
          });
        if (u.endsWith("/api/v1/me")) return Promise.resolve({ ok: true, json: async () => await me });
        return Promise.resolve({ ok: true, json: async () => ({}) });
      }) as unknown as typeof fetch,
    );
    return render(
      <MemoryRouter initialEntries={[path]}>
        <ThemeProvider>
          <ModelAccessProvider
            status={statusAt("not_configured")}
            onRefresh={() => {}}
          >
            <Routes>
              <Route
                path="*"
                element={<AppShell pendingApprovals={0} attentionCount={0} onSignOut={() => {}} />}
              >
                <Route path="*" element={<div>screen</div>} />
              </Route>
            </Routes>
          </ModelAccessProvider>
        </ThemeProvider>
      </MemoryRouter>,
    );
  }

  // A function, not a describe-body constant: aheadByHours(1/60) must be
  // computed inside the test (after any clock-advancing beforeEach has
  // already run), never at collection time — a fixed collection-time
  // "1 minute from now" reads as long-expired once the clock moves (#195-b).
  function MEMBER_WITH_DYING_SESSION() {
    return {
      principal: "alice@corp.example",
      method: "sso",
      operator: false,
      security_operator: false,
      role: "user",
      email: "alice@corp.example",
      // Inside SESSION_WARN_MS, so the session strip is on screen too.
      session_expires_at: aheadByHours(1 / 60), // 1 minute
    };
  }

  it("renders BELOW the session-expiry banner", async () => {
    renderShellAt("/runs", MEMBER_WITH_DYING_SESSION());
    const session = await screen.findByText(SESSION_EXPIRY_COPY.soon[0]);
    const model = await screen.findByText(STRIP);
    // DOCUMENT_POSITION_FOLLOWING: `model` comes after `session` in the DOM.
    expect(session.compareDocumentPosition(model) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("is withheld on /setup — that page IS the door", async () => {
    renderShellAt("/setup", MEMBER_WITH_DYING_SESSION());
    await screen.findByText(SESSION_EXPIRY_COPY.soon[0]);
    expect(screen.queryByText(STRIP)).toBeNull();
  });

  it("is withheld on /admin/settings — the Admin view carries no provider strip", async () => {
    renderShellAt("/admin/settings", { ...MEMBER_WITH_DYING_SESSION(), operator: true, role: "admin" });
    await screen.findByText(SESSION_EXPIRY_COPY.soon[0]);
    await waitFor(() => expect(screen.queryByText(STRIP)).toBeNull());
  });

  // …and a person's own connections page keeps it.
  it("stays for a user on /account", async () => {
    renderShellAt("/account", MEMBER_WITH_DYING_SESSION());
    expect(await screen.findByText(STRIP)).toBeInTheDocument();
  });

  // The live region is the SHELL's and it is EAGER. role="status" announces
  // CHANGES to a mounted region; a region that arrives together with its first
  // sentence — which is what a lazy chunk does — announces nothing.
  it("mounts its live region before the lazy strip, and with nothing to say", () => {
    // No dying session and a live credential: this wrapper is then the only
    // role=status region in the shell, and it is empty.
    vi.stubGlobal(
      "fetch",
      vi.fn((url: RequestInfo | URL) => {
        const u = String(url);
        if (u.endsWith("/healthz"))
          return Promise.resolve({
            ok: true,
            json: async () => ({ trust_domain: "wardyn.local", identity_provider: "embedded" }),
          });
        if (u.endsWith("/api/v1/me"))
          return Promise.resolve({ ok: true, json: async () => ({ principal: "a@b", role: "user", operator: false }) });
        return Promise.resolve({ ok: true, json: async () => ({}) });
      }) as unknown as typeof fetch,
    );
    render(
      <MemoryRouter initialEntries={["/runs"]}>
        <ThemeProvider>
          <ModelAccessProvider status={statusAt("live")} onRefresh={() => {}}>
            <Routes>
              <Route path="*" element={<AppShell pendingApprovals={0} attentionCount={0} onSignOut={() => {}} />}>
                <Route path="*" element={<div>screen</div>} />
              </Route>
            </Routes>
          </ModelAccessProvider>
        </ThemeProvider>
      </MemoryRouter>,
    );
    expect(screen.getByRole("status")).toBeEmptyDOMElement();
  });

  // useOperator()'s fail-open default is TRUE, and usePrincipal() is "" in
  // the same window, so a "Not now" there would write an unkeyed flag. The
  // strip says nothing until the identity is known.
  it("says nothing until /me answers, then the person's own line", async () => {
    let answer: (me: Record<string, unknown>) => void = () => {};
    const pending = new Promise<Record<string, unknown>>((resolve) => (answer = resolve));
    vi.stubGlobal(
      "fetch",
      vi.fn((url: RequestInfo | URL) => {
        const u = String(url);
        if (u.endsWith("/healthz"))
          return Promise.resolve({
            ok: true,
            json: async () => ({ trust_domain: "wardyn.local", identity_provider: "embedded" }),
          });
        if (u.endsWith("/api/v1/me")) return Promise.resolve({ ok: true, json: async () => await pending });
        return Promise.resolve({ ok: true, json: async () => ({}) });
      }) as unknown as typeof fetch,
    );
    render(
      <MemoryRouter initialEntries={["/runs"]}>
        <ThemeProvider>
          <ModelAccessProvider status={statusAt("not_configured")} onRefresh={() => {}}>
            <Routes>
              <Route path="*" element={<AppShell pendingApprovals={0} attentionCount={0} onSignOut={() => {}} />}>
                <Route path="*" element={<div>screen</div>} />
              </Route>
            </Routes>
          </ModelAccessProvider>
        </ThemeProvider>
      </MemoryRouter>,
    );

    // While /me is in flight: no line and no button. Two macrotasks first, so
    // the strip's own lazy chunk has certainly resolved and the silence below
    // is the strip's answer rather than a chunk that had not arrived yet.
    await act(async () => {
      await new Promise((r) => setTimeout(r, 0));
      await new Promise((r) => setTimeout(r, 0));
    });
    expect(screen.getByRole("status")).toBeInTheDocument();
    expect(screen.queryByText(STRIP)).toBeNull();
    expect(screen.queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeNull();

    answer({ principal: "member@corp.example", role: "user", operator: false, security_operator: false });
    expect(await screen.findByText(STRIP)).toBeInTheDocument();
  });
});
