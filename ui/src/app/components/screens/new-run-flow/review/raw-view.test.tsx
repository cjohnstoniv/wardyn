/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { parse } from "yaml";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import type { RunIntent } from "../draft/run-intent";
import { RawView } from "./raw-view";

afterEach(cleanup);

const INTENT: RunIntent = {
  shape: "agent_task", experience: "background", no_end: false, runner_pool_id: "p1",
  inline_policy: { allowed_domains: [] },
  tools: [{ id: "claude-code", kind: "harness" }],
};
const DOCUMENT = { api_version: "wardyn/v1", kind: "Run", intent: INTENT };
const shown = () => (document.querySelector("pre code") as HTMLElement).textContent ?? "";

describe("Review raw views", () => {
  it.each([
    ["json", (t: string) => JSON.parse(t)],
    ["yaml", (t: string) => parse(t)],
  ] as const)("%s serialises the real run document, keeping an explicit false and an empty list", (format, read) => {
    render(<RawView intent={INTENT} format={format} go={vi.fn()} />);
    expect(read(shown())).toEqual(DOCUMENT);
    expect(screen.getByRole("heading", { name: NR_REVIEW.RAW_TITLE(format.toUpperCase()) })).toBeInTheDocument();
    expect(screen.getByText(NR_REVIEW.RAW_BODY)).toBeInTheDocument();
  });

  it("copies exactly the text it shows", async () => {
    const user = userEvent.setup();
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    render(<RawView intent={INTENT} format="yaml" go={vi.fn()} />);
    await user.click(screen.getByRole("button", { name: NR_REVIEW.COPY }));
    expect(writeText).toHaveBeenCalledWith(shown());
    await waitFor(() => expect(screen.getByRole("button", { name: NR_REVIEW.COPIED })).toBeInTheDocument());
  });

  it("opens the editor in the same format", async () => {
    const user = userEvent.setup();
    const go = vi.fn();
    render(<RawView intent={INTENT} format="json" go={go} />);
    await user.click(screen.getByRole("button", { name: NR_REVIEW.EDIT_FORMAT("JSON") }));
    expect(go).toHaveBeenCalledWith({ screen: "document", format: "json" });
  });
});
