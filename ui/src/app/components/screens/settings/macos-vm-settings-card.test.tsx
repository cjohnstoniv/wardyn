/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Settings field for the oldest macOS base: the site config's `macos_vm.max_base_age_days`.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expandCard } from "../../../lib/test-dom";
import { expectActionBar } from "../../../../test/action-bar";
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
const saveBtn = (card: HTMLElement) => within(card).getByRole("button", { name: T.SAVE });

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
  it("sets the one field, or leaves an empty block that still names macos_vm", () => {
    expect(macosVMBody(undefined, 30)).toEqual({ max_base_age_days: 30 });
    expect(macosVMBody({ max_base_age_days: 30 }, "")).toEqual({});
  });
});

describe("MacOSVMSettingsCard", () => {
  it("shows the sheet's label and hint, the unit, and an empty field when no maximum is stored", async () => {
    const card = await open();
    expect(field(card)).toHaveValue("");
    expect(field(card)).toHaveAccessibleDescription(T.P18);
    expect(within(card).getByText(T.UNIT)).toBeInTheDocument();
    expect(saveBtn(card)).toBeDisabled();
    expectActionBar(saveBtn(card));
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
    expect(putSiteConfigMock).toHaveBeenCalledWith({ upstream_proxy_url: "http://p", macos_vm: { max_base_age_days: 30 } }, '"v1"');
    expect(toastSuccess).toHaveBeenCalledWith(T.SAVED_TOAST);
    expect(saveBtn(card)).toBeDisabled();
  });

  it("clearing the field saves an empty macos_vm block, which is no maximum", async () => {
    snapshotMock.mockResolvedValue({ siteConfig: { macos_vm: { max_base_age_days: 30 } }, etag: '"v1"' });
    const card = await open();
    await userEvent.clear(field(card));
    await userEvent.click(saveBtn(card));
    expect(putSiteConfigMock).toHaveBeenCalledWith({ macos_vm: {} }, '"v1"');
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

  it("a failed load offers a retry", async () => {
    snapshotMock.mockRejectedValueOnce(new Error("down"));
    render(<MacOSVMSettingsCard />);
    await expandCard(T.TITLE);
    await userEvent.click(await screen.findByRole("button", { name: C.SETTINGS_RETRY }));
    expect(await screen.findByRole("textbox", { name: T.P17 })).toBeInTheDocument();
  });
});
