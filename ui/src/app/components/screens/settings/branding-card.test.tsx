/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const getSettings = vi.fn();
const save = vi.fn();
vi.mock("../../../lib/api/branding", () => ({
  branding: { getSettings: () => getSettings(), save: (b: unknown) => save(b) },
}));

import { ThemeProvider } from "../../wardyn/theme-provider";
import { BrandingCard } from "./branding-card";
import { BRANDING } from "../../../lib/branding-copy";

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
});

describe("Branding card (#1125)", () => {
  it("renders the canon heading, lede and labels", async () => {
    await validDraft();
    expect(screen.getByRole("heading", { name: BRANDING.TITLE })).toBeInTheDocument();
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
    expect(await screen.findByDisplayValue("Example Corp")).toBeInTheDocument();
    expect(screen.getByDisplayValue("#a78bfa")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^Wardyn for Example Corp/ })).toHaveAttribute("aria-pressed", "true");
  });
});
