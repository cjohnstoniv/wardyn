/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { SetupStatus } from "../../../lib/types";
import { ADO } from "../../../lib/ado-entra-copy";

const adoConnectMock = vi.fn();
let adoBlockedUrl: string | null = null;
vi.mock("../../../lib/hooks/use-ado-connect", () => ({
  useAdoConnect: () => ({ connecting: false, connect: adoConnectMock, connectFallback: adoConnectMock, blockedUrl: adoBlockedUrl }),
}));

import { AdoConnectionCard } from "./ado-connection";

function status(scm_access?: SetupStatus["scm_access"]): SetupStatus {
  return { ready: true, checks: [], auth: { mode: "local" }, runner: { driver: "docker", confinement_classes: [] }, providers: [], secrets: { present: [] }, age_key: { durable: true }, has_runs: false, scm_access } as unknown as SetupStatus;
}

// #458: the card reads location.hash (the hash-focus effect), so every
// render needs a Router in scope. initialEntries defaults to a plain
// /settings landing — no hash, no focus.
function renderCard(ui: React.ReactElement, initialEntries: string[] = ["/settings"]) {
  return render(<MemoryRouter initialEntries={initialEntries}>{ui}</MemoryRouter>);
}

describe("AdoConnectionCard — the connected panel's Settings home (#386, Q9)", () => {
  beforeEach(() => {
    adoConnectMock.mockReset();
    adoBlockedUrl = null;
  });

  it("renders nothing with no Azure DevOps row configured", () => {
    renderCard(<AdoConnectionCard status={status(undefined)} onChanged={vi.fn()} />);
    expect(screen.queryByText("Azure DevOps")).not.toBeInTheDocument();
  });

  it("live: the org, how it connected, and the renewal note — no button", () => {
    renderCard(
      <AdoConnectionCard
        status={status({ state: "live", source: "org", org: "https://dev.azure.com/contoso" })}
        onChanged={vi.fn()}
      />,
    );
    expect(screen.getByText(ADO.ACCESS_LIVE_ORG)).toBeInTheDocument();
    expect(screen.getByText("https://dev.azure.com/contoso")).toBeInTheDocument();
    expect(screen.getByText(new RegExp(ADO.PANEL_HOW_ORG))).toBeInTheDocument();
    expect(screen.getByText(new RegExp(ADO.PANEL_ENDS_RENEWED))).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: ADO.CONNECT_ADO })).not.toBeInTheDocument();
  });

  // Review finding F3: a SHARED row's `live` (no source) must render
  // ACCESS_SHARED_NOTE, never a per-person claim like "Your Wardyn sign-in"
  // or "Renewed while you keep using it".
  it("live on a shared row (no source): ACCESS_SHARED_NOTE, never the per-person panel", () => {
    renderCard(<AdoConnectionCard status={status({ state: "live", org: "https://dev.azure.com/contoso" })} onChanged={vi.fn()} />);
    expect(screen.getByText(ADO.ACCESS_SHARED_LIVE)).toBeInTheDocument();
    expect(screen.getByText(ADO.ACCESS_SHARED_NOTE)).toBeInTheDocument();
    expect(screen.queryByText(new RegExp(ADO.PANEL_HOW_ORG))).not.toBeInTheDocument();
    expect(screen.queryByText(new RegExp(ADO.PANEL_ENDS_RENEWED))).not.toBeInTheDocument();
  });

  it("not_configured: the cause and the connect button, which reloads status on a real connection", async () => {
    adoConnectMock.mockResolvedValueOnce(true);
    const onChanged = vi.fn();
    renderCard(<AdoConnectionCard status={status({ state: "not_configured", cause: "row_is_newer" })} onChanged={onChanged} />);
    expect(screen.getByText(ADO.CAUSE_ROW_IS_NEWER)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: ADO.CONNECT_ADO }));
    expect(adoConnectMock).toHaveBeenCalledTimes(1);
    expect(onChanged).toHaveBeenCalledTimes(1);
  });

  it("shared_expired: the admin-facing action line, no button", () => {
    renderCard(<AdoConnectionCard status={status({ state: "shared_expired" })} onChanged={vi.fn()} />);
    expect(screen.getByText(ADO.ACCESS_SHARED_EXPIRED_ACTION)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: ADO.CONNECT_ADO })).not.toBeInTheDocument();
  });

  // Issue #458 — Go grades an admin-token/local-mode caller not_applicable
  // (scmaccess.go's `isMechanism := subject == ""`); the card used to render
  // just its title over an empty body for that state. Q458-1: one line, no
  // chip, no button.
  describe("not_applicable — an admin-token/local-mode caller (#458)", () => {
    it("renders the title and the one canon line, no chip and no button", () => {
      renderCard(<AdoConnectionCard status={status({ state: "not_applicable" })} onChanged={vi.fn()} />);
      expect(screen.getByText("Azure DevOps")).toBeInTheDocument();
      expect(screen.getByText(ADO.NOT_APPLICABLE_BODY)).toBeInTheDocument();
      expect(screen.queryByRole("button")).not.toBeInTheDocument();
      expect(screen.queryByText(ADO.ACCESS_NOT_CONNECTED)).not.toBeInTheDocument();
    });
  });

  // #458 — the capability card's consent CTA and the mid-run sign-in door
  // both land on /settings#azure-devops; arriving with that hash must move
  // focus onto this card (no other anchor exists on a five-card page).
  describe("arrival with #azure-devops focuses the card (#458)", () => {
    it("focuses the section when the URL hash matches and a row is configured", () => {
      renderCard(
        <AdoConnectionCard status={status({ state: "not_configured", cause: "row_is_newer" })} onChanged={vi.fn()} />,
        ["/settings#azure-devops"],
      );
      const section = screen.getByText("Azure DevOps").closest("section")!;
      expect(section).toHaveAttribute("id", "azure-devops");
      expect(section).toHaveAttribute("tabindex", "-1");
      expect(document.activeElement).toBe(section);
    });

    it("does not steal focus on a plain /settings landing (no hash)", () => {
      renderCard(<AdoConnectionCard status={status({ state: "not_configured", cause: "row_is_newer" })} onChanged={vi.fn()} />);
      const section = screen.getByText("Azure DevOps").closest("section")!;
      expect(document.activeElement).not.toBe(section);
    });

    // PR #501 review F3 — a `.focus()` that follows a click (the consent
    // door's own click, on the page this card navigates FROM) can lose the
    // browser's focus-visible heuristic; force it instead of hoping.
    it("F3: focus is forced visible (focusVisible: true), not left to the browser's own heuristic", () => {
      const focusSpy = vi.spyOn(HTMLElement.prototype, "focus");
      try {
        renderCard(
          <AdoConnectionCard status={status({ state: "not_configured", cause: "row_is_newer" })} onChanged={vi.fn()} />,
          ["/settings#azure-devops"],
        );
        expect(focusSpy).toHaveBeenCalledWith({ focusVisible: true });
      } finally {
        focusSpy.mockRestore();
      }
    });

    // PR #501 review F2 — the effect used to depend on the `access` OBJECT,
    // which is a new reference after every Settings reload (Re-check, a
    // secret save, a disconnect elsewhere on the page): it reran and yanked
    // focus back here even though the reader had since focused something
    // else and the URL never changed. It must depend on navigation
    // (location.key) + row presence (a boolean), not on data identity.
    it("F2: an equal-but-new status from a Settings reload does not steal focus back from elsewhere on the page", () => {
      function Harness({ s }: { s: SetupStatus }) {
        return (
          <div>
            <input data-testid="other-input" />
            <AdoConnectionCard status={s} onChanged={vi.fn()} />
          </div>
        );
      }
      const initial = status({ state: "not_configured", cause: "row_is_newer" });
      const { rerender } = render(
        <MemoryRouter initialEntries={["/settings#azure-devops"]}>
          <Harness s={initial} />
        </MemoryRouter>,
      );
      const section = screen.getByText("Azure DevOps").closest("section")!;
      expect(document.activeElement).toBe(section); // landed here first, as above

      const input = screen.getByTestId("other-input");
      input.focus();
      expect(document.activeElement).toBe(input);

      // A NEW object, same values, same route — exactly what a Settings
      // reload's own load() hands this card. MemoryRouter is the SAME
      // instance across this rerender (React matches it by type/position),
      // so no navigation happens and location.key is unchanged.
      const reloaded = status({ state: "not_configured", cause: "row_is_newer" });
      rerender(
        <MemoryRouter initialEntries={["/settings#azure-devops"]}>
          <Harness s={reloaded} />
        </MemoryRouter>,
      );
      expect(document.activeElement).toBe(input);
    });
  });
});

