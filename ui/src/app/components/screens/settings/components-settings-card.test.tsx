/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Settings card for the site config's `components` block. The wire is the
// block types.ComponentSettings reads; an all-default choice is an empty block.
import { STATES } from "../../wardyn/states";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { expandCard } from "../../../lib/test-dom";
import { expectActionBar } from "../../../../test/action-bar";

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
import { unsavedSnapshot } from "../../../lib/unsaved-registry";
import { COMPONENTS_ADMIN as C } from "../../wardyn/copy/components-admin";
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
  // #3064: a full-size filled teal Save on the right, labelled with its object.
  it("the action bar is one full-size teal Save component settings on the right", async () => {
    const card = await open();
    const save = within(card).getByRole("button", { name: "Save component settings" });
    expectActionBar(save);
    expect(save.className).not.toContain("h-8");
  });

  it("collapsed, the summary states the defaults; open, the defaults are what is selected", async () => {
    const card = await open();
    expect(screen.getByRole("button", { name: TITLE })).toHaveTextContent("Unattended runs: no restriction");
    expect(within(card).getByRole("radio", { name: /^No restriction \(default\)/ })).toBeChecked();
    expect(within(card).getByRole("switch", { name: "Allow environment and file delivery" })).toBeChecked();
    expect(within(card).getByRole("switch", { name: "Require the strongest sandbox for custom credentials" })).not.toBeChecked();
    expect(within(card).getByRole("button", { name: "Save component settings" })).toBeDisabled();
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
    await userEvent.click(within(card).getByRole("button", { name: "Save component settings" }));
    await waitFor(() => expect(putSiteConfigMock).toHaveBeenCalledTimes(1));
    expect(putSiteConfigMock).toHaveBeenCalledWith(
      { upstream_proxy_url: "http://p", components: { autonomy_cap: "L1", deny_resident_delivery: true } },
      '"v1"',
    );
    expect(toastSuccess).toHaveBeenCalledWith("Custom component settings saved.");
    await waitFor(() => expect(within(card).getByRole("button", { name: "Save component settings" })).toBeDisabled());
    expect(screen.getByRole("button", { name: TITLE })).toHaveTextContent("Unattended runs: tool calls held · Variable and file delivery off");
  });

  it("going back to every default sends an empty block", async () => {
    snapshotMock.mockResolvedValue({ siteConfig: { components: { autonomy_cap: "L1" } }, etag: '"v1"' });
    renderCard();
    await screen.findByRole("button", { name: /tool calls held/ });
    await expandCard("Custom components");
    await userEvent.click(screen.getByRole("radio", { name: /^No restriction \(default\)/ }));
    await userEvent.click(screen.getByRole("button", { name: "Save component settings" }));
    await waitFor(() => expect(putSiteConfigMock).toHaveBeenCalledWith({ components: {} }, '"v1"'));
  });

  it("a 412 shows the newer values and says someone saved first", async () => {
    const card = await open();
    putSiteConfigMock.mockRejectedValue(new HttpError(412, "stale"));
    snapshotMock.mockResolvedValue({ siteConfig: { components: { autonomy_cap: "L0" } }, etag: '"v9"' });
    await userEvent.click(within(card).getByRole("switch", { name: "Require the strongest sandbox for custom credentials" }));
    await userEvent.click(within(card).getByRole("button", { name: "Save component settings" }));
    expect(await within(card).findByRole("status")).toHaveTextContent(/Someone else changed these settings/);
    expect(within(card).getByRole("radio", { name: /^Never/ })).toBeChecked();
    expect(within(card).getByRole("switch", { name: "Require the strongest sandbox for custom credentials" })).not.toBeChecked();
  });

  it("a 400 is shown as the server sent it", async () => {
    const card = await open();
    putSiteConfigMock.mockRejectedValue(new HttpError(400, 'components.autonomy_cap: "L9" is not accepted'));
    await userEvent.click(within(card).getByRole("switch", { name: "Require the strongest sandbox for custom credentials" }));
    await userEvent.click(within(card).getByRole("button", { name: "Save component settings" }));
    expect(await within(card).findByRole("alert")).toHaveTextContent('components.autonomy_cap: "L9" is not accepted');
  });

  it("a failed read offers Retry, and says nothing about the settings", async () => {
    snapshotMock.mockRejectedValueOnce(new Error("down")).mockResolvedValue({ siteConfig: {}, etag: null });
    renderCard();
    await userEvent.click(screen.getByRole("button", { name: TITLE }));
    await userEvent.click(await screen.findByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("radio", { name: /^No restriction/ })).toBeChecked();
  });

  // UXA-74: the same compact failure block as every other Settings card, named for this card.
  it("a failed read is a compact error block naming the card, with Retry", async () => {
    snapshotMock.mockRejectedValue(new Error("down"));
    renderCard();
    await userEvent.click(screen.getByRole("button", { name: TITLE }));
    expect(await screen.findByRole("heading", { name: "Couldn't load custom components.", level: 4 })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
    expect(document.querySelector('[data-size="compact"]')).not.toBeNull();
  });

  it("a read the server worded shows its sentence, not the default line", async () => {
    snapshotMock.mockRejectedValue(new HttpError(500, "Your sentence from the server.", "site_config_failed"));
    renderCard();
    await userEvent.click(screen.getByRole("button", { name: TITLE }));
    expect(await screen.findByText("Your sentence from the server.")).toBeInTheDocument();
    expect(screen.queryByText(STATES.ERROR_DEFAULT)).toBeNull();
  });

  it("a plain network error still shows the default line", async () => {
    snapshotMock.mockRejectedValue(new Error("Failed to fetch"));
    renderCard();
    await userEvent.click(screen.getByRole("button", { name: TITLE }));
    expect(await screen.findByText(STATES.ERROR_DEFAULT)).toBeInTheDocument();
  });

  // UXA-80: the loading spinner has a name.
  it("the loading spinner is named", async () => {
    snapshotMock.mockReturnValue(new Promise(() => {}));
    renderCard();
    await userEvent.click(screen.getByRole("button", { name: TITLE }));
    expect(await screen.findByRole("status", { name: "Loading" })).toBeInTheDocument();
  });

  // UXA-37: leaving with an unsaved edit asks first, like Branding.
  it("registers an unsaved edit with the leave guard, and clears it on save", async () => {
    const card = await open();
    expect(unsavedSnapshot()).toBeNull();
    await userEvent.click(within(card).getByRole("switch", { name: "Require the strongest sandbox for custom credentials" }));
    expect(unsavedSnapshot()).toContain("Strongest sandbox required");
    await userEvent.click(within(card).getByRole("button", { name: "Save component settings" }));
    await waitFor(() => expect(unsavedSnapshot()).toBeNull());
  });

  // UXA-7: the ETag covers the whole site-config document, so a save from another card makes this one's stale.
  it("a 412 where this card's own settings did not change keeps the edit and says the page changed", async () => {
    const card = await open();
    putSiteConfigMock.mockRejectedValue(new HttpError(412, "stale"));
    // Another card saved: the fresh document has a new ETag and the same components block.
    snapshotMock.mockResolvedValue({ siteConfig: { upstream_proxy_url: "http://p", macos_vm: { max_base_age_days: 9 } }, etag: '"v3"' });
    await userEvent.click(within(card).getByRole("switch", { name: "Require the strongest sandbox for custom credentials" }));
    await userEvent.click(within(card).getByRole("button", { name: "Save component settings" }));
    expect(await within(card).findByRole("status")).toHaveTextContent(C.SETTINGS_STALE_KEPT);
    expect(within(card).queryByText(C.SETTINGS_SAVED_ELSEWHERE)).toBeNull();
    // The edit is still there and still unsaved, and saving again goes out on the new ETag.
    expect(within(card).getByRole("switch", { name: "Require the strongest sandbox for custom credentials" })).toBeChecked();
    putSiteConfigMock.mockClear();
    putSiteConfigMock.mockImplementation((cfg: object) => Promise.resolve({ siteConfig: cfg, etag: '"v4"', danglingSecretRefs: [], onboardingCompletedAtIgnored: false, appliesFrom: "", sourcesNoLongerAdmitted: null }));
    await userEvent.click(within(card).getByRole("button", { name: "Save component settings" }));
    await waitFor(() => expect(putSiteConfigMock).toHaveBeenCalledTimes(1));
    expect(putSiteConfigMock.mock.calls[0][1]).toBe('"v3"');
    expect(putSiteConfigMock.mock.calls[0][0].components).toEqual({ require_vault_for_credentials: true });
  });

  it("a 412 where this card's own settings really changed still says someone else changed them, and shows theirs", async () => {
    const card = await open();
    putSiteConfigMock.mockRejectedValue(new HttpError(412, "stale"));
    snapshotMock.mockResolvedValue({ siteConfig: { components: { autonomy_cap: "L0" } }, etag: '"v3"' });
    await userEvent.click(within(card).getByRole("switch", { name: "Require the strongest sandbox for custom credentials" }));
    await userEvent.click(within(card).getByRole("button", { name: "Save component settings" }));
    expect(await within(card).findByRole("status")).toHaveTextContent(C.SETTINGS_SAVED_ELSEWHERE);
    expect(within(card).getByRole("radio", { name: /^Never/ })).toBeChecked();
    expect(within(card).getByRole("switch", { name: "Require the strongest sandbox for custom credentials" })).not.toBeChecked();
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

  describe("inputs are locked for the whole save", () => {
    const VAULT = "Require the strongest sandbox for custom credentials";
    const ok = (cfg: object) => ({ siteConfig: cfg, etag: '"v2"', danglingSecretRefs: [], onboardingCompletedAtIgnored: false, appliesFrom: "", sourcesNoLongerAdmitted: null });
    const locked = (card: HTMLElement, on: boolean) => {
      const els = [...within(card).getAllByRole("radio"), ...within(card).getAllByRole("switch")];
      for (const el of els) {
        if (on) expect(el).toBeDisabled();
        else expect(el).toBeEnabled();
      }
    };

    it("disables radios and switches while the PUT is pending, then frees them", async () => {
      const card = await open();
      let resolve!: (v: unknown) => void;
      putSiteConfigMock.mockReturnValue(new Promise((r) => (resolve = r)));
      await userEvent.click(within(card).getByRole("switch", { name: VAULT }));
      await userEvent.click(within(card).getByRole("button", { name: "Save component settings" }));
      locked(card, true);
      resolve(ok({ components: { require_vault_for_credentials: true } }));
      await waitFor(() => locked(card, false));
      expect(within(card).getByRole("switch", { name: VAULT })).toBeChecked();
    });

    it("keeps them disabled through a 412's reload and keeps the edit after", async () => {
      const card = await open();
      let reject!: (e: unknown) => void;
      putSiteConfigMock.mockReturnValue(new Promise((_, r) => (reject = r)));
      let release!: (v: unknown) => void;
      await userEvent.click(within(card).getByRole("switch", { name: VAULT }));
      await userEvent.click(within(card).getByRole("button", { name: "Save component settings" }));
      snapshotMock.mockReturnValue(new Promise((r) => (release = r)));
      reject(new HttpError(412, "stale"));
      await waitFor(() => expect(snapshotMock).toHaveBeenCalledTimes(2));
      locked(card, true);
      release({ siteConfig: { upstream_proxy_url: "http://p" }, etag: '"v3"' });
      expect(await within(card).findByRole("status")).toHaveTextContent(C.SETTINGS_STALE_KEPT);
      locked(card, false);
      expect(within(card).getByRole("switch", { name: VAULT })).toBeChecked();
    });
  });
});
