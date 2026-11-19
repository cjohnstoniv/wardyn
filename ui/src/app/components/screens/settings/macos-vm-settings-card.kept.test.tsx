/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// KM-5: the three kept-machine site settings on the macOS virtual machines card.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expandCard } from "../../../lib/test-dom";
import { setField } from "../../../../test/set-field";
import { FORM_ACTIONS } from "../../wardyn/copy/form-actions";
import { KEEP_SETTINGS as K } from "../../wardyn/copy/keep-machine";
import { MAC_SETTINGS as T } from "../../wardyn/copy/mac-settings";

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
const snapshotMock = vi.fn();
const putSiteConfigMock = vi.fn();
vi.mock("../../../lib/api/health", () => ({
  health: { getSiteConfigSnapshot: () => snapshotMock(), putSiteConfig: (...a: unknown[]) => putSiteConfigMock(...a) },
}));

import { MacOSVMSettingsCard, macosVMBody, parseKeptIdleDays, parseKeptPerPerson } from "./macos-vm-settings-card";

async function open() {
  render(<MacOSVMSettingsCard />);
  await expandCard(T.TITLE);
  return screen.getByTestId("macos-vm-settings-card");
}
const idle = (c: HTMLElement) => within(c).getByRole("textbox", { name: K.IDLE_LABEL });
const per = (c: HTMLElement) => within(c).getByRole("textbox", { name: K.PER_LABEL });
const keptSwitch = (c: HTMLElement) => within(c).getByRole("switch", { name: K.LABEL });
const save = (c: HTMLElement) => within(c).getByRole("button", { name: FORM_ACTIONS.SAVE });

beforeEach(() => {
  snapshotMock.mockReset().mockResolvedValue({ siteConfig: { upstream_proxy_url: "http://p" }, etag: '"v1"' });
  putSiteConfigMock.mockReset().mockImplementation((cfg: object) => Promise.resolve({ siteConfig: cfg, etag: '"v2"', danglingSecretRefs: [], onboardingCompletedAtIgnored: false, appliesFrom: "", sourcesNoLongerAdmitted: null }));
});

describe("kept-machine settings", () => {
  it("KM-5 off: the switch is off with the board's hint, and the numbers read their defaults, dimmed", async () => {
    const card = await open();
    expect(keptSwitch(card)).not.toBeChecked();
    expect(within(card).getByText(K.HINT)).toBeInTheDocument();
    expect(idle(card)).toHaveValue("30");
    expect(per(card)).toHaveValue("1");
    expect(idle(card)).toHaveAccessibleDescription(K.IDLE_HINT);
    expect(per(card)).toHaveAccessibleDescription(K.PER_HINT);
    expect(idle(card).closest("div.mt-4")).toHaveClass("opacity-60");
    expect(save(card)).toBeDisabled();
  });

  it("KM-5 on: turning it on sends the three keys; the numbers lose the dimming", async () => {
    const user = userEvent.setup();
    const card = await open();
    await user.click(keptSwitch(card));
    expect(idle(card).closest("div.mt-4")).not.toHaveClass("opacity-60");
    await user.click(save(card));
    await waitFor(() => expect(putSiteConfigMock).toHaveBeenCalled());
    expect(putSiteConfigMock.mock.calls[0][0].macos_vm).toEqual({
      allow_background_watch: true, allow_attended_window: false, allow_kept_machines: true, kept_machine_idle_days: 30, max_kept_machines_per_person: 1,
    });
  });

  it("KM-5 on with values: 14 days and 2 per person are read from the stored block and saved back", async () => {
    snapshotMock.mockResolvedValue({ siteConfig: { macos_vm: { allow_kept_machines: true, kept_machine_idle_days: 14, max_kept_machines_per_person: 2 } }, etag: '"v1"' });
    const user = userEvent.setup();
    const card = await open();
    expect(keptSwitch(card)).toBeChecked();
    expect(idle(card)).toHaveValue("14");
    expect(per(card)).toHaveValue("2");
    setField(per(card), "3");
    await user.click(save(card));
    await waitFor(() => expect(putSiteConfigMock).toHaveBeenCalled());
    expect(putSiteConfigMock.mock.calls[0][0].macos_vm).toMatchObject({ allow_kept_machines: true, kept_machine_idle_days: 14, max_kept_machines_per_person: 3 });
  });

  it("an untouched kept block is carried as stored, and an untouched one is never invented", async () => {
    const user = userEvent.setup();
    const card = await open();
    await user.click(within(card).getByRole("switch", { name: T.WATCH_LABEL }));
    await user.click(save(card));
    await waitFor(() => expect(putSiteConfigMock).toHaveBeenCalled());
    expect(putSiteConfigMock.mock.calls[0][0].macos_vm).toEqual({ allow_background_watch: false, allow_attended_window: false });
  });

  it("KM-5 invalid, idle days out of range: the sentence shows and Save is held", async () => {
    const user = userEvent.setup();
    const card = await open();
    await user.click(keptSwitch(card));
    await user.clear(idle(card));
    setField(idle(card), "120");
    expect(within(card).getAllByRole("alert").map((a) => a.textContent)).toEqual([K.IDLE_INVALID]);
    expect(idle(card)).toHaveAttribute("aria-invalid", "true");
    expect(save(card)).toBeDisabled();
  });

  it("KM-5 invalid, per person out of range: the sentence shows and Save is held", async () => {
    const user = userEvent.setup();
    const card = await open();
    await user.click(keptSwitch(card));
    await user.clear(per(card));
    setField(per(card), "5");
    expect(within(card).getByRole("alert")).toHaveTextContent(K.PER_INVALID);
    expect(save(card)).toBeDisabled();
  });

  it("parses whole numbers inside the bounds only", () => {
    expect(["1", " 90 ", "30"].map(parseKeptIdleDays)).toEqual([1, 90, 30]);
    for (const bad of ["", "0", "91", "1.5", "-1", "x", "1000"]) expect(parseKeptIdleDays(bad)).toBeNull();
    expect(["1", "4"].map(parseKeptPerPerson)).toEqual([1, 4]);
    for (const bad of ["", "0", "5", "10", "2.5"]) expect(parseKeptPerPerson(bad)).toBeNull();
  });

  it("macosVMBody writes the three keys when given them, and keeps the stored ones otherwise", () => {
    expect(macosVMBody({ allow_kept_machines: true, kept_machine_idle_days: 14 }, "")).toEqual({ allow_background_watch: true, allow_attended_window: false, allow_kept_machines: true, kept_machine_idle_days: 14 });
    expect(macosVMBody(undefined, "", true, false, { allow: false, idleDays: 30, perPerson: 1 })).toMatchObject({ allow_kept_machines: false, kept_machine_idle_days: 30, max_kept_machines_per_person: 1 });
  });
});
