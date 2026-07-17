/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { Workspace, WorkspaceProfile } from "../../lib/types";

// The "View profile" dialog body (WorkspaceNeedsPanel) turns the untrusted,
// content-derived scan profile into an operator-legible view. Two invariants under
// test: (1) declared secrets show as NAMES + advisory badges with the .env warning
// and NO value affordance anywhere; (2) approving a *suggested* egress host goes
// through a confirm dialog and PUTs the full approved list.

const setApprovedEgressMock = vi.fn();
const getObservedEgressMock = vi.fn();
vi.mock("../../lib/api/workspaces", () => ({
  workspaces: {
    setApprovedEgress: (...a: unknown[]) => setApprovedEgressMock(...a),
    getObservedEgress: (...a: unknown[]) => getObservedEgressMock(...a),
  },
}));
vi.mock("sonner", () => ({
  toast: { error: vi.fn(), success: vi.fn(), info: vi.fn() },
}));

import { WorkspaceNeedsPanel } from "./workspaces";

function ws(profile: WorkspaceProfile, over: Partial<Workspace> = {}): Workspace {
  return {
    id: "ws-1",
    name: "payments",
    kind: "local_dir",
    source: "/srv/payments",
    status: "ready",
    created_at: "",
    updated_at: "",
    profile: profile as unknown as Record<string, unknown>,
    ...over,
  };
}

describe("WorkspaceNeedsPanel — declared secrets (names only)", () => {
  const profile: WorkspaceProfile = {
    languages: ["TypeScript", "Go"],
    package_managers: ["pnpm"],
    tools: ["docker"],
    has_devcontainer: true,
    has_dockerfile: true,
    needs_review: true,
    required_secrets: [
      { name: "DATABASE_URL", kind: "postgres" },
      { name: "STRIPE_SECRET_KEY", kind: "stripe" },
      { name: "DEPLOY_TOKEN", kind: "deploy", optional: true },
    ],
    services_needed: ["postgres", "redis"],
    egress_domains: ["api.anthropic.com"],
    suggested_egress: ["telemetry.acme.io"],
    secret_files_present: [".env", "config/.env.local"],
  };

  it("renders secret NAMES + kind/optional badges, the provenance caveat, and the .env warning", () => {
    const { container } = render(<WorkspaceNeedsPanel workspace={ws(profile)} onWorkspaceUpdated={vi.fn()} />);

    // Names, in monospace, exactly as declared.
    expect(screen.getByText("DATABASE_URL")).toBeInTheDocument();
    expect(screen.getByText("STRIPE_SECRET_KEY")).toBeInTheDocument();
    expect(screen.getByText("DEPLOY_TOKEN")).toBeInTheDocument();

    // Kind + optional badges live inside the secrets section ("postgres" also appears
    // as a *service* chip, so scope the assertion to avoid the duplicate).
    const secrets = screen.getByTestId("ws-secrets");
    expect(within(secrets).getByText("postgres")).toBeInTheDocument();
    expect(within(secrets).getByText("stripe")).toBeInTheDocument();
    expect(within(secrets).getByText("deploy-time")).toBeInTheDocument();

    // Honest provenance + the never-reads-values caveat.
    expect(screen.getByText(/values are never read/i)).toBeInTheDocument();
    expect(screen.getByText(/low-confidence scan/i)).toBeInTheDocument();

    // .env warning: heading, the paths, and the readable-if-mounted copy.
    expect(screen.getByText("Secret files present")).toBeInTheDocument();
    expect(screen.getByText(".env")).toBeInTheDocument();
    expect(screen.getByText("config/.env.local")).toBeInTheDocument();
    expect(screen.getByText(/readable by the agent if this directory is mounted/i)).toBeInTheDocument();

    // Honesty footer.
    expect(screen.getByText(/files deeper than 4 levels are not visible/i)).toBeInTheDocument();

    // NO value affordance: the secrets section has no input/textarea, and nothing in
    // the panel looks like a secret VALUE (api key / connection string with password).
    expect(secrets.querySelector("input")).toBeNull();
    expect(secrets.querySelector("textarea")).toBeNull();
    expect(container.textContent).not.toMatch(/sk[-_]live[-_]|:\/\/[^@\s]+:[^@\s]+@/);
  });
});

