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
import { MODEL_ACCESS_BANNER } from "../wardyn/model-access-copy";
import { ModelAccessProvider } from "../wardyn/model-access-context";
import { AGENTS } from "../../lib/workspace-providers-copy";
import { baseStatus } from "../../lib/test-fixtures";

// The model-access strip's place in the stack.
//
// The band itself is pinned by model-access-banner.test.tsx; what only the
// shell can prove is WHERE it sits and where it is withheld. It renders LAST:
// a dying session, a dead control plane and an unknown identity are each the
// better explanation of what you are looking at, and are read first.
describe("AppShell (the model-access strip)", () => {
  const PER_USER_ROW = {
    id: "claude-code",
    display: "claude-code",
    has_gateway: false,
    has_login: true,
    enabled: true,
    mechanism: "bedrock_sso",
    credential_source: "per_user",
  };

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
            status={baseStatus({
              model_access: { state: "not_configured", action: AGENTS.SIGN_IN_AWS },
              harnesses: [PER_USER_ROW],
            })}
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

  const MEMBER_WITH_DYING_SESSION = {
    principal: "alice@corp.example",
    method: "sso",
    operator: false,
    security_operator: false,
    role: "user",
    email: "alice@corp.example",
    // Inside SESSION_WARN_MS, so the session strip is on screen too.
    session_expires_at: new Date(Date.now() + 60_000).toISOString(),
  };

  it("renders BELOW the session-expiry banner", async () => {
    renderShellAt("/runs", MEMBER_WITH_DYING_SESSION);
    const session = await screen.findByText(SESSION_EXPIRY_COPY.soon[0]);
    const model = await screen.findByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN);
    // DOCUMENT_POSITION_FOLLOWING: `model` comes after `session` in the DOM.
    expect(session.compareDocumentPosition(model) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("is withheld on /setup — that page IS the door", async () => {
    renderShellAt("/setup", MEMBER_WITH_DYING_SESSION);
    await screen.findByText(SESSION_EXPIRY_COPY.soon[0]);
    expect(screen.queryByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN)).toBeNull();
  });

  it("is withheld on /settings for an OPERATOR, which already mounts the same pane", async () => {
    renderShellAt("/settings", { ...MEMBER_WITH_DYING_SESSION, operator: true, role: "admin" });
    await screen.findByText(SESSION_EXPIRY_COPY.soon[0]);
    await waitFor(() => expect(screen.queryByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN)).toBeNull());
  });

  // …and the member it does not: the Settings card's AWS button is
  // `disabled={!operator}` there, so hiding the strip would strand exactly the
  // person the refusal sentence sends to that page.
  it("stays for a MEMBER on /settings", async () => {
    renderShellAt("/settings", MEMBER_WITH_DYING_SESSION);
    expect(await screen.findByText(MODEL_ACCESS_BANNER.NOT_SIGNED_IN)).toBeInTheDocument();
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
          <ModelAccessProvider status={baseStatus({ model_access: { state: "live" } })} onRefresh={() => {}}>
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

  // useOperator()'s fail-open default is TRUE, so until /me lands a member
  // under a dead shared credential would read the ADMIN's sentence and be
  // offered a sign-in the server refuses. The door says nothing until the
  // identity is known.
  it("says nothing about a shared-dead credential until /me answers, then the member's line", async () => {
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
    const SHARED_ACTION = "Your admin's model credential expired — ask them to reconnect it";
    render(
      <MemoryRouter initialEntries={["/runs"]}>
        <ThemeProvider>
          <ModelAccessProvider
            status={baseStatus({
              model_access: { state: "shared_expired", action: SHARED_ACTION },
              harnesses: [{ ...PER_USER_ROW, credential_source: "shared" }],
            })}
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

    // While /me is in flight: no admin sentence, no button, no member line.
    // Two macrotasks first, so the strip's own lazy chunk has certainly
    // resolved and the silence below is the door's answer rather than a chunk
    // that had not arrived yet.
    await act(async () => {
      await new Promise((r) => setTimeout(r, 0));
      await new Promise((r) => setTimeout(r, 0));
    });
    expect(screen.getByRole("status")).toBeInTheDocument();
    expect(screen.queryByText(MODEL_ACCESS_BANNER.SHARED_ADMIN_EXPIRED)).toBeNull();
    expect(screen.queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeNull();
    expect(screen.queryByText(SHARED_ACTION)).toBeNull();

    answer({ principal: "member@corp.example", role: "user", operator: false, security_operator: false });
    // …and once it lands, the member reads the server's instruction, with no
    // button: nobody but their admin can repair it.
    expect(await screen.findByText(SHARED_ACTION)).toBeInTheDocument();
    expect(screen.queryByText(MODEL_ACCESS_BANNER.SHARED_ADMIN_EXPIRED)).toBeNull();
    expect(screen.queryByRole("button", { name: AGENTS.SIGN_IN_AWS })).toBeNull();
  });
});
