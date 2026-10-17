/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Key domains card (mock packet M5 S2/S4). Every expected string is read from KEY_DOMAINS,
// which is the packet's text; the Go wire shape is pinned in wire-parity.test.ts.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

const listMock = vi.fn();
const setMock = vi.fn();
const removeMock = vi.fn();
vi.mock("../../lib/api/key-domains", () => ({
  keyDomains: {
    list: (...a: unknown[]) => listMock(...a),
    set: (...a: unknown[]) => setMock(...a),
    remove: (...a: unknown[]) => removeMock(...a),
  },
}));

import { HttpError } from "../../lib/api/core";
import type { KeyDomains } from "../../lib/api/key-domains";
import { KEY_DOMAINS } from "../wardyn/copy/credentials";
import { KeyDomainsSection } from "./credentials-key-domains";

function renderCard() {
  return render(
    <MemoryRouter>
      <KeyDomainsSection />
    </MemoryRouter>,
  );
}

const data = (over: Partial<KeyDomains> = {}): KeyDomains => ({
  domains: [
    { domain: "default", declared: true, live_keys: 4, key: "Credential key", proven: true },
    { domain: "finance", declared: true, live_keys: 1, key: "Transit key finance", proven: true },
    { domain: "ghost", declared: false, live_keys: 2, proven: false },
  ],
  assignments: [
    { subject_type: "group", subject: "eng-finance", domain: "finance", set_by: "sec-1", set_at: "2026-10-01T00:00:00Z" },
    { subject_type: "all", subject: "all", domain: "default", set_by: "sec-1", set_at: "2026-10-01T00:00:00Z" },
  ],
  principal_keys: true,
  ...over,
});

beforeEach(() => {
  listMock.mockReset();
  setMock.mockReset();
  removeMock.mockReset();
});

describe("the domains table", () => {
  it("lists each domain with its key and whether boot proved it", async () => {
    listMock.mockResolvedValue(data());
    renderCard();
    expect(await screen.findByText(KEY_DOMAINS.TITLE)).toBeInTheDocument();
    // The title is on the loading skeleton too; the lede appears only with the data.
    expect(await screen.findByText(KEY_DOMAINS.LEDE)).toBeInTheDocument();
    const finance = screen.getAllByText("finance")[0].closest("tr")!;
    expect(within(finance).getByText("Transit key finance")).toBeInTheDocument();
    expect(within(finance).getByText(KEY_DOMAINS.PROVEN)).toBeInTheDocument();
    const ghost = screen.getByText("ghost").closest("tr")!;
    expect(within(ghost).getByText(KEY_DOMAINS.NOT_PROVEN)).toBeInTheDocument();
    expect(screen.getByText(KEY_DOMAINS.DEFAULT_NOTE)).toBeInTheDocument();
    expect(screen.queryByText(KEY_DOMAINS.OFF_NOTE)).toBeNull();
  });

  it("says domains apply to audit records only while per-person keys are off", async () => {
    listMock.mockResolvedValue(data({ principal_keys: false }));
    renderCard();
    expect(await screen.findByText(KEY_DOMAINS.OFF_NOTE)).toBeInTheDocument();
  });

  it("shows no card where the deployment has no key-domain service (501)", async () => {
    listMock.mockRejectedValue(new HttpError(501, "key domains require the Postgres store backend"));
    const { container } = renderCard();
    await waitFor(() => expect(listMock).toHaveBeenCalled());
    await waitFor(() => expect(container).toBeEmptyDOMElement());
  });

  it("a failed read gets the error state with Retry", async () => {
    listMock.mockRejectedValueOnce(new HttpError(500, "boom"));
    renderCard();
    const retry = await screen.findByRole("button", { name: "Retry" });
    listMock.mockResolvedValueOnce(data());
    await userEvent.click(retry);
    expect(await screen.findByText(KEY_DOMAINS.LEDE)).toBeInTheDocument();
  });
});

