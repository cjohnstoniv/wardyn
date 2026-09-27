/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { health } from "../../lib/api/health";
import { PreviewAsNewUser, PreviewAvailableProvider, UserPreviewBanner } from "./user-preview";
import { CONSOLE_VIEW, USER_PREVIEW } from "./copy/console-view";

afterEach(() => {
  vi.restoreAllMocks();
});

function stubAssign() {
  const assign = vi.fn();
  vi.spyOn(window, "location", "get").mockReturnValue({ ...window.location, assign });
  return assign;
}

describe("PreviewAsNewUser (the Permissions header)", () => {
  it("renders nothing unless the shell says the preview is available", () => {
    const { container } = render(<PreviewAsNewUser />);
    expect(container).toBeEmptyDOMElement();
  });

  it("posts the no-credential posture and reloads into the User view", async () => {
    const setMode = vi.spyOn(health, "setMemberMode").mockResolvedValue(undefined);
    const assign = stubAssign();
    render(
      <PreviewAvailableProvider value>
        <PreviewAsNewUser />
      </PreviewAvailableProvider>,
    );
    await userEvent.click(screen.getByRole("button", { name: USER_PREVIEW.MENU_NEW }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/runs"));
    expect(setMode).toHaveBeenCalledWith(true, true);
  });

  it("a failed POST says so and does not reload", async () => {
    vi.spyOn(health, "setMemberMode").mockRejectedValue(new Error("boom"));
    const assign = stubAssign();
    render(
      <PreviewAvailableProvider value>
        <PreviewAsNewUser />
      </PreviewAvailableProvider>,
    );
    await userEvent.click(screen.getByRole("button", { name: USER_PREVIEW.MENU_NEW }));
    expect(await screen.findByRole("alert")).toHaveTextContent(CONSOLE_VIEW.SWITCH_FAILED);
    expect(assign).not.toHaveBeenCalled();
  });
});

describe("UserPreviewBanner", () => {
  it("renders nothing outside the preview", () => {
    const { container } = render(<UserPreviewBanner active={false} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("states the preview, never 'member', and Exit preview returns to the Admin view", async () => {
    const setMode = vi.spyOn(health, "setMemberMode").mockResolvedValue(undefined);
    const assign = stubAssign();
    render(<UserPreviewBanner active />);
    const band = screen.getByRole("status");
    expect(band).toHaveTextContent(USER_PREVIEW.BANNER);
    expect(band.textContent).not.toMatch(/member/i);
    await userEvent.click(screen.getByRole("button", { name: USER_PREVIEW.EXIT }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/admin"));
    expect(setMode).toHaveBeenCalledWith(false, false);
  });
});
