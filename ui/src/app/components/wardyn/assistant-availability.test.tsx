/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The one visible line per state that says why the assistant is not available, and who is offered the way on.
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { AssistantAvailability, type AssistantAvailabilityProps } from "./assistant-availability";
import { ASSISTANT as T } from "./copy/assistant";
import { RAIL_MODEL_ACCESS } from "./model-access-copy";
import { OperatorProvider } from "./operator-context";

function renderLine(props: AssistantAvailabilityProps, operator = true) {
  return render(
    <MemoryRouter>
      <OperatorProvider operator={operator} operatorResolved>
        <AssistantAvailability {...props} />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

describe("AssistantAvailability", () => {
  it.each([
    ["off", {}, T.OFF],
    ["no_provider", {}, "No model provider can serve the assistant yet."],
    ["unit_denies", { providerName: "Org provider" }, "Your unit doesn't allow Org provider, the assistant's model provider."],
    ["not_connected", { providerName: "Org provider" }, "Connect your own credential for Org provider to use the assistant."],
    ["kind_never_sent", { kindLabel: "Role mapping" }, "Your organisation keeps Role mapping away from the assistant."],
    ["at_run_limit", {}, "You're at your run limit. End a run to start the assistant."],
    ["provider_error", { providerName: "Org provider" }, "Org provider didn't answer. Your draft is kept. Try again in a moment."],
  ] as const)("%s renders its line as visible text", (state, extra, line) => {
    renderLine({ state, ...extra });
    expect(screen.getByText(line)).toBeVisible();
  });

  it("renders nothing when the assistant is ready", () => {
    const { container } = renderLine({ state: "ready" });
    expect(container).toBeEmptyDOMElement();
  });

  it("offers Turn it on in Settings to a super admin, linked to Settings", () => {
    renderLine({ state: "off" });
    expect(screen.getByRole("link", { name: T.OFF_LINK })).toHaveAttribute("href", "/admin/settings");
  });

  it("shows a member the reason alone, never the way to turn it on", () => {
    renderLine({ state: "off" }, false);
    expect(screen.getByText(T.OFF)).toBeInTheDocument();
    expect(screen.queryByText(T.OFF_LINK)).toBeNull();
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("not_connected offers Connect, which opens the provider's own flow", async () => {
    const onConnect = vi.fn();
    renderLine({ state: "not_connected", providerId: "org-gw", providerName: "Org provider", onConnect });
    await userEvent.setup().click(screen.getByRole("button", { name: RAIL_MODEL_ACCESS.NO_PROVIDER_CTA }));
    expect(onConnect).toHaveBeenCalledWith("org-gw");
  });

  it("only not_connected offers Connect, and only provider_error offers Try again", async () => {
    const onRetry = vi.fn();
    const { unmount } = renderLine({ state: "unit_denies", providerName: "P" });
    expect(screen.queryByRole("button")).toBeNull();
    unmount();
    renderLine({ state: "provider_error", providerName: "P", onRetry });
    await userEvent.setup().click(screen.getByRole("button", { name: T.TRY_AGAIN }));
    expect(onRetry).toHaveBeenCalledTimes(1);
  });
});