describe("assignments", () => {
  it("lists each with its domain, naming everyone as such", async () => {
    listMock.mockResolvedValue(data());
    renderCard();
    expect(await screen.findByText(KEY_DOMAINS.ASSIGN_TITLE)).toBeInTheDocument();
    expect(screen.getByText(KEY_DOMAINS.PRECEDENCE)).toBeInTheDocument();
    expect(screen.getByText("eng-finance").closest("tr")).toHaveTextContent("finance");
    expect(screen.getByText(KEY_DOMAINS.SOURCE_ALL).closest("tr")).toHaveTextContent("default");
    expect(screen.getAllByRole("button", { name: KEY_DOMAINS.REMOVE })).toHaveLength(2);
  });

  it("Assign a domain sets a person's domain, then reads the lists again", async () => {
    listMock.mockResolvedValue(data());
    setMock.mockResolvedValue("applied");
    renderCard();
    await screen.findByText(KEY_DOMAINS.ASSIGN_TITLE);
    await userEvent.click(screen.getByRole("button", { name: KEY_DOMAINS.ASSIGN_CTA }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(KEY_DOMAINS.ASSIGN_HINT)).toBeInTheDocument();
    await userEvent.type(within(dialog).getByLabelText(KEY_DOMAINS.TYPE_USER, { selector: "#key-domain-subject" }), "ann@corp.example");
    await userEvent.selectOptions(within(dialog).getByLabelText(KEY_DOMAINS.FIELD_DOMAIN), "finance");
    await userEvent.click(within(dialog).getByRole("button", { name: KEY_DOMAINS.ASSIGN_CTA }));
    await waitFor(() => expect(setMock).toHaveBeenCalledWith("user", "ann@corp.example", "finance"));
    await waitFor(() => expect(listMock).toHaveBeenCalledTimes(2));
    expect(screen.queryByText(KEY_DOMAINS.SUBMITTED_TITLE)).toBeNull();
    // Only declared domains (and default) are offered; the undeclared one that live keys name is not.
    expect(setMock).toHaveBeenCalledTimes(1);
  });

  it("offers declared domains only, and Everyone needs no subject", async () => {
    listMock.mockResolvedValue(data());
    setMock.mockResolvedValue("applied");
    renderCard();
    await screen.findByText(KEY_DOMAINS.ASSIGN_TITLE);
    await userEvent.click(screen.getByRole("button", { name: KEY_DOMAINS.ASSIGN_CTA }));
    const dialog = await screen.findByRole("dialog");
    const options = within(within(dialog).getByLabelText(KEY_DOMAINS.FIELD_DOMAIN)).getAllByRole("option").map((o) => o.textContent);
    expect(options).toEqual(["default", "finance"]);
    const save = within(dialog).getByRole("button", { name: KEY_DOMAINS.ASSIGN_CTA });
    expect(save).toBeDisabled();
    await userEvent.click(within(dialog).getByRole("radio", { name: KEY_DOMAINS.TYPE_ALL }));
    expect(save).toBeEnabled();
    await userEvent.click(save);
    await waitFor(() => expect(setMock).toHaveBeenCalledWith("all", "", "default"));
  });

  it("a held write (202) shows the amber Submitted note and does not read the lists as changed", async () => {
    listMock.mockResolvedValue(data());
    setMock.mockResolvedValue("pending");
    renderCard();
    await screen.findByText(KEY_DOMAINS.ASSIGN_TITLE);
    await userEvent.click(screen.getByRole("button", { name: KEY_DOMAINS.ASSIGN_CTA }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText(KEY_DOMAINS.TYPE_USER, { selector: "#key-domain-subject" }), "ann");
    await userEvent.click(within(dialog).getByRole("button", { name: KEY_DOMAINS.ASSIGN_CTA }));
    const note = await screen.findByRole("status");
    expect(note).toHaveTextContent(KEY_DOMAINS.SUBMITTED_TITLE);
    expect(note).toHaveTextContent(KEY_DOMAINS.SUBMITTED_BODY);
    expect(within(note).getByRole("link", { name: KEY_DOMAINS.SUBMITTED_LINK })).toBeInTheDocument();
    // Nothing was applied: no re-read, and the table is the one it was.
    expect(listMock).toHaveBeenCalledTimes(1);
    expect(screen.queryByText("ann")).toBeNull();
  });

  it("a refused write keeps the dialog open with the server's sentence", async () => {
    listMock.mockResolvedValue(data());
    setMock.mockRejectedValue(new HttpError(422, "The key domain \"ghost\" is not declared."));
    renderCard();
    await screen.findByText(KEY_DOMAINS.ASSIGN_TITLE);
    await userEvent.click(screen.getByRole("button", { name: KEY_DOMAINS.ASSIGN_CTA }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText(KEY_DOMAINS.TYPE_USER, { selector: "#key-domain-subject" }), "ann");
    await userEvent.click(within(dialog).getByRole("button", { name: KEY_DOMAINS.ASSIGN_CTA }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("is not declared");
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  it("Remove asks first, in the approved words, then removes that assignment and reads again", async () => {
    listMock.mockResolvedValue(data());
    removeMock.mockResolvedValue("applied");
    renderCard();
    await screen.findByText(KEY_DOMAINS.ASSIGN_TITLE);
    const row = screen.getByText("eng-finance").closest("tr")!;
    await userEvent.click(within(row).getByRole("button", { name: KEY_DOMAINS.REMOVE }));
    const dialog = await screen.findByRole("alertdialog");
    const [q, rest] = KEY_DOMAINS.REMOVE_CONFIRM("eng-finance").split("? ");
    expect(within(dialog).getByText(`${q}?`)).toBeInTheDocument();
    expect(within(dialog).getByText(rest)).toBeInTheDocument();
    await userEvent.click(within(dialog).getByRole("button", { name: KEY_DOMAINS.REMOVE }));
    await waitFor(() => expect(removeMock).toHaveBeenCalledWith("group", "eng-finance"));
    await waitFor(() => expect(listMock).toHaveBeenCalledTimes(2));
  });

  it("a held remove (202) shows the note and leaves the row", async () => {
    listMock.mockResolvedValue(data());
    removeMock.mockResolvedValue("pending");
    renderCard();
    await screen.findByText(KEY_DOMAINS.ASSIGN_TITLE);
    const row = screen.getByText("eng-finance").closest("tr")!;
    await userEvent.click(within(row).getByRole("button", { name: KEY_DOMAINS.REMOVE }));
    const dialog = await screen.findByRole("alertdialog");
    await userEvent.click(within(dialog).getByRole("button", { name: KEY_DOMAINS.REMOVE }));
    expect(await screen.findByRole("status")).toHaveTextContent(KEY_DOMAINS.SUBMITTED_TITLE);
    expect(screen.getByText("eng-finance")).toBeInTheDocument();
    expect(listMock).toHaveBeenCalledTimes(1);
  });

  it("Cancel removes nothing", async () => {
    listMock.mockResolvedValue(data());
    renderCard();
    await screen.findByText(KEY_DOMAINS.ASSIGN_TITLE);
    const row = screen.getByText("eng-finance").closest("tr")!;
    await userEvent.click(within(row).getByRole("button", { name: KEY_DOMAINS.REMOVE }));
    const dialog = await screen.findByRole("alertdialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(removeMock).not.toHaveBeenCalled();
  });
});
