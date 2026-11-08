/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Runners card in Settings: the "Require a managed device" switch. Turning on is a confirm whose refusal stays
// inside the dialog; turning off is one click whose refusal shows under the switch; the switch always shows the
// stored answer, never the click.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expandCard } from "../../../lib/test-dom";
import { RUNNERS } from "../../wardyn/copy/runners";

const settingsMock = vi.fn();
const setMock = vi.fn();
vi.mock("../../../lib/api/runners", () => ({
  runners: { settings: () => settingsMock(), setRequireManagedDevice: (...a: unknown[]) => setMock(...a) },
}));

import { HttpError } from "../../../lib/api/core";
import { RunnersSettingsCard } from "./runners-settings-card";

async function open() {
  render(<RunnersSettingsCard />);
  await expandCard(RUNNERS.MANAGED_CARD_TITLE);
  return screen.getByTestId("runners-settings-card");
}
const sw = () => screen.findByRole("switch", { name: RUNNERS.MANAGED_LABEL });
const confirmDialog = () => screen.findByRole("alertdialog");

beforeEach(() => {
  settingsMock.mockReset().mockResolvedValue({ enabled: true, require_managed_device: false });
  setMock.mockReset().mockImplementation((on: boolean) => Promise.resolve({ enabled: true, require_managed_device: on }));
});

describe("RunnersSettingsCard", () => {
  it("off: the switch is unchecked, with the hint and the honest line", async () => {
    const card = await open();
    expect(await sw()).toHaveAttribute("aria-checked", "false");
    expect(within(card).getByText(RUNNERS.MANAGED_HINT)).toBeInTheDocument();
    expect(within(card).getByText(RUNNERS.MANAGED_HONEST)).toBeInTheDocument();
    expect(within(card).queryByText(RUNNERS.MANAGED_RUNNERS_OFF)).toBeNull();
  });

  it("on asks first; confirming sends true and the switch reads the answer", async () => {
    await open();
    await userEvent.click(await sw());
    const dialog = await confirmDialog();
    expect(within(dialog).getByText(RUNNERS.MANAGED_CONFIRM_TITLE)).toBeInTheDocument();
    expect(within(dialog).getByText(RUNNERS.MANAGED_CONFIRM_BODY)).toBeInTheDocument();
    expect(setMock).not.toHaveBeenCalled();
    expect(await sw()).toHaveAttribute("aria-checked", "false");
    await userEvent.click(within(dialog).getByRole("button", { name: RUNNERS.MANAGED_CONFIRM }));
    await waitFor(() => expect(setMock).toHaveBeenCalledWith(true));
    await waitFor(() => expect(screen.getByRole("switch", { name: RUNNERS.MANAGED_LABEL })).toHaveAttribute("aria-checked", "true"));
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("cancelling the confirm sends nothing and leaves it off", async () => {
    await open();
    await userEvent.click(await sw());
    await userEvent.click(within(await confirmDialog()).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(setMock).not.toHaveBeenCalled();
    expect(await sw()).toHaveAttribute("aria-checked", "false");
  });

  it("on to off sends false with no dialog", async () => {
    settingsMock.mockResolvedValue({ enabled: true, require_managed_device: true });
    await open();
    const s = await sw();
    expect(s).toHaveAttribute("aria-checked", "true");
    await userEvent.click(s);
    await waitFor(() => expect(setMock).toHaveBeenCalledWith(false));
    expect(screen.queryByRole("alertdialog")).toBeNull();
    await waitFor(() => expect(screen.getByRole("switch", { name: RUNNERS.MANAGED_LABEL })).toHaveAttribute("aria-checked", "false"));
  });

  it("a 403 on confirm is the heading and the card's own sentence, inside the dialog", async () => {
    setMock.mockRejectedValue(new HttpError(403, "forbidden"));
    await open();
    await userEvent.click(await sw());
    const dialog = await confirmDialog();
    await userEvent.click(within(dialog).getByRole("button", { name: RUNNERS.MANAGED_CONFIRM }));
    const alert = await within(dialog).findByRole("alert");
    expect(alert).toHaveTextContent(RUNNERS.MANAGED_REFUSED_TITLE);
    expect(alert).toHaveTextContent(RUNNERS.MANAGED_NOT_SUPER);
    expect(await sw()).toHaveAttribute("aria-checked", "false");
  });

  it("a 409 shows the server's sentence under the heading", async () => {
    setMock.mockRejectedValue(new HttpError(409, "settings changed under you; reload and try again"));
    await open();
    await userEvent.click(await sw());
    const dialog = await confirmDialog();
    await userEvent.click(within(dialog).getByRole("button", { name: RUNNERS.MANAGED_CONFIRM }));
    const alert = await within(dialog).findByRole("alert");
    expect(alert).toHaveTextContent(RUNNERS.MANAGED_REFUSED_TITLE);
    expect(alert).toHaveTextContent("settings changed under you; reload and try again");
    expect(alert).not.toHaveTextContent(RUNNERS.MANAGED_NOT_SUPER);
  });

  it("a failed turn-off says so under the switch, and the switch is back on", async () => {
    settingsMock.mockResolvedValue({ enabled: true, require_managed_device: true });
    setMock.mockRejectedValue(new HttpError(403, "forbidden"));
    const card = await open();
    await userEvent.click(await sw());
    const alert = await within(card).findByRole("alert");
    expect(alert).toHaveTextContent(RUNNERS.MANAGED_REFUSED_TITLE);
    expect(alert).toHaveTextContent(RUNNERS.MANAGED_NOT_SUPER);
    expect(await sw()).toHaveAttribute("aria-checked", "true");
    expect(await sw()).toBeEnabled();
  });

  it("runners off adds the quiet line, and the switch still works", async () => {
    settingsMock.mockResolvedValue({ enabled: false });
    const card = await open();
    expect(await within(card).findByText(RUNNERS.MANAGED_RUNNERS_OFF)).toBeInTheDocument();
    expect(await sw()).toBeEnabled();
  });

  it("a failed read says so, and Try again reads again", async () => {
    settingsMock.mockRejectedValueOnce(new HttpError(500, "boom"));
    const card = await open();
    expect(await within(card).findByText(RUNNERS.MANAGED_LOAD_FAILED)).toBeInTheDocument();
    await userEvent.click(within(card).getByRole("button", { name: RUNNERS.MANAGED_RETRY }));
    expect(await sw()).toHaveAttribute("aria-checked", "false");
    expect(settingsMock).toHaveBeenCalledTimes(2);
  });

  it("a stored value with no require_managed_device reads as off", async () => {
    settingsMock.mockResolvedValue({ enabled: true });
    await open();
    expect(await sw()).toHaveAttribute("aria-checked", "false");
  });
});
