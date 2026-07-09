/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { ApprovalRequest } from "../../lib/types";

// H7 fix: requested_scope never carries a real GrantKind (ApprovalRequest.kind
// is only the wire-level "credential"/"egress_domain"/"tool_call"), so
// credentialKind() in approvals.tsx key-sniffs the scope shape. Pin one
// approval per real grant shape so an api_key or ssh_key scope never again
// renders the git_pat "handed to git / can't expire a PAT" banner.

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

let pending: ApprovalRequest[] = [];
vi.mock("../../lib/api", () => ({
  api: {
    listApprovals: (state: string) => Promise.resolve(state === "PENDING" ? pending : []),
    // RunContextRow (redesign) fetches the gated run to inline its context.
    getRun: () =>
      Promise.resolve({
        id: "run_1",
        agent: "claude-code",
        repo: "acme/widgets",
        task: "Fix flaky auth tests",
        confinement_class: "CC2",
        state: "RUNNING",
      }),
  },
}));

import { ApprovalsScreen } from "./approvals";

function renderScreen() {
  return render(
    <MemoryRouter>
      <ApprovalsScreen />
    </MemoryRouter>,
  );
}

const base = {
  id: "apr_1",
  run_id: "run_1",
  kind: "credential" as const,
  state: "PENDING" as const,
  requested_at: new Date().toISOString(),
};

describe("ApprovalsScreen — credentialKind banner per grant shape", () => {
  it("renders the api_key banner for a real api_key scope, never git_pat's", async () => {
    pending = [
      {
        ...base,
        requested_scope: { host: "api.anthropic.com", header: "x-api-key", format: "%s", secret_name: "llm-key" },
      },
    ];
    renderScreen();
    await screen.findByText(/injected proxy-side/i);
    expect(screen.queryByText(/grants git write/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/handed to git inside the sandbox/i)).not.toBeInTheDocument();
  });

  it("renders a distinct ssh_key banner, never git_pat's PAT wording", async () => {
    pending = [
      { ...base, requested_scope: { host: "github.com", key_secret_ref: "ssh-key-github.com" } },
    ];
    renderScreen();
    // "written to disk" appears in both the "what" and "blast" lines by design
    // (both reuse the same honest wording) — match the host-qualified "what"
    // line specifically so a single element is found.
    await screen.findByText(/ssh key for github\.com is written to disk/i);
    expect(screen.queryByText(/handed to git inside the sandbox/i)).not.toBeInTheDocument();
    // ssh_key is a resident, agent-readable, non-downscopable key like a PAT —
    // it still earns the "grants git write" capability chip.
    expect(screen.getByText(/grants git write/i)).toBeInTheDocument();
  });

  it("renders the git_pat banner for an actual PAT scope", async () => {
    pending = [
      {
        ...base,
        requested_scope: { host: "gitlab.example.com", secret_name: "gitlab-pat", username: "oauth2" },
      },
    ];
    renderScreen();
    // Same duplication note as ssh_key above — match the host-qualified "what"
    // line specifically.
    await screen.findByText(/token for gitlab\.example\.com is handed to git/i);
    expect(screen.getByText(/grants git write/i)).toBeInTheDocument();
  });

  it("renders the github_token banner with a write chip when permissions grant write", async () => {
    pending = [
      {
        ...base,
        requested_scope: { repos: ["acme/widgets"], permissions: { contents: "write" } },
      },
    ];
    renderScreen();
    await screen.findByText(/short-lived GitHub token/i);
    expect(screen.getByText(/^grants write$/i)).toBeInTheDocument();
  });
});
