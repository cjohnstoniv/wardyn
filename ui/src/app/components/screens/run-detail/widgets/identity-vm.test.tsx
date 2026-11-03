/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Identity card on a VM run: a Machine row (with the macOS base), and Started apart from Started by.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import type { RunDetail } from "../../../../lib/types";

vi.mock("../../../../lib/api/health", () => ({ health: { health: vi.fn().mockResolvedValue({ status: "ok" }) } }));

import { IdentityWidget } from "./identity";

const run: RunDetail = {
  id: "run-1",
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
  created_by: "alice@example.com",
  agent: "claude-code",
  repo: "acme/widgets",
  task: "t",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://x",
  runner_target: "docker",
  placement: "local",
};
const dd = (label: string) => screen.getByText(label).nextElementSibling as HTMLElement;

beforeEach(() => vi.clearAllMocks());

describe("IdentityWidget on a VM run", () => {
  it("names the macOS VM and its base", () => {
    render(<IdentityWidget run={{ ...run, machine_kind: "macos_vm", vm_machine: { macos_base: "xcode-16" } as RunDetail["vm_machine"] }} onGoPolicy={() => {}} />);
    expect(dd("Machine")).toHaveTextContent("macOS VM · xcode-16");
  });

  it("names a Linux VM without a base", () => {
    render(<IdentityWidget run={{ ...run, machine_kind: "linux_vm" }} onGoPolicy={() => {}} />);
    expect(dd("Machine")).toHaveTextContent(/^Linux VM$/);
  });

  it("has no Machine row on a container run", () => {
    render(<IdentityWidget run={run} onGoPolicy={() => {}} />);
    expect(screen.queryByText("Machine")).toBeNull();
  });

  it("keeps the time under Started and the person under Started by", () => {
    render(<IdentityWidget run={run} onGoPolicy={() => {}} />);
    expect(dd("Started by")).toHaveTextContent(/^alice@example\.com$/);
    expect(dd("Started").textContent).not.toContain("alice");
  });
});
