/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import type { Workspace } from "../../../lib/types";
import { EGRESS } from "../../wardyn/copy";
import { AllowedHostsCard } from "./allowed-hosts-card";

const ws = (over: Partial<Workspace> = {}) =>
  ({ id: "w1", name: "w1", kind: "folder", approved_egress: [], requirements: {}, ...over }) as unknown as Workspace;

describe("AllowedHostsCard empty state (#3047)", () => {
  it("is a noun-phrase title with no period, and one sentence under it", () => {
    render(<AllowedHostsCard ws={ws()} onWorkspaceUpdated={() => {}} />);
    const title = screen.getByRole("heading", { name: EGRESS.ALLOWED_EMPTY_TITLE });
    expect(title.textContent).toBe("No approved hosts");
    expect(title.textContent).not.toMatch(/\.$/);
    expect(screen.getByText(EGRESS.ALLOWED_EMPTY_BODY)).toBeInTheDocument();
    expect(screen.queryByText("Nothing approved yet.")).toBeNull();
  });

  it("lists the hosts once there are some", () => {
    render(<AllowedHostsCard ws={ws({ approved_egress: ["registry.npmjs.org"] })} onWorkspaceUpdated={() => {}} />);
    expect(screen.getByText("registry.npmjs.org")).toBeInTheDocument();
    expect(screen.queryByText(EGRESS.ALLOWED_EMPTY_TITLE)).toBeNull();
  });
});
