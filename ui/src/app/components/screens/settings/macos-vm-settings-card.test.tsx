/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Settings fields of the macOS virtual machines card: the site config's `macos_vm.max_base_age_days` and
// `macos_vm.allow_background_watch` and `macos_vm.allow_attended_window`.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expandCard } from "../../../lib/test-dom";
import { expectActionBar } from "../../../../test/action-bar";
import { FORM_ACTIONS } from "../../wardyn/copy/form-actions";
import { MAC_SETTINGS as T } from "../../wardyn/copy/mac-settings";
import { COMPONENTS_ADMIN as C } from "../../wardyn/copy/components-admin";

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
import { MacOSVMSettingsCard, macosVMBody, parseMaxBaseAge } from "./macos-vm-settings-card";

async function open() {
  render(<MacOSVMSettingsCard />);
  await expandCard(T.TITLE);
  return screen.getByTestId("macos-vm-settings-card");
}
const field = (card: HTMLElement) => within(card).getByRole("textbox", { name: T.P17 });
const saveBtn = (card: HTMLElement) => within(card).getByRole("button", { name: FORM_ACTIONS.SAVE });

beforeEach(() => {
  toastSuccess.mockClear();
  toastError.mockClear();
  snapshotMock.mockReset().mockResolvedValue({ siteConfig: { upstream_proxy_url: "http://p" }, etag: '"v1"' });
  putSiteConfigMock.mockReset().mockImplementation((cfg: object) =>
    Promise.resolve({ siteConfig: cfg, etag: '"v2"', danglingSecretRefs: [], onboardingCompletedAtIgnored: false, appliesFrom: "", sourcesNoLongerAdmitted: null }),
  );
});

describe("parseMaxBaseAge / macosVMBody", () => {
  it("empty is no maximum; only a positive whole number is a value", () => {
    expect(parseMaxBaseAge("")).toBe("");
    expect(parseMaxBaseAge("  ")).toBe("");
    expect(parseMaxBaseAge(" 30 ")).toBe(30);
    for (const bad of ["0", "-5", "1.5", "30d", "1e3", "007x", "99999999999999999999"]) expect(parseMaxBaseAge(bad)).toBeNull();
  });
  it("sets the days, or drops them, and always names the watch switch", () => {
    expect(macosVMBody(undefined, 30)).toEqual({ max_base_age_days: 30, allow_background_watch: true, allow_attended_window: false });
    expect(macosVMBody({ max_base_age_days: 30 }, "")).toEqual({ allow_background_watch: true, allow_attended_window: false });
  });
  it("writes the switch true or false, whatever is stored, and keeps each field when the other changes", () => {
    expect(macosVMBody(undefined, 30, false)).toEqual({ max_base_age_days: 30, allow_background_watch: false, allow_attended_window: false });
    expect(macosVMBody({ allow_background_watch: false }, 30)).toEqual({ max_base_age_days: 30, allow_background_watch: true, allow_attended_window: false });
    expect(macosVMBody({ max_base_age_days: 30, allow_background_watch: false }, "", false)).toEqual({ allow_background_watch: false, allow_attended_window: false });
    // The window switch is written as given, whatever is stored, and the others are kept.
    expect(macosVMBody({ allow_attended_window: true }, 30, true, false)).toEqual({ max_base_age_days: 30, allow_background_watch: true, allow_attended_window: false });
    expect(macosVMBody({ allow_attended_window: false, max_base_age_days: 7 }, 7, false, true)).toEqual({ max_base_age_days: 7, allow_background_watch: false, allow_attended_window: true });
  });
});

