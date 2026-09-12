/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Agent picker in isolation (§5c.4). NewRunScreen's own suite covers the
// roster-driven cases end to end; this file pins the two the SCREEN cannot
// reach — a value the options don't carry, which is what a cloned BYOA run
// lands on.
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { AgentPicker } from "./agent-picker";
import { agentLabel, type WizardAgent } from "./wizard-types";

function renderPicker(value: WizardAgent, harnesses?: Parameters<typeof AgentPicker>[0]["harnesses"]) {
  const onChange = vi.fn();
  render(<AgentPicker value={value} harnesses={harnesses} onChange={onChange} />);
  return onChange;
}

// V1 r2 MEDIUM: the roster-unknown fallback listed claude-code/codex-cli only,
// but `none` ("Your own tools") is a real WizardAgent — a cloned BYOA run
// painted an EMPTY trigger (Radix has no item to read the selected value off)
// and offered no way back to the value the run would actually launch with.
describe("AgentPicker — the value on screen is always the value the run will use", () => {
  it("roster-unknown lists all three WIZARD_AGENTS, including none", async () => {
    renderPicker("none", undefined);
    const trigger = screen.getByRole("combobox", { name: "Agent" });
    expect(trigger).toHaveTextContent(agentLabel("none"));
    expect(agentLabel("none")).toBe("Your own tools");

    await userEvent.click(trigger);
    expect(await screen.findByRole("option", { name: "Claude Code" })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "Codex CLI" })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "Your own tools" })).toBeInTheDocument();
  });

  it("an OFF-ROSTER value still names itself in the trigger, and is never disabled", async () => {
    // A roster that omits `none` entirely — the same hole, reached with a
    // roster that DID load.
    renderPicker("none", [
      { id: "claude-code", display: "Claude Code", has_gateway: true, has_login: true, enabled: true },
    ]);
    const trigger = screen.getByRole("combobox", { name: "Agent" });
    expect(trigger).toHaveTextContent("Your own tools");
    await userEvent.click(trigger);
    const own = await screen.findByRole("option", { name: "Your own tools" });
    expect(own).not.toHaveAttribute("aria-disabled", "true");
  });

  it("a roster value keeps the roster's OWN display name, not agentLabel's", async () => {
    renderPicker("claude-code", [
      { id: "claude-code", display: "Claude Code (corp image)", has_gateway: true, has_login: true, enabled: true },
    ]);
    expect(screen.getByRole("combobox", { name: "Agent" })).toHaveTextContent("Claude Code (corp image)");
  });
});
