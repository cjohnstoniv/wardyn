/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { Workspace, WorkspaceProfile } from "../../../lib/types";

const getEnvAsCodeMock = vi.fn();
const writeEnvAsCodeMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: {
    getEnvAsCode: (...a: unknown[]) => getEnvAsCodeMock(...a),
    writeEnvAsCode: (...a: unknown[]) => writeEnvAsCodeMock(...a),
  },
}));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn(), info: vi.fn() } }));

import { EnvAsCodeCard } from "./env-as-code-card";

function ws(over: Partial<Workspace> = {}, profile: WorkspaceProfile | null = {}): Workspace {
  return {
    id: "ws-1",
    name: "payments",
    kind: "local_dir",
    source: "/srv/payments",
    status: "scanned",
    created_at: "",
    updated_at: "",
    profile: profile as unknown as Record<string, unknown>,
    ...over,
  };
}

beforeEach(() => {
  getEnvAsCodeMock.mockReset();
  writeEnvAsCodeMock.mockReset();
});

describe("EnvAsCodeCard — generate", () => {
  it("fetches and renders the generated files with the regenerate caveat", async () => {
    getEnvAsCodeMock.mockResolvedValue({ "AGENTS.md": "# env" });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<EnvAsCodeCard ws={ws()} />);

    await user.click(screen.getByRole("button", { name: /generate files/i }));
    expect(await screen.findByText("AGENTS.md")).toBeInTheDocument();
    expect(screen.getByText("# env")).toBeInTheDocument();
    expect(getEnvAsCodeMock).toHaveBeenCalledWith("ws-1");
    expect(screen.getByText(/regenerate after a rescan/i)).toBeInTheDocument();
  });

  it("disables Generate before a scan has produced a profile", () => {
    render(<EnvAsCodeCard ws={ws({}, null)} />);
    expect(screen.getByRole("button", { name: /generate files/i })).toBeDisabled();
    expect(screen.getByText(/scan first/i)).toBeInTheDocument();
  });
});

describe("EnvAsCodeCard — write into the directory (local_dir only)", () => {
  it("offers Write into the directory for a local_dir workspace and calls the write endpoint", async () => {
    getEnvAsCodeMock.mockResolvedValue({ "AGENTS.md": "# env" });
    writeEnvAsCodeMock.mockResolvedValue({ "AGENTS.md": "# env" });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<EnvAsCodeCard ws={ws({ kind: "local_dir" })} />);

    await user.click(screen.getByRole("button", { name: /generate files/i }));
    await screen.findByText("AGENTS.md");
    await user.click(screen.getByRole("button", { name: /write into the directory/i }));
    await waitFor(() => expect(writeEnvAsCodeMock).toHaveBeenCalledWith("ws-1"));
    expect(await screen.findByText("written")).toBeInTheDocument();
  });

  it("omits Write into the directory for a repo-only workspace, saying copy-and-commit instead", async () => {
    getEnvAsCodeMock.mockResolvedValue({ "AGENTS.md": "# env" });
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<EnvAsCodeCard ws={ws({ kind: "repo", source: "acme/payments" })} />);

    await user.click(screen.getByRole("button", { name: /generate files/i }));
    await screen.findByText("AGENTS.md");
    expect(screen.queryByRole("button", { name: /write into the directory/i })).not.toBeInTheDocument();
    expect(screen.getByText(/copy these and commit them/i)).toBeInTheDocument();
    expect(writeEnvAsCodeMock).not.toHaveBeenCalled();
  });

  it("offers Write into the directory when a composed workspace has any local_dir source", async () => {
    getEnvAsCodeMock.mockResolvedValue({ "AGENTS.md": "# env" });
    const composed = ws({ kind: "repo", source: "acme/payments" }) as unknown as Workspace & {
      sources: { type: string }[];
    };
    composed.sources = [{ type: "repo" }, { type: "local_dir" }];
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<EnvAsCodeCard ws={composed} />);

    await user.click(screen.getByRole("button", { name: /generate files/i }));
    await screen.findByText("AGENTS.md");
    expect(screen.getByRole("button", { name: /write into the directory/i })).toBeInTheDocument();
  });
});
