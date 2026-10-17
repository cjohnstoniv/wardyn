/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

vi.mock("../../../lib/api/runs", () => ({ runs: { gradePolicy: vi.fn().mockResolvedValue(null) } }));

// The Allowed barriers control writes through setSpecKey. A refusal on a source
// that still parses is rare, so the test decides when the helper refuses.
const refusal = vi.hoisted(() => ({ next: null as null | { ok: false; line: number; column: number; message: string } }));
vi.mock("../../wardyn/policy-document/policy-source", async (original) => {
  const actual = await original<typeof import("../../wardyn/policy-document/policy-source")>();
  return {
    ...actual,
    setSpecKey: (...args: Parameters<typeof actual.setSpecKey>) => refusal.next ?? actual.setSpecKey(...args),
  };
});

import type { PolicySourceFormat } from "../../wardyn/policy-document/policy-source";
import { ProfileCeilingPolicy } from "./profile-ceiling-policy";

const SOURCE = "# ceiling\nmin_confinement_class: CC2 # wall\nallowed_domains: []\n";
let source = SOURCE;

function Ceiling() {
  const [text, setText] = React.useState(SOURCE);
  const [format, setFormat] = React.useState<PolicySourceFormat>("yaml");
  source = text;
  return (
    <ProfileCeilingPolicy source={text} format={format} onSourceChange={setText} onFormatChange={setFormat} disabled={false} />
  );
}

beforeEach(() => {
  refusal.next = null;
  source = SOURCE;
});

describe("ProfileCeilingPolicy — Allowed barriers", () => {
  it("writes the floor into the source and keeps the comments around it", async () => {
    render(<Ceiling />);
    await userEvent.click(screen.getByRole("button", { name: "Vault" }));
    expect(source).toBe("# ceiling\nmin_confinement_class: CC3 # wall\nallowed_domains: []\n");
    expect(screen.getByRole("button", { name: "Vault", pressed: true })).toBeInTheDocument();
  });

  it("says a refused write beside the control, with its position, and leaves the source alone", async () => {
    refusal.next = { ok: false, line: 2, column: 1, message: "The edit must contain only JSON-compatible values and safe numbers." };
    render(<Ceiling />);
    await userEvent.click(screen.getByRole("button", { name: "Vault" }));

    const said = screen.getByText(/The edit must contain only JSON-compatible values and safe numbers\./);
    expect(said).toHaveAttribute("role", "status");
    expect(said).toHaveTextContent("Line 2, column 1");
    expect(source).toBe(SOURCE);
    expect(screen.getByRole("button", { name: "Wall", pressed: true })).toBeInTheDocument();

    // The next write that succeeds changes the source, and the old refusal goes with it.
    refusal.next = null;
    await userEvent.click(screen.getByRole("button", { name: "Vault" }));
    expect(source).toContain("min_confinement_class: CC3");
    expect(screen.queryByText(/The edit must contain only/)).toBeNull();
  });
});
