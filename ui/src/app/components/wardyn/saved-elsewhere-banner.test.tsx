/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { toast } from "sonner";
import { SavedElsewhereBanner } from "./saved-elsewhere-banner";
import { PROVIDERS, PROVIDERS_DRAFT } from "../../lib/workspace-providers-copy";

vi.mock("sonner", () => ({
  toast: { error: vi.fn(), success: vi.fn(), info: vi.fn() },
}));

describe("SavedElsewhereBanner", () => {
  beforeEach(() => {
    vi.mocked(toast.success).mockClear();
  });

  it("Copy my changes puts the WHOLE document on the clipboard and toasts (Q460-3)", async () => {
    const write = vi.fn().mockResolvedValue(undefined);
    Object.assign(navigator, { clipboard: { writeText: write } });
    const whole = JSON.stringify({ git: [{ id: "github", base_urls: ["https://github.com/acme"] }] }, null, 2);
    render(<SavedElsewhereBanner documentText={whole} onDiscard={() => {}} />);

    expect(screen.getByText(PROVIDERS.SAVED_ELSEWHERE_TITLE)).toBeInTheDocument();
    expect(screen.getByText(PROVIDERS.SAVED_ELSEWHERE_BODY)).toBeInTheDocument();
    // The whole document is what's shown, not just the changed fields.
    expect(screen.getByText(/github/)).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: PROVIDERS_DRAFT.CONFLICT_COPY }));
    expect(write).toHaveBeenCalledWith(whole);
    // toast is mocked (sonner renders nothing here) — asserted as a call, the
    // use-copy-to-clipboard.test.ts precedent, not a DOM node.
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith(PROVIDERS_DRAFT.CONFLICT_COPIED_TOAST));
  });

  it("a clipboard failure falls back to selecting the text instead of copying", async () => {
    Object.assign(navigator, { clipboard: undefined });
    const addRange = vi.fn();
    const removeAllRanges = vi.fn();
    vi.spyOn(window, "getSelection").mockReturnValue({ addRange, removeAllRanges } as unknown as Selection);

    render(<SavedElsewhereBanner documentText="the whole document" onDiscard={() => {}} />);
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS_DRAFT.CONFLICT_COPY }));

    expect(removeAllRanges).toHaveBeenCalled();
    expect(addRange).toHaveBeenCalled();
    expect(toast.success).not.toHaveBeenCalled();
  });

  it("Discard mine and reload fires onDiscard — never a 'save over theirs' control", async () => {
    const onDiscard = vi.fn();
    render(<SavedElsewhereBanner documentText="{}" onDiscard={onDiscard} />);

    // Copy my changes before Discard — order is part of the contract (#217).
    const buttons = screen.getAllByRole("button");
    expect(buttons[0]).toHaveTextContent(PROVIDERS_DRAFT.CONFLICT_COPY);
    expect(buttons[1]).toHaveTextContent(PROVIDERS_DRAFT.DISCARD_AND_RELOAD);
    expect(screen.queryByText(/save over theirs/i)).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: PROVIDERS_DRAFT.DISCARD_AND_RELOAD }));
    expect(onDiscard).toHaveBeenCalledTimes(1);
  });
});