describe("WorkspaceNeedsPanel — egress tiers + approve/remove", () => {
  beforeEach(() => setApprovedEgressMock.mockReset());

  const profile: WorkspaceProfile = {
    egress_domains: ["api.anthropic.com"], // allowed automatically
    suggested_egress: ["telemetry.acme.io"], // needs review
  };
  const workspace = ws(profile, { approved_egress: ["already.example.com"] });

  it("approving a suggested host confirms first, then PUTs the full approved list", async () => {
    const updated = { ...workspace, approved_egress: ["already.example.com", "telemetry.acme.io"] };
    setApprovedEgressMock.mockResolvedValue(updated);
    const onWorkspaceUpdated = vi.fn();
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    render(<WorkspaceNeedsPanel workspace={workspace} onWorkspaceUpdated={onWorkspaceUpdated} />);

    // The suggested row's "Approve" (exact) — not the confirm dialog's "Approve host".
    await user.click(screen.getByRole("button", { name: "Approve" }));

    // Confirm dialog states the untrusted-content caveat before anything is PUT.
    expect(await screen.findByText(/untrusted content/i)).toBeInTheDocument();
    expect(setApprovedEgressMock).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: /approve host/i }));

    await waitFor(() =>
      expect(setApprovedEgressMock).toHaveBeenCalledWith("ws-1", [
        "already.example.com",
        "telemetry.acme.io",
      ]),
    );
    await waitFor(() => expect(onWorkspaceUpdated).toHaveBeenCalledWith(updated));
  });

  it("removing an approved host PUTs the list minus that host (no confirm)", async () => {
    setApprovedEgressMock.mockResolvedValue({ ...workspace, approved_egress: [] });
    const onWorkspaceUpdated = vi.fn();
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    render(<WorkspaceNeedsPanel workspace={workspace} onWorkspaceUpdated={onWorkspaceUpdated} />);

    await user.click(screen.getByRole("button", { name: /remove/i }));
    await waitFor(() => expect(setApprovedEgressMock).toHaveBeenCalledWith("ws-1", []));
  });
});

describe("WorkspaceNeedsPanel — suspected committed secrets (content-free)", () => {
  it("lists path:line — kind, the never-shown-or-stored caveat, and NO value affordance", () => {
    const profile: WorkspaceProfile = {
      leak_findings: [
        { path: "config/settings.py", kind: "aws-access-key", line: 42 },
        { path: "deploy/creds", kind: "private-key-block" }, // no line
      ],
    };
    const { container } = render(<WorkspaceNeedsPanel workspace={ws(profile)} onWorkspaceUpdated={vi.fn()} />);

    const leaks = screen.getByTestId("ws-leaks");
    expect(within(leaks).getByText(/rotate\/remove before mounting/i)).toBeInTheDocument();
    // path:line and the detector id (kind), never a value.
    expect(within(leaks).getByText("config/settings.py:42")).toBeInTheDocument();
    expect(within(leaks).getByText(/aws-access-key/)).toBeInTheDocument();
    expect(within(leaks).getByText("deploy/creds")).toBeInTheDocument();
    expect(within(leaks).getByText(/private-key-block/)).toBeInTheDocument();
    // Copy makes the content-free provenance explicit.
    expect(within(leaks).getByText(/never shown or stored/i)).toBeInTheDocument();
    // No value affordance, and nothing that looks like a secret value.
    expect(leaks.querySelector("input")).toBeNull();
    expect(leaks.querySelector("textarea")).toBeNull();
    expect(container.textContent).not.toMatch(/sk[-_]live[-_]|AKIA[0-9A-Z]{16}|:\/\/[^@\s]+:[^@\s]+@/);
  });
});

