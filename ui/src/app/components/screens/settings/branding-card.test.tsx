/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const getSettings = vi.fn();
const save = vi.fn();
const remove = vi.fn();
vi.mock("../../../lib/api/branding", () => ({
  branding: { getSettings: () => getSettings(), save: (b: unknown) => save(b), remove: () => remove() },
}));
const toastSuccess = vi.fn();
const toastError = vi.fn();
vi.mock("sonner", () => ({
  toast: { success: (m: string) => toastSuccess(m), error: (m: string, o?: unknown) => toastError(m, o) },
}));

import { ThemeProvider } from "../../wardyn/theme-provider";
import { BrandingCard } from "./branding-card";
import { BRANDING } from "../../../lib/branding-copy";
import { expandCard, startsWith } from "../../../lib/test-dom";

function renderCard() {
  return render(
    <ThemeProvider>
      <BrandingCard />
    </ThemeProvider>,
  );
}

async function fill(label: string, value: string) {
  const input = screen.getAllByLabelText(label)[0];
  await userEvent.clear(input);
  if (value) await userEvent.type(input, value);
}

async function validDraft() {
  getSettings.mockResolvedValue({});
  renderCard();
  await waitFor(() => expect(getSettings).toHaveBeenCalled());
  // #1200 compact cards — collapsed by default; the form lives in the body.
  await expandCard(BRANDING.TITLE);
  await fill(BRANDING.ORG_NAME_LABEL, "Example Corp");
  await fill(BRANDING.PRIMARY_LABEL, "#7c3aed");
  await fill(BRANDING.TEXT_LABEL, "#ffffff");
}

// Testing Library collapses whitespace in the DOM text (the no-break space in
// "512 KB" included) but not in the string it is handed.
const norm = (s: string) => s.replace(/\s+/g, " ");

const saveButton = () => screen.getByRole("button", { name: BRANDING.SAVE });

afterEach(() => {
  getSettings.mockReset();
  save.mockReset();
  remove.mockReset();
  toastSuccess.mockReset();
  toastError.mockReset();
});

// #1486: the logo Field wraps a div, so its cloned aria-describedby missed the
// file input it describes.
describe("Branding card — the logo input's hint", () => {
  it("the file input is described by the logo hint", async () => {
    await validDraft();
    expect(screen.getByLabelText(BRANDING.LOGO_LABEL)).toHaveAccessibleDescription(BRANDING.LOGO_HINT);
  });
});

