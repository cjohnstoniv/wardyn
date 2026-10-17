/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Settings card for the site config's `components` block. The wire is the
// block types.ComponentSettings reads; an all-default choice is an empty block.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { expandCard } from "../../../lib/test-dom";

const toastSuccess = vi.fn();
const toastError = vi.fn();
vi.mock("sonner", () => ({ toast: { success: (...a: unknown[]) => toastSuccess(...a), error: (...a: unknown[]) => toastError(...a) } }));

const snapshotMock = vi.fn();
const putSiteConfigMock = vi.fn();
vi.mock("../../../lib/api/health", () => ({
  health: {
    getSiteConfigSnapshot: () => snapshotMock(),
    putSiteConfig: (...a: unknown[]) => putSiteConfigMock(...a),
  },
}));

import { HttpError } from "../../../lib/api/core";
import { ComponentsSettingsCard, settingsBody } from "./components-settings-card";

const TITLE = /^Custom components/;

function renderCard() {
  return render(
    <MemoryRouter initialEntries={["/admin/settings"]}>
      <Routes>
        <Route path="/admin/settings" element={<ComponentsSettingsCard />} />
        <Route path="/admin/permissions" element={<p>permissions page</p>} />
        <Route path="/admin/components" element={<p>components page</p>} />
      </Routes>
    </MemoryRouter>,
  );
}

async function open() {
  renderCard();
  await screen.findByRole("button", { name: /Unattended runs: no restriction|Unattended runs: / });
  await expandCard("Custom components");
  return screen.getByTestId("components-settings-card");
}

beforeEach(() => {
  toastSuccess.mockClear();
  toastError.mockClear();
  snapshotMock.mockReset().mockResolvedValue({ siteConfig: { upstream_proxy_url: "http://p" }, etag: '"v1"' });
  putSiteConfigMock.mockReset().mockImplementation((cfg: { components?: object }) =>
    Promise.resolve({ siteConfig: cfg, etag: '"v2"', danglingSecretRefs: [], onboardingCompletedAtIgnored: false, appliesFrom: "", sourcesNoLongerAdmitted: null }),
  );
});

describe("settingsBody", () => {
  it("sends only what differs from the default", () => {
    expect(settingsBody({ cap: "", residentAllowed: true, vault: false })).toEqual({});
    expect(settingsBody({ cap: "L1", residentAllowed: false, vault: true })).toEqual({
      autonomy_cap: "L1",
      deny_resident_delivery: true,
      require_vault_for_credentials: true,
    });
  });
});

