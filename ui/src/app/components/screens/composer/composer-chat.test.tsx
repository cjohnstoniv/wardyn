/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The conversation on its own: text is characters, a tool call is one collapsed line, and what is posted is what was typed.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { ASSISTANT as T } from "../../wardyn/copy/assistant";
import { ComposerChat } from "./composer-chat";
import type { ChatItem } from "./composer-stream";

afterEach(cleanup);

const HOSTILE = "![x](https://example.invalid/x.png) [go](https://example.invalid) <img src=x onerror=alert(1)> **bold** # head";

function chat(items: ChatItem[], over: Partial<React.ComponentProps<typeof ComposerChat>> = {}) {
  const onSend = vi.fn().mockResolvedValue(undefined);
  render(
    <MemoryRouter>
      <ComposerChat items={items} letters={{ "Person A": "Alice" }} turns={[]} canSend onSend={onSend} {...over} />
    </MemoryRouter>,
  );
  return onSend;
}

describe("ComposerChat", () => {
  it("renders a message as text: the characters are there, and no image, link or element comes from them", () => {
    chat([
      { kind: "user", gen: 0, seq: 1, text: HOSTILE },
      { kind: "assistant", gen: 0, seq: 2, text: HOSTILE },
    ]);
    for (const n of screen.getAllByTestId(/^composer-message-/)) {
      expect(n.textContent).toBe(HOSTILE);
      expect(n.querySelector("img, a, strong, h1, em")).toBeNull();
      expect(n.children).toHaveLength(0);
    }
    expect(screen.queryByRole("img")).toBeNull();
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("keeps a URL as text and puts a letter's name back for display only", () => {
    chat([{ kind: "assistant", gen: 0, seq: 1, text: "Person A: see https://example.invalid/page" }]);
    expect(screen.getByTestId("composer-message-assistant").textContent).toBe("Alice: see https://example.invalid/page");
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("sends exactly what was typed, and keeps it in the box when the send fails", async () => {
    const onSend = chat([], {});
    await userEvent.type(screen.getByRole("textbox", { name: T.CHAT_PLACEHOLDER }), "tell Person A");
    await userEvent.click(screen.getByRole("button", { name: T.SEND }));
    expect(onSend).toHaveBeenCalledWith("tell Person A");
    cleanup();
    const failing = chat([], { onSend: vi.fn().mockRejectedValue(new Error("no")) });
    void failing;
    await userEvent.type(screen.getByRole("textbox", { name: T.CHAT_PLACEHOLDER }), "keep me");
    await userEvent.click(screen.getByRole("button", { name: T.SEND }));
    expect(screen.getByRole("textbox", { name: T.CHAT_PLACEHOLDER })).toHaveValue("keep me");
  });

  it("disables the box and Send when nothing can be sent", () => {
    chat([], { canSend: false });
    expect(screen.getByRole("textbox", { name: T.CHAT_PLACEHOLDER })).toBeDisabled();
    expect(screen.getByRole("button", { name: T.SEND })).toBeDisabled();
  });

  it("shows a tool call as one collapsed line", () => {
    chat([{ kind: "tool", gen: 0, seq: 1, name: "read", args: "unit Payments", result: "1 ceiling", ok: true, dryRun: true }]);
    expect(screen.getByRole("button", { name: /read.*1 ceiling/ })).toHaveAttribute("aria-expanded", "false");
  });
});