describe("Branding card (#1125)", () => {
  it("renders the canon heading, lede and labels", async () => {
    await validDraft();
    expect(screen.getByRole("heading", { name: startsWith(BRANDING.TITLE) })).toBeInTheDocument();
    expect(screen.getByText(BRANDING.LEDE)).toBeInTheDocument();
    expect(screen.getByText(norm(BRANDING.LOGO_HINT))).toBeInTheDocument();
    expect(screen.getByText(BRANDING.LINK_HINT)).toBeInTheDocument();
    expect(screen.getByText(BRANDING.PREVIEW_TITLE)).toBeInTheDocument();
    expect(screen.getByText(BRANDING.CONTRAST_OK("5.6"))).toBeInTheDocument();
    expect(saveButton()).toBeEnabled();
  });

  it("offers both name formats and previews the one chosen", async () => {
    await validDraft();
    const preview = screen.getByTestId("branding-preview");
    expect(within(preview).getByText("Example Corp Wardyn")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: /^Wardyn for Example Corp/ }));
    expect(within(preview).getByText("Wardyn for Example Corp")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^Example Corp Wardyn/ })).toHaveAttribute("aria-pressed", "false");
  });

  it("refuses an invalid colour, live, and holds the save", async () => {
    await validDraft();
    await fill(BRANDING.PRIMARY_LABEL, "#7Q3aeZ");
    expect(screen.getByText(BRANDING.ERR_COLOR)).toBeInTheDocument();
    expect(screen.getByText(BRANDING.FIX_ONE)).toBeInTheDocument();
    expect(saveButton()).toBeDisabled();
  });

  it("names the contrast ratio of a low-contrast pair", async () => {
    await validDraft();
    await fill(BRANDING.PRIMARY_LABEL, "#fef08a");
    expect(screen.getByText(BRANDING.ERR_CONTRAST("1.1"))).toBeInTheDocument();
    expect(saveButton()).toBeDisabled();
  });

  it("checks the dark pair when one is set", async () => {
    await validDraft();
    await userEvent.click(screen.getByLabelText(BRANDING.DARK_LABEL));
    const [, darkFill] = screen.getAllByLabelText(BRANDING.PRIMARY_LABEL);
    const [, darkText] = screen.getAllByLabelText(BRANDING.TEXT_LABEL);
    await userEvent.type(darkFill, "#a78bfa");
    await userEvent.type(darkText, "#ffffff");
    expect(screen.getByText(BRANDING.ERR_CONTRAST("2.7"))).toBeInTheDocument();
    expect(saveButton()).toBeDisabled();
  });

  it("refuses a Support link that is not https", async () => {
    await validDraft();
    await fill(BRANDING.LINK_LABEL, "http://status.example.com");
    expect(screen.getByText(BRANDING.ERR_LINK)).toBeInTheDocument();
    expect(saveButton()).toBeDisabled();
  });

  it("refuses an oversized logo with its size", async () => {
    await validDraft();
    const big = new File([new Uint8Array(3_560_000)], "example-corp-full-res.png", { type: "image/png" });
    fireEvent.change(screen.getByLabelText(BRANDING.LOGO_LABEL), { target: { files: [big] } });
    expect(screen.getByText(norm(BRANDING.ERR_LOGO("3.6 MB")))).toBeInTheDocument();
    expect(saveButton()).toBeDisabled();
    await fill(BRANDING.LINK_LABEL, "http://x.example.com");
    expect(screen.getByText(BRANDING.FIX_MANY)).toBeInTheDocument();
  });

  it("saves the draft, with the dark pair only when set and the logo only when picked", async () => {
    await validDraft();
    await fill(BRANDING.LINK_LABEL, "https://status.example.com");
    save.mockResolvedValue({ org_name: "Example Corp", name_format: "prefix", primary: "#7c3aed", primary_text: "#ffffff" });
    await userEvent.click(saveButton());
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1));
    expect(save.mock.calls[0][0]).toEqual({
      org_name: "Example Corp", name_format: "prefix", primary: "#7c3aed", primary_text: "#ffffff",
      support_url: "https://status.example.com",
    });
  });

  it("seeds from the stored brand", async () => {
    getSettings.mockResolvedValue({
      org_name: "Example Corp", name_format: "suffix", primary: "#7c3aed", primary_text: "#ffffff",
      dark_custom: true, dark_primary: "#a78bfa", dark_primary_text: "#171717", support_url: "https://status.example.com",
    });
    renderCard();
    await expandCard(BRANDING.TITLE);
    expect(await screen.findByDisplayValue("Example Corp")).toBeInTheDocument();
    expect(screen.getByDisplayValue("#a78bfa")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^Wardyn for Example Corp/ })).toHaveAttribute("aria-pressed", "true");
  });

  // #1215 — Remove logo / Remove branding, approved 2026-09-30.
  const STORED = {
    org_name: "Acme", name_format: "suffix", primary: "#1d4ed8", primary_text: "#ffffff",
    dark_custom: true, dark_primary: "#93c5fd", dark_primary_text: "#0a0a0a", support_url: "https://status.acme.example",
    logo_url: "/api/v1/branding/logo?v=abc", icon_url: "/api/v1/branding/logo?v=abc",
  };

  async function storedCard(extra: Record<string, unknown> = {}) {
    getSettings.mockResolvedValue({ ...STORED, ...extra });
    renderCard();
    await expandCard(BRANDING.TITLE);
    await screen.findByTestId("branding-stored-logo");
  }

  it("offers neither removal on a console with no branding", async () => {
    await validDraft();
    expect(screen.queryByRole("button", { name: BRANDING.REMOVE_LOGO })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: BRANDING.REMOVE_BRANDING })).not.toBeInTheDocument();
  });

  it("Remove logo: confirms in the approved words, then removes only the logo and toasts", async () => {
    await storedCard();
    save.mockResolvedValue({ ...STORED, logo_url: undefined });
    await userEvent.click(screen.getByRole("button", { name: BRANDING.REMOVE_LOGO }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(BRANDING.REMOVE_LOGO_TITLE)).toBeInTheDocument();
    expect(within(dialog).getByText(BRANDING.REMOVE_LOGO_BODY("Acme"))).toBeInTheDocument();
    expect(BRANDING.REMOVE_LOGO_BODY("Acme")).toBe(
      "The header and the browser tab show Acme’s initials instead. The name and colours stay.");
    expect(save).not.toHaveBeenCalled();
    await userEvent.click(within(dialog).getByRole("button", { name: BRANDING.REMOVE_LOGO_CONFIRM }));
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1));
    // The STORED record plus remove_logo — never a half-typed form.
    expect(save.mock.calls[0][0]).toEqual({
      org_name: "Acme", name_format: "suffix", primary: "#1d4ed8", primary_text: "#ffffff",
      support_url: "https://status.acme.example", dark_primary: "#93c5fd", dark_primary_text: "#0a0a0a", remove_logo: true,
    });
    expect(remove).not.toHaveBeenCalled();
    await waitFor(() => expect(toastSuccess).toHaveBeenCalledWith(BRANDING.REMOVE_LOGO_TOAST));
    expect(BRANDING.REMOVE_LOGO_TOAST).toBe("Logo removed.");
    await waitFor(() => expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument());
    // The logo row is gone and the file chooser is back; the initials tile is the preview's mark.
    expect(screen.queryByTestId("branding-stored-logo")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: BRANDING.REMOVE_LOGO })).not.toBeInTheDocument();
    expect(screen.getByLabelText(BRANDING.LOGO_LABEL)).toBeInTheDocument();
    expect(within(screen.getByTestId("branding-preview")).getByText("A")).toBeInTheDocument();
  });

  it("Cancel leaves the logo alone", async () => {
    await storedCard();
    await userEvent.click(screen.getByRole("button", { name: BRANDING.REMOVE_LOGO }));
    await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Cancel" }));
    expect(save).not.toHaveBeenCalled();
    expect(screen.getByTestId("branding-stored-logo")).toBeInTheDocument();
  });

  it("Remove branding: confirms in the approved words, deletes everything and toasts", async () => {
    await storedCard();
    remove.mockResolvedValue(undefined);
    await userEvent.click(screen.getByRole("button", { name: BRANDING.REMOVE_BRANDING }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(BRANDING.REMOVE_BRANDING_TITLE)).toBeInTheDocument();
    expect(within(dialog).getByText(BRANDING.REMOVE_BRANDING_BODY)).toBeInTheDocument();
    // A logo a person uploaded has no site-configuration line.
    expect(within(dialog).queryByText(BRANDING.FILE_LOGO_DIALOG_LINE_BRANDING)).not.toBeInTheDocument();
    await userEvent.click(within(dialog).getByRole("button", { name: BRANDING.REMOVE_BRANDING_CONFIRM }));
    await waitFor(() => expect(remove).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(toastSuccess).toHaveBeenCalledWith(BRANDING.REMOVE_BRANDING_TOAST));
    expect(BRANDING.REMOVE_BRANDING_TOAST).toBe("Branding removed.");
    // Back to a first visit: empty name, Save held, no removal offered.
    await waitFor(() => expect(screen.queryByRole("button", { name: BRANDING.REMOVE_BRANDING })).not.toBeInTheDocument());
    expect(screen.getByLabelText(BRANDING.ORG_NAME_LABEL)).toHaveValue("");
    expect(saveButton()).toBeDisabled();
  });

  it("a refused removal toasts the failure and keeps the dialog open", async () => {
    await storedCard();
    remove.mockRejectedValue(new Error("nope"));
    await userEvent.click(screen.getByRole("button", { name: BRANDING.REMOVE_BRANDING }));
    await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: BRANDING.REMOVE_BRANDING_CONFIRM }));
    await waitFor(() => expect(toastError).toHaveBeenCalledWith(BRANDING.REMOVE_BRANDING_FAILED, expect.anything()));
    expect(screen.getByRole("alertdialog")).toBeInTheDocument();
  });

  it("a logo from the site configuration: the note, no Remove logo, no chooser, and Remove branding says the logo returns", async () => {
    await storedCard({ logo_from_file: true });
    expect(screen.getByText(BRANDING.FILE_LOGO_NOTE)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: BRANDING.REMOVE_LOGO })).not.toBeInTheDocument();
    expect(screen.queryByLabelText(BRANDING.LOGO_LABEL)).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: BRANDING.REMOVE_BRANDING }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(BRANDING.REMOVE_BRANDING_BODY)).toBeInTheDocument();
    expect(within(dialog).getByText(BRANDING.FILE_LOGO_DIALOG_LINE_BRANDING)).toBeInTheDocument();
    expect(BRANDING.FILE_LOGO_DIALOG_LINE_BRANDING).toBe("The logo from your site configuration comes back once branding is set up again.");
  });
});