describe("ComponentsSettingsCard", () => {
  it("collapsed, the summary states the defaults; open, the defaults are what is selected", async () => {
    const card = await open();
    expect(screen.getByRole("button", { name: TITLE })).toHaveTextContent("Unattended runs: no restriction");
    expect(within(card).getByRole("radio", { name: /^No restriction \(default\)/ })).toBeChecked();
    expect(within(card).getByRole("switch", { name: "Allow environment and file delivery" })).toBeChecked();
    expect(within(card).getByRole("switch", { name: "Require the strongest sandbox for custom credentials" })).not.toBeChecked();
    expect(within(card).getByRole("button", { name: "Save" })).toBeDisabled();
    expect(within(card).getByText("Who may define their own components")).toBeInTheDocument();
  });

  it("reads the stored block", async () => {
    snapshotMock.mockResolvedValue({
      siteConfig: { components: { autonomy_cap: "L0", deny_resident_delivery: true, require_vault_for_credentials: true } },
      etag: '"v1"',
    });
    renderCard();
    const summary = await screen.findByRole("button", { name: TITLE });
    await waitFor(() =>
      expect(summary).toHaveTextContent("Unattended runs: never · Variable and file delivery off · Strongest sandbox required"),
    );
    await expandCard("Custom components");
    expect(screen.getByRole("radio", { name: /^Never/ })).toBeChecked();
    expect(screen.getByRole("switch", { name: "Allow environment and file delivery" })).not.toBeChecked();
    expect(screen.getByRole("switch", { name: "Require the strongest sandbox for custom credentials" })).toBeChecked();
  });

  it("saving spreads the document, replaces components, and sends the ETag", async () => {
    const card = await open();
    await userEvent.click(within(card).getByRole("radio", { name: /^Hold tool calls/ }));
    await userEvent.click(within(card).getByRole("switch", { name: "Allow environment and file delivery" }));
    await userEvent.click(within(card).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(putSiteConfigMock).toHaveBeenCalledTimes(1));
    expect(putSiteConfigMock).toHaveBeenCalledWith(
      { upstream_proxy_url: "http://p", components: { autonomy_cap: "L1", deny_resident_delivery: true } },
      '"v1"',
    );
    expect(toastSuccess).toHaveBeenCalledWith("Custom component settings saved.");
    await waitFor(() => expect(within(card).getByRole("button", { name: "Save" })).toBeDisabled());
    expect(screen.getByRole("button", { name: TITLE })).toHaveTextContent("Unattended runs: tool calls held · Variable and file delivery off");
  });

  it("going back to every default sends an empty block", async () => {
    snapshotMock.mockResolvedValue({ siteConfig: { components: { autonomy_cap: "L1" } }, etag: '"v1"' });
    renderCard();
    await screen.findByRole("button", { name: /tool calls held/ });
    await expandCard("Custom components");
    await userEvent.click(screen.getByRole("radio", { name: /^No restriction \(default\)/ }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(putSiteConfigMock).toHaveBeenCalledWith({ components: {} }, '"v1"'));
  });

  it("a 412 shows the newer values and says someone saved first", async () => {
    const card = await open();
    putSiteConfigMock.mockRejectedValue(new HttpError(412, "stale"));
    snapshotMock.mockResolvedValue({ siteConfig: { components: { autonomy_cap: "L0" } }, etag: '"v9"' });
    await userEvent.click(within(card).getByRole("switch", { name: "Require the strongest sandbox for custom credentials" }));
    await userEvent.click(within(card).getByRole("button", { name: "Save" }));
    expect(await within(card).findByRole("status")).toHaveTextContent(/Someone else changed these settings/);
    expect(within(card).getByRole("radio", { name: /^Never/ })).toBeChecked();
    expect(within(card).getByRole("switch", { name: "Require the strongest sandbox for custom credentials" })).not.toBeChecked();
  });

  it("a 400 is shown as the server sent it", async () => {
    const card = await open();
    putSiteConfigMock.mockRejectedValue(new HttpError(400, 'components.autonomy_cap: "L9" is not accepted'));
    await userEvent.click(within(card).getByRole("switch", { name: "Require the strongest sandbox for custom credentials" }));
    await userEvent.click(within(card).getByRole("button", { name: "Save" }));
    expect(await within(card).findByRole("alert")).toHaveTextContent('components.autonomy_cap: "L9" is not accepted');
  });

  it("a failed read offers Retry, and says nothing about the settings", async () => {
    snapshotMock.mockRejectedValueOnce(new Error("down")).mockResolvedValue({ siteConfig: {}, etag: null });
    renderCard();
    await userEvent.click(screen.getByRole("button", { name: TITLE }));
    await userEvent.click(await screen.findByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("radio", { name: /^No restriction/ })).toBeChecked();
  });

  it("who may define their own is a link to Permissions, not a second switch", async () => {
    const card = await open();
    await userEvent.click(within(card).getByRole("button", { name: "Open Permissions" }));
    expect(await screen.findByText("permissions page")).toBeInTheDocument();
  });

  it("the org's components are a link to the Components page", async () => {
    const card = await open();
    await userEvent.click(within(card).getByRole("button", { name: "Open Components" }));
    expect(await screen.findByText("components page")).toBeInTheDocument();
  });
});
