/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { Workspace, WorkspaceProfile } from "../../../lib/types";
import type { WorkspaceRequirementsMap } from "../../../lib/api/workspaces";
import { OperatorProvider } from "../../wardyn/operator-context";

const setRequirementsMock = vi.fn();
const setWorkspaceLLMCredMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: {
    setRequirements: (...a: unknown[]) => setRequirementsMock(...a),
    setWorkspaceLLMCred: (...a: unknown[]) => setWorkspaceLLMCredMock(...a),
  },
}));
const listSecretsMock = vi.fn().mockResolvedValue([]);
vi.mock("../../../lib/api/secrets", () => ({
  secrets: { listSecrets: (...a: unknown[]) => listSecretsMock(...a) },
}));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn(), info: vi.fn() } }));
// serverId is required: LLMCredFields (workspace-llm-cred.tsx) skips any row
// without one — a deliberate filter mirrored from step-access.tsx's picker,
// since a client display id with no server-side identity would silently fail
// to bind server-side. A real deriveAiRows "Managed subscription" row always
// carries one (aiServerId("anthropic_subscription", false)).
const listIntegrationsMock = vi.fn().mockResolvedValue({
  ai: [{ id: "ai-managed", serverId: "ai-managed", name: "Managed subscription", typeLabel: "anthropic · managed login" }],
  scm: [],
});
vi.mock("../../../lib/api/integrations", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/integrations")>(
    "../../../lib/api/integrations",
  );
  return { ...actual, integrationsApi: { list: (...a: unknown[]) => listIntegrationsMock(...a) } };
});

import { RequirementsCard } from "./requirements-card";
import { RD2 } from "../../../lib/workspace-copy";

// Radix Tabs activates a trigger on mousedown (not click) — fireEvent.click
// alone never fires that, so switching tabs needs real userEvent.
async function openTab(name: string) {
  const user = userEvent.setup({ pointerEventsCheck: 0 });
  await user.click(screen.getByRole("tab", { name }));
}

function ws(over: Partial<Workspace> = {}): Workspace {
  return {
    id: "ws-1",
    name: "payments",
    kind: "repo",
    source: "acme/payments",
    ref: "main",
    status: "scanned",
    created_at: "",
    updated_at: "",
    ...over,
  };
}

beforeEach(() => {
  setRequirementsMock.mockReset();
  setWorkspaceLLMCredMock.mockReset();
});

describe("RequirementsCard — model access is the first group", () => {
  it("shows 'Inherits global' style label when nothing is bound, before any contract group", () => {
    render(
      <RequirementsCard ws={ws()} storedSecretNames={[]} onWorkspaceUpdated={vi.fn()} onSecretStored={vi.fn()} />,
    );
    const heading = screen.getByText("Model access");
    expect(heading).toBeInTheDocument();
    expect(screen.getByText("None")).toBeInTheDocument();
    // The model-access group's own heading precedes the contract group headings
    // in document order (jsdom keeps source order; compareDocumentPosition
    // confirms it rather than assuming array order).
    const secretsHeading = screen.queryByText("Secrets");
    if (secretsHeading) {
      expect(heading.compareDocumentPosition(secretsHeading) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    }
  });

  it("opens the model access dialog and reports the saved workspace", async () => {
    const onWorkspaceUpdated = vi.fn();
    setWorkspaceLLMCredMock.mockResolvedValue(ws({ llm_cred: { integration_ref: "ai-managed" } }));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(
      <RequirementsCard ws={ws()} storedSecretNames={[]} onWorkspaceUpdated={onWorkspaceUpdated} onSecretStored={vi.fn()} />,
    );
    await user.click(screen.getByRole("button", { name: /bind model access/i }));
    await user.click(await screen.findByRole("radio", { name: /managed subscription/i }));
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() =>
      expect(setWorkspaceLLMCredMock).toHaveBeenCalledWith("ws-1", { integration_ref: "ai-managed" }),
    );
    await waitFor(() => expect(onWorkspaceUpdated).toHaveBeenCalled());
  });
});

