/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// KM-4: Your settings, Your kept machines. Each test names the board frame it draws.
import { STATES } from "../../wardyn/states";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { HttpError } from "../../../lib/api/core";
import { mediumDate, shortDate } from "../../../lib/format";
import { aheadByHours } from "../../../lib/test-clock";
import { expandCard } from "../../../lib/test-dom";
import type { KeptMachine, MyMachines } from "../../../lib/types";
import { KEEP_LIST as T } from "../../wardyn/copy/keep-machine";

const toastSuccess = vi.fn();
vi.mock("sonner", () => ({ toast: { success: (...a: unknown[]) => toastSuccess(...a), error: vi.fn() } }));
const listMock = vi.fn();
const deleteMock = vi.fn();
vi.mock("../../../lib/api/me-machines", () => ({ myMachines: () => listMock(), deleteMyMachine: (...a: unknown[]) => deleteMock(...a) }));
const getRunMock = vi.fn();
vi.mock("../../../lib/api/runs", () => ({ runs: { getRun: (...a: unknown[]) => getRunMock(...a) } }));

import { KeptMachinesCard } from "./kept-machines-card";

const GB = 1024 ** 3;
const dev = (over: Partial<KeptMachine> = {}): KeptMachine => ({
  id: "m-dev", name: "dev", owner_subject: "me", runner_id: "r1", base: "Standard", base_prepared_at: aheadByHours(-500), state: "kept", scrubbed: true,
  disk_bytes: 12 * GB, created_at: aheadByHours(-600), last_used_at: aheadByHours(-48), expires_at: aheadByHours(24 * 28), runner_name: "Maya's MacBook", ...over,
});
const review = (over: Partial<KeptMachine> = {}): KeptMachine =>
  dev({ id: "m-rev", name: "design-review", base: "Xcode 26", disk_bytes: 31 * GB, state: "in_use", run_id: "run_1234567890", ...over });
const reply = (machines: KeptMachine[], over: Partial<MyMachines> = {}): MyMachines => ({
  machines, allowed: true, count: machines.length, limit: 2, worst_bytes: 51 * GB, ...over,
});

const renderCard = (hash = "") =>
  render(
    <MemoryRouter initialEntries={[`/account${hash}`]}>
      <KeptMachinesCard />
    </MemoryRouter>,
  );

const mount = async () => {
  renderCard();
  await screen.findByTestId("kept-machines-card");
  await expandCard(T.TITLE);
  return screen.getByTestId("kept-machines-card");
};
const rowOf = (card: HTMLElement, name: string) => within(card).getByText(name).closest("li") as HTMLElement;

beforeEach(() => {
  toastSuccess.mockClear();
  listMock.mockReset().mockResolvedValue(reply([dev(), review()]));
  deleteMock.mockReset().mockResolvedValue(null);
  getRunMock.mockReset().mockResolvedValue({ id: "run_1234567890", title: "ui-polish" });
});

