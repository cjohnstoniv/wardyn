/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { fireEvent, screen, within } from "@testing-library/react";

/** Shows one of New Run's four panels the way a person does, through the panel
 *  nav. A panel that is not on screen is hidden, so its controls have no role
 *  to find until this is called. */
export function goToPanel(panel: "Run" | "Workspace" | "Access" | "Policy"): void {
  fireEvent.click(
    within(screen.getByRole("navigation", { name: "New run" })).getByRole("button", { name: new RegExp(`^${panel}`) }),
  );
}