describe("RequirementsCard — reuses the wizard's StepRequirements and persists edits immediately", () => {
  const profile: WorkspaceProfile = {
    required_secrets: [{ name: "DATABASE_URL", kind: "postgres" }],
    egress_domains: ["registry.npmjs.org"],
  };

  it("renders the same contract groups the wizard renders", async () => {
    render(
      <RequirementsCard
        ws={ws({ profile: profile as unknown as Record<string, unknown> })}
        storedSecretNames={[]}
        onWorkspaceUpdated={vi.fn()}
        onSecretStored={vi.fn()}
      />,
    );
    // The Requirements step opens on Reach now (dependency order — the
    // record-last redesign); Secrets needs switching to.
    expect(screen.getByTestId("group-reach")).toBeInTheDocument();
    expect(screen.getByText("registry.npmjs.org")).toBeInTheDocument();
    await openTab("Secrets");
    expect(screen.getByTestId("group-secrets")).toBeInTheDocument();
    expect(screen.getByText("DATABASE_URL")).toBeInTheDocument();
  });

  it("flipping a lane calls setRequirements immediately (no separate save step)", async () => {
    const onWorkspaceUpdated = vi.fn();
    const updated = ws({ profile: profile as unknown as Record<string, unknown> });
    setRequirementsMock.mockResolvedValue(updated);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(
      <RequirementsCard
        ws={ws({ profile: profile as unknown as Record<string, unknown> })}
        storedSecretNames={["database-url"]}
        onWorkspaceUpdated={onWorkspaceUpdated}
        onSecretStored={vi.fn()}
      />,
    );
    await user.click(screen.getByRole("tab", { name: "Secrets" }));
    const secretsGroup = within(screen.getByTestId("group-secrets"));
    await user.click(secretsGroup.getByRole("radio", { name: "Optional" }));
    await waitFor(() =>
      expect(setRequirementsMock).toHaveBeenCalledWith(
        "ws-1",
        expect.objectContaining({
          // Keyed by the STORABLE name (the server grammar's shape), while the
          // row still displays the detected env-var name.
          "secret:database-url": { level: "optional", provenance: "operator_set" },
        }),
      ),
    );
    await waitFor(() => expect(onWorkspaceUpdated).toHaveBeenCalledWith(updated));
  });

  // UI-WS-6: a row declared only via a shared source's own contract (the
  // FOLD) — with an empty overlay on THIS workspace — used to fall back to
  // requirements[key]?.level ?? "required" and render Required, disagreeing
  // with the header's own Needs-you line (computed from the same fold).
  it("displays a level from the EFFECTIVE fold, not just this workspace's own (empty) overlay", async () => {
    const w = ws({
      profile: profile as unknown as Record<string, unknown>,
      requirements: {},
      effective_requirements: {
        "secret:database-url": { level: "optional", provenance: "scan_seeded" },
        "egress:registry.npmjs.org": { level: "required", provenance: "scan_seeded" },
      },
    });
    render(
      <RequirementsCard ws={w} storedSecretNames={["database-url"]} onWorkspaceUpdated={vi.fn()} onSecretStored={vi.fn()} />,
    );
    await openTab("Secrets");
    expect(
      within(screen.getByTestId("group-secrets")).getByRole("radio", { name: "Optional" }),
    ).toHaveAttribute("aria-checked", "true");
  });

  // UI-WS-7's sibling on this surface: `pending` is seeded from the fold, so
  // toggling ONE lane must not bake every OTHER, untouched fold-only
  // (scan_seeded) row into the overlay PUT — that would freeze it past the
  // rescan meant to refresh it, exactly the wizard-side bug UI-WS-7 closes.
  it("touching one lane does not write an untouched scan_seeded fold row into the overlay PUT", async () => {
    const w = ws({
      profile: profile as unknown as Record<string, unknown>,
      requirements: {},
      effective_requirements: {
        "secret:database-url": { level: "required", provenance: "scan_seeded" },
        "egress:registry.npmjs.org": { level: "required", provenance: "scan_seeded" },
      },
    });
    setRequirementsMock.mockResolvedValue(w);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(
      <RequirementsCard ws={w} storedSecretNames={["database-url"]} onWorkspaceUpdated={vi.fn()} onSecretStored={vi.fn()} />,
    );

    // Touch ONLY the Reach (egress) lane — Secrets stays untouched.
    const reachGroup = within(screen.getByTestId("group-reach"));
    await user.click(reachGroup.getByRole("radio", { name: "Optional" }));

    await waitFor(() => expect(setRequirementsMock).toHaveBeenCalledTimes(1));
    const body = setRequirementsMock.mock.calls[0][1] as WorkspaceRequirementsMap;
    expect(body["egress:registry.npmjs.org"]).toEqual({ level: "optional", provenance: "operator_set" });
    // The untouched secret row never rides along into the overlay.
    expect(body["secret:database-url"]).toBeUndefined();
  });

  // The race PUT-per-toggle used to lose: deriving each write straight from
  // the `ws` prop (which only updates once the PREVIOUS PUT resolves) means
  // two toggles fired before that round trip lands both read the same stale
  // base, and the second's write silently drops the first. Local `pending`
  // state composes them instead.
  it("two toggles fired before the first PUT resolves both land in the SECOND write (composed, not the stale ws prop)", async () => {
    let resolveFirst!: (w: Workspace) => void;
    setRequirementsMock.mockImplementationOnce(
      () => new Promise<Workspace>((resolve) => { resolveFirst = resolve; }),
    );
    setRequirementsMock.mockResolvedValueOnce(ws({ profile: profile as unknown as Record<string, unknown> }));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(
      <RequirementsCard
        ws={ws({ profile: profile as unknown as Record<string, unknown> })}
        storedSecretNames={["database-url"]}
        onWorkspaceUpdated={vi.fn()}
        onSecretStored={vi.fn()}
      />,
    );

    // Toggle 1: Reach (the default tab) — flips the egress lane.
    const reachGroup = within(screen.getByTestId("group-reach"));
    await user.click(reachGroup.getByRole("radio", { name: "Optional" }));
    await waitFor(() => expect(setRequirementsMock).toHaveBeenCalledTimes(1));

    // Toggle 2, fired while call 1 is still in flight: Secrets.
    await user.click(screen.getByRole("tab", { name: "Secrets" }));
    const secretsGroup = within(screen.getByTestId("group-secrets"));
    await user.click(secretsGroup.getByRole("radio", { name: "Optional" }));
    await waitFor(() => expect(setRequirementsMock).toHaveBeenCalledTimes(2));

    resolveFirst(ws({ profile: profile as unknown as Record<string, unknown> }));

    // The SECOND call's body carries BOTH edits.
    const secondBody = setRequirementsMock.mock.calls[1][1] as WorkspaceRequirementsMap;
    expect(secondBody["egress:registry.npmjs.org"]).toEqual({ level: "optional", provenance: "operator_set" });
    expect(secondBody["secret:database-url"]).toEqual({ level: "optional", provenance: "operator_set" });
  });

  // A rejected PUT used to leave the optimistic `pending` map untouched — the
  // toggle stayed rendered checked forever, and every later toggle composed
  // onto (and re-sent) that phantom lane.
  it("a rejected PUT reverts pending — the toggle un-checks, and the next write doesn't resurrect it", async () => {
    const onWorkspaceUpdated = vi.fn();
    setRequirementsMock.mockRejectedValueOnce(new Error("409 conflict"));
    setRequirementsMock.mockResolvedValueOnce(ws({ profile: profile as unknown as Record<string, unknown> }));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(
      <RequirementsCard
        ws={ws({ profile: profile as unknown as Record<string, unknown> })}
        storedSecretNames={["database-url"]}
        onWorkspaceUpdated={onWorkspaceUpdated}
        onSecretStored={vi.fn()}
      />,
    );

    // Toggle 1 (Reach, the default tab) — rejected.
    const reachGroup = within(screen.getByTestId("group-reach"));
    await user.click(reachGroup.getByRole("radio", { name: "Optional" }));
    await waitFor(() => expect(setRequirementsMock).toHaveBeenCalledTimes(1));
    // Reverted once the rejection lands — not left phantom-checked.
    await waitFor(() => expect(reachGroup.getByRole("radio", { name: "Required" })).toHaveAttribute("aria-checked", "true"));
    expect(reachGroup.getByRole("radio", { name: "Optional" })).toHaveAttribute("aria-checked", "false");

    // Toggle 2 (Secrets) — succeeds.
    await user.click(screen.getByRole("tab", { name: "Secrets" }));
    const secretsGroup = within(screen.getByTestId("group-secrets"));
    await user.click(secretsGroup.getByRole("radio", { name: "Optional" }));
    await waitFor(() => expect(setRequirementsMock).toHaveBeenCalledTimes(2));

    // The second write carries ONLY the secrets edit — the reverted (failed)
    // Reach edit never resurrects itself onto a later write.
    const secondBody = setRequirementsMock.mock.calls[1][1] as WorkspaceRequirementsMap;
    expect(secondBody["secret:database-url"]).toEqual({ level: "optional", provenance: "operator_set" });
    expect(secondBody["egress:registry.npmjs.org"]).toBeUndefined();
  });
});