describe("Your kept machines", () => {
  it("km4-list: one kept and one in use, each with its Mac, base, size and dates", async () => {
    const card = await mount();
    expect(within(card).getByRole("heading", { name: new RegExp(`^${T.TITLE}`) })).toHaveTextContent(T.SUMMARY(2, "Maya's MacBook"));
    expect(within(card).getByText(T.LEDE())).toBeInTheDocument();
    const a = rowOf(card, "dev");
    expect(within(a).getByText(T.CHIP_KEPT)).toBeInTheDocument();
    expect(within(a).getByText(T.META("Maya's MacBook", "Standard", 12))).toBeInTheDocument();
    expect(within(a).getByText(T.DATES(mediumDate(dev().last_used_at), mediumDate(dev().expires_at)))).toBeInTheDocument();
    const b = rowOf(card, "design-review");
    expect(within(b).getByText(T.CHIP_IN_USE)).toBeInTheDocument();
    expect(within(b).getByText(T.META("Maya's MacBook", "Xcode 26", 31))).toBeInTheDocument();
    expect(await within(b).findByText(T.IN_USE("ui-polish"))).toBeInTheDocument();
    expect(within(b).getByRole("button", { name: `${T.DELETE} design-review` })).toBeDisabled();
    expect(within(a).getByRole("button", { name: `${T.DELETE} dev` })).toBeEnabled();
  });

  it("km4-list: the lede states the idle days when the server sends them, else names no number", async () => {
    listMock.mockResolvedValue(reply([dev()], { idle_days: 7 }));
    const card = await mount();
    expect(within(card).getByText(T.LEDE(7))).toBeInTheDocument();
    expect(card.textContent).toContain("7 days after its last use");
  });

  it("km4-list: a run with no title is named by its short id", async () => {
    getRunMock.mockResolvedValue({ id: "run_1234567890", title: "" });
    const card = await mount();
    expect(await within(rowOf(card, "design-review")).findByText(T.IN_USE("12345678"))).toBeInTheDocument();
  });

  it("a machine a run is still making reads Preparing, names that run, and cannot be deleted", async () => {
    listMock.mockResolvedValue(reply([dev({ state: "creating", scrubbed: false, run_id: "run_1234567890" })]));
    const a = rowOf(await mount(), "dev");
    expect(within(a).getByText(T.CHIP_PREPARING)).toBeInTheDocument();
    expect(await within(a).findByText(T.PREPARING("ui-polish"))).toBeInTheDocument();
    expect(within(a).queryByText(T.CHIP_NOT_CLEANED)).toBeNull();
    expect(within(a).getByRole("button", { name: `${T.DELETE} dev` })).toBeDisabled();
  });

  it("km4-not-scrubbed: Not cleaned yet, with the line that says why", async () => {
    listMock.mockResolvedValue(reply([dev({ scrubbed: false })]));
    const a = rowOf(await mount(), "dev");
    expect(within(a).getByText(T.CHIP_NOT_CLEANED)).toBeInTheDocument();
    expect(within(a).getByText(T.NOT_SCRUBBED)).toBeInTheDocument();
    expect(within(a).getByRole("button", { name: `${T.DELETE} dev` })).toBeEnabled();
  });

  it("km4-expiring: a machine within 7 days of its expiry says so, singular at one day", async () => {
    listMock.mockResolvedValue(reply([dev({ expires_at: aheadByHours(24 * 3 - 2) }), dev({ id: "x", name: "tomorrow", expires_at: aheadByHours(12) })]));
    const card = await mount();
    expect(within(rowOf(card, "dev")).getByText(T.EXPIRES_SOON(3))).toBeInTheDocument();
    expect(within(rowOf(card, "tomorrow")).getByText(T.EXPIRES_SOON(1))).toBeInTheDocument();
  });

  it("km4-deleting: a delete still under way reads Deleting with the neutral sentence, never claiming the Mac is offline", async () => {
    listMock.mockResolvedValue(reply([dev({ state: "deleting", delete_started_at: aheadByHours(-1), delete_reason: "person" })]));
    const a = rowOf(await mount(), "dev");
    expect(within(a).getByText(T.CHIP_DELETING)).toBeInTheDocument();
    expect(within(a).getByText(T.DELETE_UNCONFIRMED)).toBeInTheDocument();
    expect(within(a).queryByText(/offline/)).toBeNull();
    expect(within(a).getByRole("button", { name: `${T.DELETE} dev` })).toBeDisabled();
  });

  it("km4-expired: delete_reason expired reads Expired, not Deleting", async () => {
    const started = aheadByHours(-30);
    listMock.mockResolvedValue(reply([dev({ state: "deleting", delete_started_at: started, delete_reason: "expired" }), review()]));
    const a = rowOf(await mount(), "dev");
    expect(within(a).getByText(T.CHIP_EXPIRED)).toBeInTheDocument();
    expect(within(a).queryByText(T.CHIP_DELETING)).toBeNull();
    expect(within(a).getByText(T.EXPIRED(shortDate(started), "Maya's MacBook"))).toBeInTheDocument();
  });

  it("km4-confirm: Delete asks first, naming the machine and its Mac, and Cancel deletes nothing", async () => {
    const user = userEvent.setup();
    const card = await mount();
    await user.click(within(card).getByRole("button", { name: `${T.DELETE} dev` }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(T.CONFIRM_TITLE("dev"))).toBeInTheDocument();
    expect(within(dialog).getByText(T.CONFIRM_BODY("dev", "Maya's MacBook"))).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: T.CANCEL }));
    expect(deleteMock).not.toHaveBeenCalled();
  });

  it("km4-toasts: a confirmed delete says it was deleted and reloads the list without the row", async () => {
    const user = userEvent.setup();
    const card = await mount();
    listMock.mockResolvedValue(reply([review()]));
    await user.click(within(card).getByRole("button", { name: `${T.DELETE} dev` }));
    await user.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: T.DELETE }));
    await waitFor(() => expect(toastSuccess).toHaveBeenCalledWith(T.TOAST_DELETED("dev", "Maya's MacBook")));
    expect(deleteMock).toHaveBeenCalledWith("m-dev");
    await waitFor(() => expect(within(card).queryByText("dev")).toBeNull());
  });

  it("km4-toasts: a delete that waits for its Mac says it will finish when the Mac is back", async () => {
    const user = userEvent.setup();
    const card = await mount();
    deleteMock.mockResolvedValue(dev({ state: "deleting" }));
    await user.click(within(card).getByRole("button", { name: `${T.DELETE} dev` }));
    await user.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: T.DELETE }));
    await waitFor(() => expect(toastSuccess).toHaveBeenCalledWith(T.TOAST_DEFERRED("dev", "Maya's MacBook")));
  });

  it("km8-delete-in-use: a delete that races a resume shows the server's sentence and keeps the row", async () => {
    const user = userEvent.setup();
    const card = await mount();
    const sentence = "This machine is in use by a run. End that run first, then delete it.";
    deleteMock.mockRejectedValue(new HttpError(409, sentence, undefined, "kept_machine_in_use"));
    await user.click(within(card).getByRole("button", { name: `${T.DELETE} dev` }));
    await user.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: T.DELETE }));
    expect(await within(card).findByRole("alert")).toHaveTextContent(sentence);
    expect(within(card).getByText("dev")).toBeInTheDocument();
  });

  it("a delete the server could not confirm shows its sentence, and the reloaded row is Deleting", async () => {
    const user = userEvent.setup();
    const card = await mount();
    deleteMock.mockRejectedValue(new HttpError(503, T.DELETE_UNCONFIRMED, undefined, "kept_machine_delete_unconfirmed"));
    listMock.mockResolvedValue(reply([dev({ state: "deleting", delete_reason: "person" })]));
    await user.click(within(card).getByRole("button", { name: `${T.DELETE} dev` }));
    await user.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: T.DELETE }));
    expect(await within(card).findByRole("alert")).toHaveTextContent(T.DELETE_UNCONFIRMED);
    await waitFor(() => expect(within(rowOf(card, "dev")).getByText(T.CHIP_DELETING)).toBeInTheDocument());
    expect(within(rowOf(card, "dev")).queryByText(/offline/)).toBeNull();
  });

  it("km4-empty: the switch on and nothing kept shows None and the sentence that says how to keep one", async () => {
    listMock.mockResolvedValue(reply([], { count: 0 }));
    const card = await mount();
    expect(within(card).getByRole("heading", { name: new RegExp(`^${T.TITLE}`) })).toHaveTextContent(T.NONE);
    expect(within(card).getByText(T.EMPTY)).toBeInTheDocument();
    expect(within(card).getByText(T.LEDE())).toBeInTheDocument();
  });

  it("the switch off and nothing kept: the card is not drawn", async () => {
    listMock.mockResolvedValue(reply([], { allowed: false, reason: "kept_machine_site_off", count: 0 }));
    renderCard();
    await waitFor(() => expect(listMock).toHaveBeenCalled());
    expect(screen.queryByTestId("kept-machines-card")).toBeNull();
  });

  it("the switch off with machines already kept: they stay listed and deletable", async () => {
    listMock.mockResolvedValue(reply([dev()], { allowed: false, reason: "kept_machine_site_off" }));
    const card = await mount();
    expect(within(rowOf(card, "dev")).getByRole("button", { name: `${T.DELETE} dev` })).toBeEnabled();
  });

  it("a caller who is no person gets no card", async () => {
    listMock.mockRejectedValue(new HttpError(403, "no", undefined, "kept_machine_not_your_mac"));
    renderCard();
    await waitFor(() => expect(listMock).toHaveBeenCalled());
    expect(screen.queryByTestId("kept-machines-card")).toBeNull();
  });

  it("the list failing to load draws the card with a retry", async () => {
    listMock.mockRejectedValueOnce(new HttpError(500, "boom"));
    const card = await mount();
    expect(within(card).getByText(T.LOAD_FAILED)).toBeInTheDocument();
  });

  it("a load the server worded shows its sentence, not the default line", async () => {
    listMock.mockRejectedValueOnce(new HttpError(500, "Your sentence from the server.", "kept_machine_list_failed"));
    const card = await mount();
    expect(within(card).getByText("Your sentence from the server.")).toBeInTheDocument();
    expect(within(card).queryByText(STATES.ERROR_DEFAULT)).toBeNull();
  });

  it("a plain network error still shows the default line", async () => {
    listMock.mockRejectedValueOnce(new Error("Failed to fetch"));
    const card = await mount();
    expect(within(card).getByText(STATES.ERROR_DEFAULT)).toBeInTheDocument();
  });

  it("the link from a run's Machine tile opens the card and focuses it once the list is drawn, and a refresh does not reopen it", async () => {
    let release: (v: MyMachines) => void = () => undefined;
    listMock.mockReset().mockImplementationOnce(() => new Promise<MyMachines>((r) => (release = r)));
    renderCard("#kept-machines");
    expect(screen.queryByTestId("kept-machines-card")).toBeNull();
    release(reply([dev()]));
    const card = await screen.findByTestId("kept-machines-card");
    await waitFor(() => expect(card).toHaveFocus());
    const head = within(card).getByRole("button", { name: new RegExp(`^${T.TITLE}`) });
    expect(head).toHaveAttribute("aria-expanded", "true");
    expect(within(card).getByRole("button", { name: `${T.DELETE} dev` })).toBeVisible();

    // A delete reloads the list; the person collapsed the card meanwhile, and the answer does not reopen it.
    let reload: (v: MyMachines) => void = () => undefined;
    listMock.mockImplementationOnce(() => new Promise<MyMachines>((r) => (reload = r)));
    await userEvent.click(within(card).getByRole("button", { name: `${T.DELETE} dev` }));
    await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: T.DELETE }));
    await waitFor(() => expect(listMock).toHaveBeenCalledTimes(2));
    await userEvent.click(head);
    expect(head).toHaveAttribute("aria-expanded", "false");
    reload(reply([]));
    await waitFor(() => expect(within(card).getByText(T.NONE)).toBeInTheDocument());
    expect(head).toHaveAttribute("aria-expanded", "false");
  });

  it("without the hash the card stays collapsed", async () => {
    renderCard();
    const card = await screen.findByTestId("kept-machines-card");
    expect(within(card).getByRole("button", { name: new RegExp(`^${T.TITLE}`) })).toHaveAttribute("aria-expanded", "false");
    expect(card).not.toHaveFocus();
  });
});
