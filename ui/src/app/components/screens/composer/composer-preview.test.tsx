/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { AssistantPreview } from "../../../lib/api/assistant";
import { ASSISTANT as T } from "../../wardyn/copy/assistant";
import { ComposerPreview, kindLabel } from "./composer-preview";

afterEach(cleanup);

const PREVIEW: AssistantPreview = {
  provider: "Org provider",
  model: "model-name",
  sections: [{ label: "Your unit", text: "<b>not markup</b>\nline two", untrusted: true }],
  digest: "d",
  letters: {},
  never_sent: [],
  tools: [],
};

function show(over: Partial<React.ComponentProps<typeof ComposerPreview>> = {}) {
  const onStart = vi.fn();
  const onCancel = vi.fn();
  render(<ComposerPreview preview={PREVIEW} changed={false} busy={false} error={null} onStart={onStart} onCancel={onCancel} {...over} />);
  return { onStart, onCancel };
}

describe("ComposerPreview", () => {
  it("shows each record's text as text, with its label and the untrusted mark", () => {
    show();
    const pre = screen.getByText(/not markup/);
    expect(pre.tagName).toBe("PRE");
    expect(pre.children).toHaveLength(0);
    expect(screen.getByText("Your unit")).toBeInTheDocument();
    expect(screen.getByText(T.UNTRUSTED)).toBeInTheDocument();
  });

  it("names the kinds the organisation keeps away only when there are some", () => {
    show();
    expect(screen.queryByText(/Kinds your organisation keeps away/)).toBeNull();
    cleanup();
    show({ preview: { ...PREVIEW, never_sent: ["governance_unit", "unknown_kind"] } });
    expect(screen.getByText(T.KINDS_KEPT_AWAY(`${kindLabel("governance_unit")}, unknown_kind`))).toBeInTheDocument();
  });

  it("says what changed above the preview only after a change", () => {
    show();
    expect(screen.queryByText(T.PREVIEW_CHANGED)).toBeNull();
    cleanup();
    show({ changed: true });
    expect(screen.getByRole("status")).toHaveTextContent(T.PREVIEW_CHANGED);
  });

  it("starts only on the Start button, and shows a refusal", async () => {
    const { onStart } = show({ error: "No." });
    expect(onStart).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: T.START }));
    expect(onStart).toHaveBeenCalledTimes(1);
    expect(within(screen.getByTestId("composer-preview")).getByRole("alert")).toHaveTextContent("No.");
  });
});