// The card is never remounted on an out-of-band refresh (workspace-detail.tsx
// gives it no `key`) — including the Edit-workspace overlay's onClose, which
// reloads with load(false) and can NULL the requirements contract server-side
// (sourcesChanged, internal/api/workspaces.go). Without reconciliation the
// card would go on rendering — and PUTting back — a contract the server no
// longer has.
describe("RequirementsCard — pending reconciles with the server", () => {
  const profile: WorkspaceProfile = {
    required_secrets: [{ name: "DATABASE_URL", kind: "postgres" }],
  };

  it("re-seeds pending when ws's requirements change out from under the (not remounted) card", async () => {
    const withContract = ws({
      profile: profile as unknown as Record<string, unknown>,
      requirements: { "secret:database-url": { level: "optional", provenance: "operator_set" } },
      updated_at: "2025-01-01T00:00:00.000000Z",
    });
    const { rerender } = render(
      <RequirementsCard ws={withContract} storedSecretNames={["database-url"]} onWorkspaceUpdated={vi.fn()} onSecretStored={vi.fn()} />,
    );
    await openTab("Secrets");
    await waitFor(() =>
      expect(within(screen.getByTestId("group-secrets")).getByRole("radio", { name: "Optional" })).toHaveAttribute(
        "aria-checked",
        "true",
      ),
    );

    // Same component instance (no remount) — a fresh `ws` prop with the
    // contract reset to empty and a NEWER updated_at, exactly what load(false)
    // hands back after an Edit-workspace close that changed sources (the
    // server bumps updated_at on every write, the reset included) — this must
    // win over the locally-pending contract.
    const resetWs = ws({
      profile: profile as unknown as Record<string, unknown>,
      requirements: {},
      updated_at: "2025-01-01T00:00:05.000000Z",
    });
    rerender(
      <RequirementsCard ws={resetWs} storedSecretNames={["database-url"]} onWorkspaceUpdated={vi.fn()} onSecretStored={vi.fn()} />,
    );

    await waitFor(() =>
      expect(within(screen.getByTestId("group-secrets")).getByRole("radio", { name: "Required" })).toHaveAttribute(
        "aria-checked",
        "true",
      ),
    );
  });

  // reconcile-wave5.md's medium finding: the `saving` guard only covers the
  // write's OWN round trip. workspace-detail.tsx polls every 2.5s while
  // scanning/recording, so a GET issued BEFORE a PUT can still resolve AFTER
  // it, carrying a pre-write snapshot — `saving` has already gone false by
  // then. Without the updated_at guard, that stale snapshot reverts `pending`
  // right back, and the operator's NEXT toggle then PUTs a full-replace map
  // that silently drops the change that had actually landed.
  it("a stale poll snapshot (older updated_at) landing after a successful write does not revert pending", async () => {
    const before = ws({
      profile: profile as unknown as Record<string, unknown>,
      requirements: {},
      updated_at: "2025-01-01T00:00:00.000000Z",
    });
    const afterWrite = ws({
      profile: profile as unknown as Record<string, unknown>,
      requirements: { "secret:database-url": { level: "optional", provenance: "operator_set" } },
      updated_at: "2025-01-01T00:00:05.000000Z",
    });
    setRequirementsMock.mockResolvedValueOnce(afterWrite);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const { rerender } = render(
      <RequirementsCard ws={before} storedSecretNames={["database-url"]} onWorkspaceUpdated={vi.fn()} onSecretStored={vi.fn()} />,
    );
    await openTab("Secrets");
    await user.click(within(screen.getByTestId("group-secrets")).getByRole("radio", { name: "Optional" }));
    await waitFor(() => expect(setRequirementsMock).toHaveBeenCalledTimes(1));

    // The write lands — the parent's `ws` prop advances too (onWorkspaceUpdated -> setWs).
    rerender(
      <RequirementsCard ws={afterWrite} storedSecretNames={["database-url"]} onWorkspaceUpdated={vi.fn()} onSecretStored={vi.fn()} />,
    );
    await waitFor(() =>
      expect(within(screen.getByTestId("group-secrets")).getByRole("radio", { name: "Optional" })).toHaveAttribute(
        "aria-checked",
        "true",
      ),
    );

    // A GET that was already in flight before the toggle resolves now, with
    // the PRE-write snapshot: same (older) updated_at, reverted requirements.
    rerender(
      <RequirementsCard ws={before} storedSecretNames={["database-url"]} onWorkspaceUpdated={vi.fn()} onSecretStored={vi.fn()} />,
    );

    // pending must NOT revert — the toggle stays Optional.
    await waitFor(() =>
      expect(within(screen.getByTestId("group-secrets")).getByRole("radio", { name: "Optional" })).toHaveAttribute(
        "aria-checked",
        "true",
      ),
    );
  });
});