// Review finding F9 — a blocked popup.
describe("AdoConnectionCard — a blocked popup (F9)", () => {
  beforeEach(() => {
    adoConnectMock.mockReset();
    adoBlockedUrl = "/api/v1/scm/azure-devops/signin";
  });

  it("shows the canon sentence and a plain fallback link to the sign-in URL, alongside the button", () => {
    renderCard(<AdoConnectionCard status={status({ state: "not_configured", cause: "row_is_newer" })} onChanged={vi.fn()} />);
    expect(screen.getByText(ADO.CONNECT_POPUP_BLOCKED)).toBeInTheDocument();
    const link = screen.getByRole("link", { name: ADO.CONNECT_POPUP_OPEN });
    expect(link).toHaveAttribute("href", "/api/v1/scm/azure-devops/signin");
    expect(link).toHaveAttribute("target", "_blank");
  });

  // Review follow-up N1: clicking the fallback link starts the SAME poll
  // (connectFallback), so the card reloads status on a real connection.
  it("N1: clicking the fallback link starts the poll and reloads status once connected", async () => {
    adoConnectMock.mockResolvedValueOnce(true);
    const onChanged = vi.fn();
    renderCard(<AdoConnectionCard status={status({ state: "not_configured", cause: "row_is_newer" })} onChanged={onChanged} />);
    await userEvent.click(screen.getByRole("link", { name: ADO.CONNECT_POPUP_OPEN }));
    expect(adoConnectMock).toHaveBeenCalledTimes(1);
    expect(onChanged).toHaveBeenCalledTimes(1);
  });
});
