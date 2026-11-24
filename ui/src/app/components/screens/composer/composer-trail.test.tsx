/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { ASSISTANT as T } from "../../wardyn/copy/assistant";
import type { ToolItem } from "./composer-stream";
import { ComposerTrail, ToolLine, usageAfter } from "./composer-trail";

afterEach(cleanup);

const TOOL: ToolItem = { kind: "tool", gen: 0, seq: 3, name: "validate", args: "run draft", result: "valid", ok: true, dryRun: true };

describe("ToolLine", () => {
  it("is collapsed until clicked, then shows its arguments, the dry-run mark and the token use", async () => {
    render(<ToolLine item={TOOL} usage={{ input_tokens: 3 }} />);
    const line = screen.getByRole("button", { name: /validate/ });
    expect(line).toHaveAttribute("aria-expanded", "false");
    expect(screen.getByText("run draft")).not.toBeVisible();
    await userEvent.click(line);
    expect(screen.getByText("run draft")).toBeVisible();
    expect(screen.getByText(T.DRY_RUN)).toBeVisible();
    expect(screen.getByText("input_tokens 3")).toBeVisible();
  });

  it("shows no dry-run mark for a call the server did not record as one", async () => {
    render(<ToolLine item={{ ...TOOL, dryRun: null }} />);
    await userEvent.click(screen.getByRole("button"));
    expect(screen.queryByText(T.DRY_RUN)).toBeNull();
  });
});

describe("usageAfter", () => {
  it("takes the first turn that ended after the call", () => {
    const turns = [
      { gen: 0, seq: 2, usage: { a: 1 } },
      { gen: 0, seq: 9, usage: { a: 2 } },
    ];
    expect(usageAfter(turns, { gen: 0, seq: 3 })).toEqual({ a: 2 });
    expect(usageAfter(turns, { gen: 0, seq: 9 })).toBeNull();
  });
});

describe("ComposerTrail", () => {
  it("lists the calls and the audit fields, and links the assistant's run", () => {
    render(
      <MemoryRouter>
        <ComposerTrail
          tools={[TOOL]}
          suggestion={{ promptDigest: "sha256:aa", outputDigest: "sha256:bb", tools: ["validate"] }}
          audit={{ person: "alice", purpose: T.COMPOSE, provider: "Org provider", model: "model-name", linked: "", runHref: "/runs/r-1" }}
        />
      </MemoryRouter>,
    );
    const trail = screen.getByTestId("composer-trail");
    for (const label of [T.TOOL_CALLS, T.AUDIT_PERSON, T.AUDIT_PURPOSE, T.COL_PROVIDER, T.AUDIT_MODEL, T.AUDIT_PROMPT_DIGEST, T.AUDIT_OUTPUT_DIGEST, T.AUDIT_TOOLS, T.AUDIT_LINKED]) {
      expect(within(trail).getByText(label)).toBeInTheDocument();
    }
    expect(within(trail).getByText("sha256:aa")).toBeInTheDocument();
    expect(within(trail).getByRole("link", { name: T.OPEN_RUN })).toHaveAttribute("href", "/runs/r-1");
  });

  it("leaves the digests out before a turn has completed", () => {
    render(
      <MemoryRouter>
        <ComposerTrail tools={[]} suggestion={null} audit={{ person: "alice", purpose: T.COMPOSE, provider: "p", model: "m", linked: "", runHref: null }} />
      </MemoryRouter>,
    );
    expect(screen.queryByText(T.AUDIT_PROMPT_DIGEST)).toBeNull();
    expect(screen.queryByRole("link")).toBeNull();
  });
});