describe("RequirementsCard — a viewer's lane toggles and Bind-model-access are disabled", () => {
  it("disables the Reach lane radio and Bind model access for a viewer", () => {
    const profile: WorkspaceProfile = { egress_domains: ["registry.npmjs.org"] };
    render(
      <OperatorProvider operator={false}>
        <RequirementsCard
          ws={ws({ profile: profile as unknown as Record<string, unknown> })}
          storedSecretNames={[]}
          onWorkspaceUpdated={vi.fn()}
          onSecretStored={vi.fn()}
        />
      </OperatorProvider>,
    );
    expect(screen.getByRole("button", { name: /bind model access/i })).toBeDisabled();
    const reachGroup = within(screen.getByTestId("group-reach"));
    expect(reachGroup.getByRole("radio", { name: "Optional" })).toBeDisabled();
  });
});

describe("RequirementsCard — Reach/Record reflect the REAL llm_cred binding, not the wizard's PowerSource", () => {
  const profile: WorkspaceProfile = {
    required_secrets: [{ name: "DATABASE_URL", kind: "postgres" }],
    egress_domains: ["registry.npmjs.org"],
  };

  it("treats an unbound workspace as the server default — Record a session stays enabled", async () => {
    render(
      <RequirementsCard
        ws={ws({ profile: profile as unknown as Record<string, unknown> })}
        storedSecretNames={[]}
        onWorkspaceUpdated={vi.fn()}
        onSecretStored={vi.fn()}
      />,
    );
    // Reach's power card states the resolution up front…
    expect(within(screen.getByTestId("power-source-card")).getByText("server default")).toBeInTheDocument();
    // …and Record (last tab) offers the agent session.
    await openTab("Verify");
    expect(screen.getByRole("button", { name: "Verify with a session" })).toBeEnabled();
    expect(screen.queryByText(RD2.RECORD_NEEDS)).not.toBeInTheDocument();
  });

  it("Reach seeds the 'from model access' row from a pinned Integration binding", async () => {
    render(
      <RequirementsCard
        ws={ws({
          profile: profile as unknown as Record<string, unknown>,
          llm_cred: { integration_ref: "ai-acme-key" },
        })}
        storedSecretNames={[]}
        onWorkspaceUpdated={vi.fn()}
        onSecretStored={vi.fn()}
      />,
    );
    const egressGroup = within(screen.getByTestId("group-reach"));
    const chip = egressGroup.getByText("from model access");
    expect(chip).toHaveAttribute("title", RD2.EGRESS_TIP);
    // A pinned binding is named plainly by its Integration ref rather than a
    // fabricated hostname — see step-requirements.tsx's ResolvedToken. (The
    // SAME label also appears in the Model access chip above — scope to the
    // egress group so the two don't collide.)
    expect(egressGroup.getByText("ai-acme-key")).toBeInTheDocument();
  });
});
