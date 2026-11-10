/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Audit rows that carry a second line (a gateway call id or a detail): the sentence's floor, the call id with its
// copy button, and the gateway token mint row. Its own file: audit.test.tsx is at the file-size limit.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { AuditEvent } from "../../lib/types";
import { aheadByHours } from "../../lib/test-clock";

const listAuditMock = vi.fn();
vi.mock("../../lib/api/audit", async (importOriginal) => ({
  audit: { listAudit: (...a: unknown[]) => listAuditMock(...a) },
  // The real reading of a row's call id, not a stub.
  gatewayCallId: (await importOriginal<typeof import("../../lib/api/audit")>()).gatewayCallId,
}));
vi.mock("../../lib/api/runs", () => ({
  runs: { getRun: vi.fn().mockResolvedValue(undefined), listRuns: vi.fn().mockResolvedValue([]) },
}));
vi.mock("../../lib/api/health", () => ({
  health: { health: () => Promise.resolve({}), whoami: () => Promise.resolve(null) },
}));

import { AuditScreen } from "./audit";

function ev(partial: Partial<AuditEvent> & { id: string }): AuditEvent {
  return { time: aheadByHours(-1), actor_type: "agent", actor: "spiffe://wardyn/agent", action: "tool.call", outcome: "success", ...partial } as AuditEvent;
}

function renderScreen() {
  return render(
    <MemoryRouter>
      <AuditScreen />
    </MemoryRouter>,
  );
}

describe("AuditScreen rows with a second line", () => {
  beforeEach(() => listAuditMock.mockReset());

  it("keeps the sentence-and-detail column's floor (#1878), as the plain row does", async () => {
    const id = "3f2a1c70-8e4b-4d19-a6f2-5b0c7de19a44";
    listAuditMock.mockResolvedValue([
      ev({ id: "e1", action: "egress.allow", target: "gateway.example.com", data: { gateway_call_id: id } }),
    ]);
    renderScreen();
    const column = (await screen.findByText("Allowed egress to gateway.example.com")).parentElement!;
    expect(column).toHaveClass("min-w-0", "flex-1", "sm:min-w-[10rem]");
  });

  // The call id a row went to a gateway under. Untrusted text: shown only as a UUID, only as text, and copied whole.
  it("shows the gateway call id on an egress row with a copy button, and a row without it looks as it did", async () => {
    const id = "3f2a1c70-8e4b-4d19-a6f2-5b0c7de19a44";
    listAuditMock.mockResolvedValue([
      ev({ id: "e1", action: "egress.allow", target: "gateway.example.com", data: { gateway_call_id: id } }),
      ev({ id: "e2", action: "egress.deny", target: "gateway.example.com", outcome: "denied", data: { gateway_call_id: id.replace("3f2a", "b81e") } }),
      ev({ id: "e3", action: "egress.allow", target: "registry.example.com", data: {} }),
      ev({ id: "e4", action: "egress.allow", target: "no-data.example.com" }),
      // Not a UUID, or on a row that is not an egress decision: no line, and the text is never parsed as markup.
      ev({ id: "e5", action: "egress.allow", target: "evil.example.com", data: { gateway_call_id: "<img src=x onerror=alert(1)>" } }),
      ev({ id: "e6", action: "run.create", data: { gateway_call_id: id } }),
    ]);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    // After setup(), which installs a clipboard of its own.
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    const { container } = renderScreen();
    expect(await screen.findByText(`Call id ${id}`)).toBeInTheDocument();
    expect(screen.getByText(`Call id ${id.replace("3f2a", "b81e")}`)).toBeInTheDocument();
    expect(screen.getAllByTestId("audit-call-id")).toHaveLength(2);
    expect(screen.getByText("Allowed egress to registry.example.com").parentElement?.children).toHaveLength(1);
    expect(screen.getByText("Allowed egress to no-data.example.com").parentElement?.children).toHaveLength(1);
    expect(screen.getByText("Allowed egress to evil.example.com").parentElement?.children).toHaveLength(1);
    expect(container.querySelector("img")).toBeNull();
    expect(screen.queryByText(/onerror/)).not.toBeInTheDocument();
    expect(screen.getByText("Created the run").parentElement?.children).toHaveLength(1);
    // Copy takes the whole id, never the "Call id" label.
    const [copy] = screen.getAllByRole("button", { name: "Copy call id" });
    expect(screen.getAllByRole("button", { name: "Copy call id" })).toHaveLength(2);
    await user.click(copy);
    expect(writeText).toHaveBeenCalledWith(id);
  });

  it("words a gateway token mint row, and files it under Credentials", async () => {
    listAuditMock.mockResolvedValue([
      ev({ id: "e1", actor_type: "agent", action: "gateway_identity.mint", target: "corp-gateway", data: { provider: "corp-gateway" } }),
      ev({ id: "e2", action: "gateway_identity.mint", target: "old-gateway", outcome: "denied", data: { reason: "identity_deactivated" } }),
      ev({ id: "e3", action: "run.create" }),
    ]);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen();
    expect(await screen.findByText("Signed a gateway token for corp-gateway")).toBeInTheDocument();
    expect(screen.getByText("Refused a gateway token for old-gateway")).toBeInTheDocument();
    expect(screen.queryByTestId("audit-call-id")).not.toBeInTheDocument();
    await user.click(screen.getByRole("combobox", { name: /event/i }));
    await user.click(await screen.findByRole("option", { name: /^Credentials$/i }));
    await waitFor(() => expect(screen.queryByText("Created the run")).not.toBeInTheDocument());
    expect(screen.getByText("Signed a gateway token for corp-gateway")).toBeInTheDocument();
  });
});