describe("WorkspaceNeedsPanel — advisory secret provenance badges (code/ci)", () => {
  it("groups code/ci refs separately from declared secrets, with plain-language badges", () => {
    const profile: WorkspaceProfile = {
      required_secrets: [
        { name: "POSTGRES_PASSWORD", kind: "deploy", optional: true },
        { name: "SENTRY_DSN", kind: "code", optional: true },
        { name: "NPM_TOKEN", kind: "ci", optional: true },
      ],
    };
    render(<WorkspaceNeedsPanel workspace={ws(profile)} onWorkspaceUpdated={vi.fn()} />);

    // A real declared credential lives under "Secrets this workspace declares".
    const secrets = screen.getByTestId("ws-secrets");
    expect(within(secrets).getByText("POSTGRES_PASSWORD")).toBeInTheDocument();
    // Code/CI-only refs live under the separate advisory group with translated badges.
    const codeRefs = screen.getByTestId("ws-code-refs");
    expect(within(codeRefs).getByText("from source")).toBeInTheDocument();
    expect(within(codeRefs).getByText("CI-only")).toBeInTheDocument();
    // The raw detector ids are translated away; no generic "optional" chip piles on.
    expect(within(codeRefs).queryByText("code")).toBeNull();
    expect(within(codeRefs).queryByText("ci")).toBeNull();
    expect(within(codeRefs).queryByText("optional")).toBeNull();
    // A code/ci ref is NOT double-listed under the real-secrets section.
    expect(within(secrets).queryByText("SENTRY_DSN")).toBeNull();
  });
});

describe("WorkspaceNeedsPanel — observed-but-denied egress", () => {
  beforeEach(() => {
    setApprovedEgressMock.mockReset();
    getObservedEgressMock.mockReset();
  });

  it("fetches lazily on demand and approving a denied host PUTs it into the approved list", async () => {
    // No suggested/auto egress in the profile, so the ONLY "Approve" button in the
    // panel is the observed one (unambiguous getByRole below).
    const workspace = ws({}, { approved_egress: ["already.example.com"] });
    getObservedEgressMock.mockResolvedValue({ denied: ["metrics.acme.io"], runs_examined: 4 });
    const updated = { ...workspace, approved_egress: ["already.example.com", "metrics.acme.io"] };
    setApprovedEgressMock.mockResolvedValue(updated);
    const onWorkspaceUpdated = vi.fn();
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    render(<WorkspaceNeedsPanel workspace={workspace} onWorkspaceUpdated={onWorkspaceUpdated} />);

    // Lazy: nothing is fetched until the operator asks.
    expect(getObservedEgressMock).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: /check run history/i }));
    await waitFor(() => expect(getObservedEgressMock).toHaveBeenCalledWith("ws-1"));

    expect(await screen.findByText("metrics.acme.io")).toBeInTheDocument();
    expect(screen.getByText(/from 4 recent runs/i)).toBeInTheDocument();

    // Approve reuses the same confirm + setApprovedEgress flow the suggested tier uses.
    await user.click(screen.getByRole("button", { name: "Approve" }));
    expect(await screen.findByText(/untrusted content/i)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /approve host/i }));

    await waitFor(() =>
      expect(setApprovedEgressMock).toHaveBeenCalledWith("ws-1", [
        "already.example.com",
        "metrics.acme.io",
      ]),
    );
    await waitFor(() => expect(onWorkspaceUpdated).toHaveBeenCalledWith(updated));
  });

  it("shows a muted note when no denied egress was observed", async () => {
    getObservedEgressMock.mockResolvedValue({ denied: [], runs_examined: 3 });
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    render(<WorkspaceNeedsPanel workspace={ws({})} onWorkspaceUpdated={vi.fn()} />);
    await user.click(screen.getByRole("button", { name: /check run history/i }));

    expect(await screen.findByText(/no denied egress observed/i)).toBeInTheDocument();
    expect(setApprovedEgressMock).not.toHaveBeenCalled();
  });
});
