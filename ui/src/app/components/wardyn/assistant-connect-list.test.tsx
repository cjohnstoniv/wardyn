/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A person's list of the providers marked for the assistant: one chip per state, and Connect only where
// a credential is missing.
import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ADO_PAT } from "../../lib/ado-pat-copy";
import { AssistantConnectList, type AssistantConnectionRow } from "./assistant-connect-list";
import { ASSISTANT as T } from "./copy/assistant";
import { DENY_LISTS } from "./copy/deny-lists";
import { RAIL_MODEL_ACCESS } from "./model-access-copy";

const ROWS: AssistantConnectionRow[] = [
  { providerId: "org", label: "Org provider · model-name", state: "not_connected" },
  { providerId: "local", label: "Local provider · model-name-small", state: "connected" },
  { providerId: "second", label: "Second provider · model-name", state: "denied" },
];

const rowOf = (name: string) => within(screen.getByText(name).closest("li") as HTMLElement);

describe("AssistantConnectList", () => {
  it("shows the title, the hint and a marked chip on every row", () => {
    render(<AssistantConnectList rows={ROWS} />);
    expect(screen.getByRole("heading", { name: DENY_LISTS.STRIP_PROVIDERS })).toBeInTheDocument();
    expect(screen.getByText(T.CONNECT_HINT)).toBeInTheDocument();
    expect(screen.getAllByText(T.MARKED)).toHaveLength(3);
  });

  it("each state reads as its own chip, reusing the existing words", () => {
    render(<AssistantConnectList rows={ROWS} />);
    expect(rowOf("Org provider · model-name").getByText(ADO_PAT.CHIP_NOT_CONNECTED)).toBeInTheDocument();
    expect(rowOf("Local provider · model-name-small").getByText(ADO_PAT.CHIP_CONNECTED)).toBeInTheDocument();
    expect(rowOf("Second provider · model-name").getByText(T.DENIED_BY_UNIT)).toBeInTheDocument();
  });

  it("Connect appears on the not-connected row alone and opens that provider's flow", async () => {
    const onConnect = vi.fn();
    render(<AssistantConnectList rows={ROWS} onConnect={onConnect} />);
    expect(screen.getAllByRole("button")).toHaveLength(1);
    await userEvent.setup().click(rowOf("Org provider · model-name").getByRole("button", { name: new RegExp(RAIL_MODEL_ACCESS.NO_PROVIDER_CTA) }));
    expect(onConnect).toHaveBeenCalledWith("org");
  });
});
