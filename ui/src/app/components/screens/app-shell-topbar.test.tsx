/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Split from app-shell.test.tsx (#195): that file was over the 800-line test
// gate. The sidebar/mobile-nav describes stay there; the TopBar-focused
// describes and the drive-bits describe that only need renderMobileNav for a
// supporting assertion live here.
import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { AppShell, MobileNav } from "./app-shell";
import { TopBar } from "./top-bar";
import { useUserDrive, type Role } from "../wardyn/operator-context";
import { ThemeProvider } from "../wardyn/theme-provider";
import { baseMeDrive } from "../../lib/test-fixtures";
import { registerUnsaved } from "../../lib/unsaved-registry";
import { UnsavedGuardProvider } from "../../lib/use-unsaved-guard";
import { UNSAVED } from "../../lib/unsaved-copy";

// below md the desktop aside is hidden, so this Sheet-based hamburger is
// the ONLY navigation. These pins fail if the drawer stops opening, drops nav
// items, or loses its aria-expanded/Escape wiring.
// role is the full three-valued union since 0.7. operator/securityOperator
// mirror the server's two predicates exactly: "admin" is both, "security_admin"
// is only the second, "user" is neither.
function renderMobileNav(role: Role = "admin", memberMode = false) {
  return render(
    <MemoryRouter>
      <UnsavedGuardProvider>
        <MobileNav
          pendingApprovals={2}
          attentionCount={0}
          meta={{
            trustDomain: "example.test",
            identityProvider: "spiffe",
            principal: "u@example.test",
            email: "",
            name: "",
            method: "sso",
            resolved: true,
            identityResolved: true,
            operator: role === "admin",
            securityOperator: role !== "user",
            role,
            sessionExpiresAt: null,
            memberLocalDirRoot: null,
            userDrive: null,
            userDriveDeniedByProfile: "",
            userDriveUnavailable: "",
            memberMode,
            memberModeNoCredential: false,
            memberPreviewAvailable: false,
            runner: "",
            networkPolicy: "",
            sso: false,
          }}
        />
      </UnsavedGuardProvider>
    </MemoryRouter>,
  );
}

// TopBar, driven directly (the sidebar/mobile-nav describes live in the
// sibling app-shell.test.tsx).
// #460 review — wrapped in UnsavedGuardProvider: a no-op while nothing's
// dirty (every existing assertion below is unaffected), and what the
// account-menu guard test just past this function needs to see the real
// confirm dialog instead of the context's no-provider fallback.
function renderTopBar(role: Role) {
  return render(
    <MemoryRouter>
      <UnsavedGuardProvider>
        <ThemeProvider>
          <TopBar
            onSignOut={() => {}}
            meta={{
              trustDomain: "example.test",
              identityProvider: "spiffe",
              principal: "u@example.test",
              email: "",
              name: "",
              method: "sso",
              resolved: true,
              identityResolved: true,
              operator: role === "admin",
              securityOperator: role !== "user",
              role,
              sessionExpiresAt: null,
              memberLocalDirRoot: null,
              userDrive: null,
              userDriveDeniedByProfile: "",
              userDriveUnavailable: "",
              memberMode: false,
              memberModeNoCredential: false,
              memberPreviewAvailable: false,
              runner: "",
              networkPolicy: "",
              sso: false,
            }}
            pendingApprovals={0}
            attentionCount={0}
            onNewRun={() => {}}
          />
        </ThemeProvider>
      </UnsavedGuardProvider>
    </MemoryRouter>,
  );
}

// #460 review — every plain <Link> in the header (top-bar.tsx) and the rail
// goes through the one guardedClick; Settings is the entry the review named
// explicitly, and since M-2 it lives in the rail's lower slot, not the menu.
describe("Header and rail links are guarded (#460 review)", () => {
  it("Settings, dirty: opens the confirm, and Keep editing stays", async () => {
    const user = userEvent.setup();
    const unregister = registerUnsaved("dirty-test-editor-stay", () => "unsaved text");
    try {
      renderMobileNav("admin");
      await user.click(screen.getByRole("button", { name: /open navigation menu/i }));
      await user.click(screen.getByRole("link", { name: /^Settings/ }));

      const dialog = await screen.findByRole("alertdialog");
      expect(within(dialog).getByText(UNSAVED.TITLE)).toBeInTheDocument();

      await user.click(within(dialog).getByRole("button", { name: UNSAVED.STAY }));
      expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
    } finally {
      unregister();
    }
  });

  it("Settings, dirty: Discard proceeds", async () => {
    const user = userEvent.setup();
    const unregister = registerUnsaved("dirty-test-editor-discard", () => "unsaved text");
    try {
      renderMobileNav("admin");
      await user.click(screen.getByRole("button", { name: /open navigation menu/i }));
      await user.click(screen.getByRole("link", { name: /^Settings/ }));
      await user.click(await screen.findByRole("button", { name: UNSAVED.DISCARD }));
      expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
    } finally {
      unregister();
    }
  });

  it("a clean session's Settings click opens no dialog", async () => {
    const user = userEvent.setup();
    renderMobileNav("admin");
    await user.click(screen.getByRole("button", { name: /open navigation menu/i }));
    await user.click(screen.getByRole("link", { name: /^Settings/ }));
    expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
  });

  // #460 review (M9b) — every plain <Link> here is guarded, not only
  // Settings: the wordmark itself links to the view's home and is the FIRST
  // control in the header, reached before the account menu on every screen.
  it("the wordmark/logo link (-> the view's home) is guarded too", async () => {
    const user = userEvent.setup();
    const unregister = registerUnsaved("dirty-logo-test", () => "unsaved text");
    try {
      renderTopBar("admin");
      await user.click(screen.getByRole("link", { name: /Wardyn/i }));
      const dialog = await screen.findByRole("alertdialog");
      expect(within(dialog).getByText(UNSAVED.TITLE)).toBeInTheDocument();
    } finally {
      unregister();
    }
  });
});

// 0.7.3 F6: the Fence/NetworkPolicy chips are gone outright — no degraded
// chip, no replacement. Both were deployment-wide facts fixed at boot that
// never changed while the console was open; posture now lives on the setup
// Environment step alone (environment-step.tsx's k8sEgressRow/k8sClassesRow).
describe("TopBar — the header states no posture", () => {
  // ticket: 0.7.3 F6
  it("carries no NetworkPolicy or barrier chip", () => {
    renderTopBar("admin");
    const header = screen.getByRole("banner");
    expect(within(header).queryByText(/NetworkPolicy:/)).not.toBeInTheDocument();
    expect(
      within(header).queryByText(/^(Fence|Wall|Vault|No barrier)$/),
    ).not.toBeInTheDocument();
  });
});

// M-2 (packet M-A): the avatar menu is identity and Sign out only; the view is
// the switch beside the wordmark, and New run is the User view's alone.
describe("TopBar — the switch, New run and the slimmed avatar menu (M-2)", () => {
  it.each(["admin", "security_admin", "user"] as Role[])("%s: the menu holds Sign out and nothing else", async (role) => {
    const user = userEvent.setup();
    renderTopBar(role);
    await user.click(screen.getAllByRole("button").at(-1)!);
    const items = within(screen.getByRole("menu")).getAllByRole("menuitem");
    expect(items.map((i) => i.textContent?.trim())).toEqual(["Sign out"]);
  });

  it("an SSO admin in the Admin view: the switch, Admin view pressed, no New run", () => {
    renderTopBar("admin");
    const group = screen.getByRole("group", { name: "Console view" });
    expect(within(group).getByRole("button", { name: "Admin view" })).toHaveAttribute("aria-pressed", "true");
    expect(within(group).getByRole("button", { name: "Admin view" })).toHaveAttribute("aria-disabled", "true");
    expect(within(group).getByRole("button", { name: "User view" })).toHaveAttribute("aria-pressed", "false");
    expect(screen.queryByRole("button", { name: "New run" })).toBeNull();
  });

  it("a user: no switch, and New run", () => {
    renderTopBar("user");
    expect(screen.queryByRole("group", { name: "Console view" })).toBeNull();
    expect(screen.getByRole("button", { name: "New run" })).toBeInTheDocument();
  });

  it("the role chip calls the non-admin side a user", async () => {
    const user = userEvent.setup();
    renderTopBar("user");
    await user.click(screen.getAllByRole("button").at(-1)!);
    expect(within(screen.getByRole("menu")).getByText("user", { exact: true })).toBeInTheDocument();
  });
});

// 0.7 — the shell's ONE GET /me is what fills MeIdentity.userDrive: New Run and the
// member Getting Started page read the caller's drive off the context and issue
// no fetch of their own. That seam had only e2e coverage, and a component test
// cannot see it: workspace-card.test.tsx renders the block perfectly from props
// the shell might never actually hand it. Pinned here, at the one place the
// wire body becomes the context value.
describe("AppShell — /me's drive bits reach useUserDrive()", () => {
  afterEach(() => vi.unstubAllGlobals());

  function DriveProbe() {
    const { drive, deniedByProfile, unavailable } = useUserDrive();
    return (
      <>
        <span data-testid="drive-probe">
          {JSON.stringify({ drive, deniedByProfile })}
        </span>
        {/* R4/F091 — read as its OWN element rather than folded into the JSON
            above, so the three cases that predate the third key keep asserting
            the exact string they always did. */}
        <span data-testid="drive-unavailable-probe">{`[${unavailable}]`}</span>
      </>
    );
  }

  function renderShellWithMe(me: Record<string, unknown>) {
    vi.stubGlobal(
      "fetch",
      vi.fn((url: RequestInfo | URL) => {
        const u = String(url);
        if (u.endsWith("/healthz")) {
          return Promise.resolve({
            ok: true,
            json: async () => ({
              trust_domain: "wardyn.local",
              identity_provider: "embedded",
            }),
          });
        }
        if (u.endsWith("/api/v1/me"))
          return Promise.resolve({ ok: true, json: async () => me });
        return Promise.resolve({ ok: true, json: async () => ({}) });
      }) as unknown as typeof fetch,
    );
    return render(
      <MemoryRouter>
        <ThemeProvider>
          <Routes>
            <Route
              element={
                <AppShell
                  pendingApprovals={0}
                  attentionCount={0}
                  onSignOut={() => {}}
                />
              }
            >
              <Route index element={<DriveProbe />} />
            </Route>
          </Routes>
        </ThemeProvider>
      </MemoryRouter>,
    );
  }

  it("hands the allocation down verbatim once /me resolves", async () => {
    const drive = baseMeDrive();
    renderShellWithMe({
      principal: "alice@corp.example",
      method: "sso",
      operator: false,
      role: "user",
      user_drive: drive,
      user_drive_denied_by_profile: "",
    });

    // Negative control: the fail-CLOSED seed is what paints first, so a green
    // assertion below cannot be a constant.
    expect(screen.getByTestId("drive-probe")).toHaveTextContent(
      JSON.stringify({ drive: null, deniedByProfile: "" }),
    );
    await waitFor(() =>
      expect(screen.getByTestId("drive-probe")).toHaveTextContent(
        JSON.stringify({ drive, deniedByProfile: "" }),
      ),
    );
  });

  it("carries the door as a SIBLING of the allocation, not a property of it", async () => {
    renderShellWithMe({
      principal: "alice@corp.example",
      method: "sso",
      operator: false,
      role: "user",
      user_drive: null,
      user_drive_denied_by_profile: "Greenfield contractors",
    });

    await waitFor(() =>
      expect(screen.getByTestId("drive-probe")).toHaveTextContent(
        JSON.stringify({
          drive: null,
          deniedByProfile: "Greenfield contractors",
        }),
      ),
    );
  });

  it("stays at no-allocation-and-no-door for a pre-0.7 daemon that sends neither field", async () => {
    renderShellWithMe({
      principal: "alice@corp.example",
      method: "sso",
      operator: false,
      role: "user",
    });

    // Let /me land before reading the probe, so this is the RESOLVED value and
    // not the seed it happens to equal.
    await screen.findByText("alice");
    expect(screen.getByTestId("drive-probe")).toHaveTextContent(
      JSON.stringify({ drive: null, deniedByProfile: "" }),
    );
    // …and the third key reads as "nothing is wrong", not as a reason invented
    // out of an absent field.
    expect(screen.getByTestId("drive-unavailable-probe")).toHaveTextContent(
      "[]",
    );
  });

  // R4/F091 — the THIRD key. /me suppresses the allocation for all four of
  // these (me.go:119-123), so `drive: null` is the SAME answer for every one of
  // them and the reason is the only thing that tells them apart — the context
  // must read and carry this key, or every consumer sees only "you have no
  // allocation", the one remedy that is wrong in all four cases.
  // One case per token in the server's closed vocabulary
  // (user_drives_resolve.go:87-99): a token this test does not carry is a
  // token the console would silently drop.
  it.each([
    ["groups_snapshot_stale"],
    ["unmountable"],
    ["unavailable"],
    ["governance_unavailable"],
  ])(
    "carries %s through to useUserDrive() beside the suppressed allocation",
    async (reason) => {
      renderShellWithMe({
        principal: "alice@corp.example",
        method: "sso",
        operator: false,
        role: "user",
        user_drive: null,
        user_drive_denied_by_profile: "",
        user_drive_unavailable: reason,
      });

      // Negative control: the fail-closed seed paints first, so the assertion
      // below cannot be a constant.
      expect(screen.getByTestId("drive-unavailable-probe")).toHaveTextContent(
        "[]",
      );
      await waitFor(() =>
        expect(screen.getByTestId("drive-unavailable-probe")).toHaveTextContent(
          `[${reason}]`,
        ),
      );
      // The allocation stays null — the reason REPLACES the offer, it does not
      // ride beside one.
      expect(screen.getByTestId("drive-probe")).toHaveTextContent(
        JSON.stringify({ drive: null, deniedByProfile: "" }),
      );
    },
  );

  it("keeps the reason and the door as SEPARATE bits — a denied door is not an outage", async () => {
    renderShellWithMe({
      principal: "alice@corp.example",
      method: "sso",
      operator: false,
      role: "user",
      user_drive: null,
      user_drive_denied_by_profile: "Greenfield contractors",
      user_drive_unavailable: "",
    });

    await waitFor(() =>
      expect(screen.getByTestId("drive-probe")).toHaveTextContent(
        JSON.stringify({
          drive: null,
          deniedByProfile: "Greenfield contractors",
        }),
      ),
    );
    expect(screen.getByTestId("drive-unavailable-probe")).toHaveTextContent(
      "[]",
    );
  });
});
