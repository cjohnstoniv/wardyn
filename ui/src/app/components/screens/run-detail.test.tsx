/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// ConnectSSHCard (prompt-v3-ssh-pane.md): owner-only, running-only,
// gateway-enabled-only visibility, and the no-keys-yet lead-in. Standalone
// (ConnectSSHCard is exported for exactly this) rather than mounting the
// whole RunDetailScreen's fetch graph.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { AgentRun } from "../../lib/types";

const healthMock = vi.fn();
vi.mock("../../lib/api/health", () => ({
  health: { health: (...a: unknown[]) => healthMock(...a) },
}));

const listKeysMock = vi.fn();
vi.mock("../../lib/api/ssh-keys", () => ({
  sshKeys: { listKeys: (...a: unknown[]) => listKeysMock(...a) },
}));

import { ConnectSSHCard } from "./run-detail";
import { OperatorProvider } from "../wardyn/operator-context";

const OWNER = "alice@example.com";
const baseRun: AgentRun = {
  id: "11111111-1111-1111-1111-111111111111",
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
  created_by: OWNER,
  agent: "claude-code",
  repo: "acme/widgets",
  task: "do the thing",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://wardyn.local/agent-run/11111111-1111-1111-1111-111111111111",
  runner_target: "docker",
  interactive: false,
};

function renderCard(run: Partial<AgentRun> = {}, principal = OWNER) {
  return render(
    <MemoryRouter>
      <OperatorProvider operator principal={principal}>
        <ConnectSSHCard run={{ ...baseRun, ...run }} />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  healthMock.mockReset();
  listKeysMock.mockReset();
});

describe("ConnectSSHCard — visibility", () => {
  it("renders nothing for a run the caller does not own", async () => {
    healthMock.mockResolvedValue({ ssh: { enabled: true, advertise_addr: "wardyn.corp.example:2222" } });
    listKeysMock.mockResolvedValue([{ fingerprint: "SHA256:x", principal: OWNER, name: "k", public_key: "", created_at: "" }]);
    const { container } = renderCard({}, "mallory@example.com");
    // Give any (unexpected) fetch a tick to resolve before asserting absence.
    await new Promise((r) => setTimeout(r, 0));
    expect(container.querySelector("section")).toBeNull();
    expect(healthMock).not.toHaveBeenCalled();
  });

  it("renders nothing for a stopped run, even when owned", async () => {
    healthMock.mockResolvedValue({ ssh: { enabled: true } });
    const { container } = renderCard({ state: "COMPLETED" });
    await new Promise((r) => setTimeout(r, 0));
    expect(container.querySelector("section")).toBeNull();
    expect(healthMock).not.toHaveBeenCalled();
  });

  it("renders nothing when the deployment has SSH disabled (healthz omits/falses it)", async () => {
    healthMock.mockResolvedValue({});
    listKeysMock.mockResolvedValue([]);
    renderCard();
    await waitFor(() => expect(healthMock).toHaveBeenCalled());
    expect(screen.queryByText("Connect via SSH")).toBeNull();
  });
});

describe("ConnectSSHCard — content", () => {
  it("shows the full card (command, ssh config, fingerprint, VS Code disclosure) when a key is registered", async () => {
    healthMock.mockResolvedValue({
      ssh: { enabled: true, advertise_addr: "wardyn.corp.example:2222", host_key_fingerprint: "SHA256:abc123" },
    });
    listKeysMock.mockResolvedValue([
      { fingerprint: "SHA256:x", principal: OWNER, name: "laptop", public_key: "", created_at: "2026-01-01T00:00:00Z" },
    ]);
    renderCard();

    await screen.findByText("Connect via SSH");
    expect(screen.getByText(`ssh ${baseRun.id}@wardyn.corp.example -p 2222`)).toBeInTheDocument();
    expect(screen.getByText(/ED25519 SHA256:abc123/)).toBeInTheDocument();
    expect(screen.getByText("ssh config")).toBeInTheDocument();
    expect(screen.getByText("VS Code Remote-SSH")).toBeInTheDocument();
    // No-keys lead-in must NOT show once a key exists.
    expect(screen.queryByText("Add your SSH key first")).toBeNull();
  });

  it("leads with 'Add your SSH key first' when the caller has no keys, but still shows the real command", async () => {
    healthMock.mockResolvedValue({ ssh: { enabled: true, advertise_addr: "wardyn.corp.example:2222" } });
    listKeysMock.mockResolvedValue([]);
    renderCard();

    await screen.findByText("Add your SSH key first");
    expect(screen.getByText(`ssh ${baseRun.id}@wardyn.corp.example -p 2222`)).toBeInTheDocument();
    // Two "Manage SSH keys" affordances render in this state (the lead-in's
    // own CTA + the card's standing footer link) — both must point at the
    // keys screen.
    const links = screen.getAllByRole("link", { name: /manage ssh keys/i });
    expect(links.length).toBeGreaterThanOrEqual(1);
    for (const link of links) {
      expect(link).toHaveAttribute("href", "/ssh-keys");
    }
  });

  it("omits the -p flag when advertise_addr carries no port", async () => {
    healthMock.mockResolvedValue({ ssh: { enabled: true, advertise_addr: "wardyn.corp.example" } });
    listKeysMock.mockResolvedValue([{ fingerprint: "SHA256:x", principal: OWNER, name: "k", public_key: "", created_at: "" }]);
    renderCard();

    await screen.findByText("Connect via SSH");
    expect(screen.getByText(`ssh ${baseRun.id}@wardyn.corp.example`)).toBeInTheDocument();
  });
});