describe("MacOSVMSettingsCard", () => {
  it("shows the sheet's label and hint, the unit, and an empty field when no maximum is stored", async () => {
    const card = await open();
    expect(field(card)).toHaveValue("");
    expect(field(card)).toHaveAccessibleDescription(T.P18);
    expect(within(card).getAllByText(T.UNIT)).toHaveLength(2);
    expect(saveBtn(card)).toBeDisabled();
    expectActionBar(saveBtn(card));
  });

  it("shows the watch switch on when nothing is stored, and saving it off sends allow_background_watch false", async () => {
    const card = await open();
    const watch = within(card).getByRole("switch", { name: T.WATCH_LABEL });
    expect(watch).toHaveAttribute("aria-checked", "true");
    expect(within(card).getByText(T.WATCH_HINT)).toBeInTheDocument();
    expect(saveBtn(card)).toBeDisabled();
    await userEvent.click(watch);
    await userEvent.click(saveBtn(card));
    expect(putSiteConfigMock).toHaveBeenCalledWith({ upstream_proxy_url: "http://p", macos_vm: { allow_background_watch: false, allow_attended_window: false } }, '"v1"');
    expect(toastSuccess).toHaveBeenCalledWith("macOS settings saved.");
    expect(saveBtn(card)).toBeDisabled();
  });

  it("reads a stored false as off, and turning it on again drops the key with one Save beside the days", async () => {
    snapshotMock.mockResolvedValue({ siteConfig: { macos_vm: { max_base_age_days: 30, allow_background_watch: false } }, etag: '"v1"' });
    const card = await open();
    const watch = within(card).getByRole("switch", { name: T.WATCH_LABEL });
    expect(watch).toHaveAttribute("aria-checked", "false");
    await userEvent.click(watch);
    await userEvent.type(field(card), "0");
    await userEvent.click(saveBtn(card));
    expect(putSiteConfigMock).toHaveBeenCalledWith({ macos_vm: { max_base_age_days: 300, allow_background_watch: true, allow_attended_window: false } }, '"v1"');
  });

  it("loads the stored maximum", async () => {
    snapshotMock.mockResolvedValue({ siteConfig: { macos_vm: { max_base_age_days: 30 } }, etag: '"v1"' });
    const card = await open();
    expect(field(card)).toHaveValue("30");
    expect(saveBtn(card)).toBeDisabled();
  });

  it("saving spreads the document, sets macos_vm.max_base_age_days, and sends the ETag", async () => {
    const card = await open();
    await userEvent.type(field(card), "30");
    await userEvent.click(saveBtn(card));
    expect(putSiteConfigMock).toHaveBeenCalledWith({ upstream_proxy_url: "http://p", macos_vm: { max_base_age_days: 30, allow_background_watch: true, allow_attended_window: false } }, '"v1"');
    expect(toastSuccess).toHaveBeenCalledWith(T.SAVED_TOAST);
    expect(saveBtn(card)).toBeDisabled();
  });

  it("clearing the field saves an empty macos_vm block, which is no maximum", async () => {
    snapshotMock.mockResolvedValue({ siteConfig: { macos_vm: { max_base_age_days: 30 } }, etag: '"v1"' });
    const card = await open();
    await userEvent.clear(field(card));
    await userEvent.click(saveBtn(card));
    expect(putSiteConfigMock).toHaveBeenCalledWith({ macos_vm: { allow_background_watch: true, allow_attended_window: false } }, '"v1"');
    expect(field(card)).toHaveValue("");
  });

  it.each(["0", "-3", "2.5", "thirty"])("an invalid value (%s) is flagged and cannot be saved", async (bad) => {
    const card = await open();
    await userEvent.type(field(card), bad);
    expect(within(card).getByRole("alert")).toHaveTextContent(T.INVALID);
    expect(field(card)).toBeInvalid();
    expect(saveBtn(card)).toBeDisabled();
    expect(putSiteConfigMock).not.toHaveBeenCalled();
  });

  it("the server's refusal shows as sent", async () => {
    putSiteConfigMock.mockRejectedValue(new HttpError(400, "macos_vm.max_base_age_days: must be 0 or more"));
    const card = await open();
    await userEvent.type(field(card), "5");
    await userEvent.click(saveBtn(card));
    expect(await within(card).findByRole("alert")).toHaveTextContent("macos_vm.max_base_age_days: must be 0 or more");
  });

  it("a stale save reloads and says so", async () => {
    putSiteConfigMock.mockRejectedValue(new HttpError(412, "stale"));
    snapshotMock.mockResolvedValueOnce({ siteConfig: {}, etag: '"v1"' }).mockResolvedValueOnce({ siteConfig: { macos_vm: { max_base_age_days: 14 } }, etag: '"v3"' });
    const card = await open();
    await userEvent.type(field(card), "5");
    await userEvent.click(saveBtn(card));
    expect(await within(card).findByRole("status")).toHaveTextContent(C.SETTINGS_SAVED_ELSEWHERE);
    expect(field(card)).toHaveValue("14");
  });

  // UXA-7: a 412 caused by another card's save is not "someone else changed these settings".
  it("a stale save where this card's own block did not change keeps the edit and says the page changed", async () => {
    putSiteConfigMock.mockRejectedValue(new HttpError(412, "stale"));
    snapshotMock.mockResolvedValueOnce({ siteConfig: {}, etag: '"v1"' }).mockResolvedValueOnce({ siteConfig: { components: { autonomy_cap: "L1" } }, etag: '"v3"' });
    const card = await open();
    await userEvent.type(field(card), "5");
    await userEvent.click(saveBtn(card));
    expect(await within(card).findByRole("status")).toHaveTextContent(C.SETTINGS_STALE_KEPT);
    expect(within(card).queryByText(C.SETTINGS_SAVED_ELSEWHERE)).toBeNull();
    expect(field(card)).toHaveValue("5");
    expect(saveBtn(card)).toBeEnabled();
  });

  // UXA-25: the hint says who can watch, and what never can be watched.
  it("the watch switch names the person who started the run and says a controlled Mac is never watched", () => {
    expect(T.WATCH_LABEL).toBe("Allow watching background desktops");
    expect(T.WATCH_HINT).toBe(
      "On by default. The person who started a background macOS run can watch its desktop, view only, unless a governance profile denies it. A Mac that allows desktop control is never watched. Turn this off to stop all watching on this install. A watch that is open ends within a minute.",
    );
  });

  it("shows the window switch off when nothing is stored (w2-set-off), with its label and hint", async () => {
    const card = await open();
    const sw = within(card).getByRole("switch", { name: T.WINDOW_LABEL });
    expect(sw).toHaveAttribute("aria-checked", "false");
    expect(within(card).getByText(T.WINDOW_HINT)).toBeInTheDocument();
    expect(saveBtn(card)).toBeDisabled();
  });

  it("turned on is unsaved until Save (w2-set-dirty), which sends allow_attended_window true and toasts (w2-set-saved)", async () => {
    const card = await open();
    const sw = within(card).getByRole("switch", { name: T.WINDOW_LABEL });
    await userEvent.click(sw);
    expect(sw).toHaveAttribute("aria-checked", "true");
    expect(saveBtn(card)).toBeEnabled();
    await userEvent.click(saveBtn(card));
    expect(putSiteConfigMock).toHaveBeenCalledWith({ upstream_proxy_url: "http://p", macos_vm: { allow_background_watch: true, allow_attended_window: true } }, '"v1"');
    expect(toastSuccess).toHaveBeenCalledWith(T.SAVED_TOAST);
    expect(saveBtn(card)).toBeDisabled();
    expect(within(card).getByRole("switch", { name: T.WINDOW_LABEL })).toHaveAttribute("aria-checked", "true");
  });

  it("reads a stored true as on, and turning it off writes false, never an absent key", async () => {
    snapshotMock.mockResolvedValue({ siteConfig: { macos_vm: { allow_attended_window: true } }, etag: '"v1"' });
    const card = await open();
    const sw = within(card).getByRole("switch", { name: T.WINDOW_LABEL });
    expect(sw).toHaveAttribute("aria-checked", "true");
    await userEvent.click(sw);
    await userEvent.click(saveBtn(card));
    expect(putSiteConfigMock).toHaveBeenCalledWith({ macos_vm: { allow_background_watch: true, allow_attended_window: false } }, '"v1"');
  });

  it("another admin turned the window on first (w2-set-elsewhere): a 412 shows theirs", async () => {
    putSiteConfigMock.mockRejectedValue(new HttpError(412, "stale"));
    snapshotMock.mockResolvedValueOnce({ siteConfig: {}, etag: '"v1"' }).mockResolvedValueOnce({ siteConfig: { macos_vm: { allow_attended_window: true } }, etag: '"v3"' });
    const card = await open();
    await userEvent.click(within(card).getByRole("switch", { name: T.WATCH_LABEL }));
    await userEvent.click(saveBtn(card));
    expect(await within(card).findByRole("status")).toHaveTextContent(C.SETTINGS_SAVED_ELSEWHERE);
    expect(within(card).getByRole("switch", { name: T.WINDOW_LABEL })).toHaveAttribute("aria-checked", "true");
  });

  it("keyboard focus reaches the window switch (w2-set-focus), and Space turns it on", async () => {
    const card = await open();
    const sw = within(card).getByRole("switch", { name: T.WINDOW_LABEL });
    sw.focus();
    expect(sw).toHaveFocus();
    await userEvent.keyboard(" ");
    expect(sw).toHaveAttribute("aria-checked", "true");
  });

  it("the window switch's words are the board's (W2-09, W2-10)", () => {
    expect(T.WINDOW_LABEL).toBe("Allow a window on the Mac for macOS VM runs");
    expect(T.WINDOW_HINT).toBe(
      "Off by default. When on, the person who started a macOS VM run on a Mac they registered can open a window on that Mac and type in it, unless a governance profile or the Screen and actions recording level forbids it. A window is not recorded. A Mac that isn't theirs never gets one. Turn this off to stop new windows; a window that is open ends within a minute.",
    );
  });

  it("a failed load offers a retry", async () => {
    snapshotMock.mockRejectedValueOnce(new Error("down"));
    render(<MacOSVMSettingsCard />);
    await expandCard(T.TITLE);
    await userEvent.click(await screen.findByRole("button", { name: C.SETTINGS_RETRY }));
    expect(await screen.findByRole("textbox", { name: T.P17 })).toBeInTheDocument();
  });

  it("locks the field and switch while the save is pending", async () => {
    const card = await open();
    let resolve!: (v: unknown) => void;
    putSiteConfigMock.mockReturnValue(new Promise((r) => (resolve = r)));
    await userEvent.type(field(card), "5");
    await userEvent.click(saveBtn(card));
    expect(field(card)).toBeDisabled();
    for (const sw of within(card).getAllByRole("switch")) expect(sw).toBeDisabled();
    resolve({ siteConfig: { macos_vm: { max_base_age_days: 5 } }, etag: '"v2"', danglingSecretRefs: [], onboardingCompletedAtIgnored: false, appliesFrom: "", sourcesNoLongerAdmitted: null });
    await waitFor(() => expect(field(card)).toBeEnabled());
    for (const sw of within(card).getAllByRole("switch")) expect(sw).toBeEnabled();
  });
});
