/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "What this type gets" against GET /permissions/explain's wire (#739): each
// state as packet A draws it, a restricted value's rows (with no grant, on
// any kind, image included), the audience after "only", the wall note, Remove
// on this type's own rows, value names, and a refused read.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

// F2: a refused add shows the server's sentence inside the dialog, never a
// toast (permissions.test.tsx's own pattern for the same spy).
const toastErrorMock = vi.fn();
vi.mock("sonner", () => ({
  toast: { error: (...a: unknown[]) => toastErrorMock(...a), success: vi.fn(), warning: vi.fn() },
}));

const explainMock = vi.fn();
const getPermissionsMock = vi.fn();
const deleteGrantMock = vi.fn();
const upsertGrantMock = vi.fn();
vi.mock("../../../lib/api/permissions", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/permissions")>("../../../lib/api/permissions");
  return {
    ...actual,
    permissions: {
      explainCapabilities: (...a: unknown[]) => explainMock(...a),
      getPermissions: () => getPermissionsMock(),
      deleteGrant: (id: string) => deleteGrantMock(id),
      upsertGrant: (...a: unknown[]) => upsertGrantMock(...a),
    },
  };
});
vi.mock("../../../lib/api/user-types", () => ({
  userTypes: {
    listUserTypes: async () => [
      { id: "developer", name: "Developer" },
      { id: "portfolio-manager", name: "Portfolio manager" },
    ],
  },
}));
const listWorkspacesMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({ workspaces: { listWorkspaces: () => listWorkspacesMock() } }));
vi.mock("../../../lib/api/policies", () => ({
  policies: { listPolicies: async () => [{ id: "pol-1", name: "Read-only research" }] },
}));
const getModelProvidersMock = vi.fn();
vi.mock("../../../lib/api/model-providers", () => ({
  modelProviders: { getModelProviders: () => getModelProvidersMock() },
}));

import { HttpError } from "../../../lib/api/core";
import type { ExplainRow } from "../../../lib/api/permissions";
import { KIND, PERM } from "../../../lib/permissions-copy";
import type { CapabilityGrant } from "../../../lib/types";
import { EXPLAIN, USER_TYPES as UT } from "../../../lib/user-types-copy";
import { OperatorProvider } from "../../wardyn/operator-context";
import { ExplainGrid } from "./explain-grid";

const PM = "portfolio-manager";

function grant(over: Partial<CapabilityGrant>): CapabilityGrant {
  return {
    id: "g-1",
    subject_type: "user_type",
    subject: PM,
    capability: "agent",
    value: "codex",
    effect: "deny",
    created_at: "",
    created_by: "",
    ...over,
  } as CapabilityGrant;
}

function answer(rows: ExplainRow[], grants: CapabilityGrant[] = []) {
  explainMock.mockResolvedValue({ subject_type: "user_type", subject: PM, kinds_version: 3, rows });
  getPermissionsMock.mockResolvedValue({ grants, enforcement: {} });
}

function renderGrid(operator = true) {
  return render(
    <OperatorProvider operator={operator} securityOperator>
      <ExplainGrid subject={PM} name="Portfolio manager" disabled={false} />
    </OperatorProvider>,
  );
}

const row = (kind: string, value: string) => screen.findByTestId(`explain-row-${kind}-${value}`);

beforeEach(() => {
  cleanup();
  explainMock.mockReset();
  getPermissionsMock.mockReset();
  deleteGrantMock.mockReset();
  upsertGrantMock.mockReset();
  toastErrorMock.mockReset();
  listWorkspacesMock.mockReset().mockResolvedValue([{ id: "ws-1", name: "Portfolio tools" }]);
  getModelProvidersMock
    .mockReset()
    .mockResolvedValue({ providers: { providers: [{ id: "mp-1", name: "Desk gateway", kind: "anthropic_key" }] } });
});

describe("ExplainGrid — the request", () => {
  it("asks for the type by its id, across main's ten kinds in order", async () => {
    answer([]);
    renderGrid();
    await screen.findByTestId("explain-grid");
    expect(explainMock).toHaveBeenCalledWith("user_type", PM, [
      "egress_host",
      "secret",
      "workspace",
      "image",
      "agent",
      "workspace_provider",
      "model_provider",
      "feature",
      "policy",
      "component",
    ]);
  });

  it("a 400 (a type deleted since the list loaded) shows the failure line and the server's sentence", async () => {
    explainMock.mockRejectedValue(new HttpError(400, 'user type "portfolio-manager" does not exist'));
    getPermissionsMock.mockResolvedValue({ grants: [], enforcement: {} });
    renderGrid();
    expect(await screen.findByText(UT.EXPLAIN_LOAD_FAILED)).toBeInTheDocument();
    expect(screen.getByText('user type "portfolio-manager" does not exist')).toBeInTheDocument();
    expect(screen.queryByTestId("explain-grid")).not.toBeInTheDocument();
  });
});

describe("ExplainGrid — states, as packet A draws them", () => {
  it("names each state, green for this type's own, red for a block, grey otherwise", async () => {
    answer([
      { kind: "egress_host", value: "*", state: "everyone" },
      { kind: "secret", value: "*", state: "this_type" },
      { kind: "workspace", value: "*", state: "not_available" },
      { kind: "image", value: "*", state: "admins_only" },
      { kind: "agent", value: "*", state: "blocked" },
    ]);
    renderGrid();
    const tone = async (kind: string, label: string) => within(await row(kind, "*")).getByText(label).className;
    expect(await tone("secret", EXPLAIN.STATE.this_type)).toContain("bg-success-subtle");
    expect(await tone("agent", EXPLAIN.STATE.blocked)).toContain("bg-danger-subtle");
    for (const [kind, state] of [
      ["egress_host", "everyone"],
      ["workspace", "not_available"],
      ["image", "admins_only"],
    ] as const) {
      expect(await tone(kind, EXPLAIN.STATE[state])).toContain("bg-muted");
      expect((await row(kind, "*")).className).toContain("text-muted-foreground");
    }
    expect((await row("secret", "*")).className).not.toContain("text-muted-foreground");
  });

  it("a restricted value renders with no grant naming it, and is not available on image too", async () => {
    answer([
      { kind: "image", value: "*", state: "admins_only" },
      { kind: "image", value: "ghcr.io/acme/toolbox:1.4", state: "not_available", restricted: true },
      { kind: "model_provider", value: "*", state: "everyone" },
      { kind: "model_provider", value: "mp-unlisted", state: "not_available", restricted: true },
    ]);
    renderGrid();
    const img = await row("image", "ghcr.io/acme/toolbox:1.4");
    expect(within(img).getByText(EXPLAIN.STATE.not_available)).toBeInTheDocument();
    expect(within(img).getByText("ghcr.io/acme/toolbox:1.4")).toBeInTheDocument();
    expect(within(await row("model_provider", "mp-unlisted")).getByText(EXPLAIN.STATE.not_available)).toBeInTheDocument();
  });

  it("a restricted value this type isn't listed for names who it is for after 'only'", async () => {
    answer(
      [
        { kind: "workspace_provider", value: "*", state: "everyone" },
        { kind: "workspace_provider", value: "ado-org", state: "not_available", restricted: true },
        { kind: "workspace_provider", value: "gh-app", state: "not_available", restricted: true },
      ],
      [
        grant({ id: "a", subject: "developer", capability: "workspace_provider", value: "ado-org", effect: "allow" }),
        grant({ id: "b", subject_type: "group", subject: "data-eng", capability: "workspace_provider", value: "gh-app", effect: "allow" }),
        // A deny is not an audience.
        grant({ id: "c", subject_type: "user", subject: "x@corp.test", capability: "workspace_provider", value: "gh-app", effect: "deny" }),
      ],
    );
    renderGrid();
    // A type by its name, not its id (packet A: "· only Developer").
    expect(within(await row("workspace_provider", "ado-org")).getByText(EXPLAIN.ONLY("Developer"))).toBeInTheDocument();
    expect(within(await row("workspace_provider", "gh-app")).getByText(EXPLAIN.ONLY("data-eng (group)"))).toBeInTheDocument();
  });

  it("no 'only' on a row that is not restricted, or one this type can reach", async () => {
    answer(
      [
        { kind: "policy", value: "*", state: "everyone" },
        { kind: "policy", value: "pol-2", state: "this_type", restricted: true },
        { kind: "policy", value: "pol-3", state: "not_available" },
      ],
      [grant({ id: "a", subject: "developer", capability: "policy", value: "pol-3", effect: "allow" })],
    );
    renderGrid();
    await row("policy", "pol-3");
    expect(screen.queryByText(/^only /)).not.toBeInTheDocument();
  });

  it("the wall note sits once, under the first family with a block", async () => {
    answer([
      { kind: "agent", value: "*", state: "everyone" },
      { kind: "agent", value: "codex", state: "blocked" },
      { kind: "feature", value: "*", state: "everyone" },
      { kind: "feature", value: "ssh_key", state: "blocked" },
    ]);
    renderGrid();
    const head = await screen.findByText(EXPLAIN.WALL_HEAD);
    expect(screen.getAllByText(EXPLAIN.WALL_HEAD)).toHaveLength(1);
    expect(head.closest("div")?.textContent).toBe(`${EXPLAIN.WALL_HEAD} ${EXPLAIN.WALL_BODY}`);
    expect(head.closest("[class*='border-t']")?.querySelector("h5")?.textContent).toBe("Agents");
  });

  it("no wall note without a block", async () => {
    answer([{ kind: "agent", value: "*", state: "everyone" }]);
    renderGrid();
    await row("agent", "*");
    expect(screen.queryByText(EXPLAIN.WALL_HEAD)).not.toBeInTheDocument();
  });
});

describe("ExplainGrid — Remove", () => {
  it("offers Remove only on a row written for this type, confirms, deletes that grant, and re-reads", async () => {
    answer(
      [
        { kind: "agent", value: "*", state: "everyone" },
        { kind: "agent", value: "codex", state: "blocked" },
        { kind: "secret", value: "*", state: "everyone" },
        { kind: "secret", value: "db", state: "this_type" },
      ],
      [
        grant({ id: "own-deny" }),
        // This type's allow comes from a row for everyone: not this type's to remove.
        grant({ id: "all-allow", subject_type: "all", subject: "", capability: "secret", value: "db", effect: "allow" }),
      ],
    );
    deleteGrantMock.mockResolvedValue(undefined);
    renderGrid();
    const codex = await row("agent", "codex");
    await screen.findByRole("button", { name: `${EXPLAIN.REMOVE} Codex` });
    expect(within(await row("secret", "db")).queryByRole("button")).not.toBeInTheDocument();

    await userEvent.click(within(codex).getByRole("button", { name: `${EXPLAIN.REMOVE} Codex` }));
    expect(screen.getByText(PERM.REMOVE_CONFIRM("Portfolio manager"))).toBeInTheDocument();
    expect(deleteGrantMock).not.toHaveBeenCalled();
    explainMock.mockClear();
    await userEvent.click(screen.getByRole("button", { name: PERM.REMOVE }));
    expect(deleteGrantMock).toHaveBeenCalledWith("own-deny");
    expect(explainMock).toHaveBeenCalledTimes(1);
  });

  it("a refused delete shows the server's sentence", async () => {
    answer([{ kind: "agent", value: "codex", state: "blocked" }], [grant({ id: "own-deny" })]);
    deleteGrantMock.mockRejectedValue(new HttpError(403, "security admins only"));
    renderGrid();
    await userEvent.click(await screen.findByRole("button", { name: `${EXPLAIN.REMOVE} Codex` }));
    await userEvent.click(screen.getByRole("button", { name: PERM.REMOVE }));
    expect(await screen.findByRole("alert")).toHaveTextContent("security admins only");
  });
});

describe("ExplainGrid — values by name", () => {
  it("names what packet A names, and shows any other value as written", async () => {
    answer([
      { kind: "workspace", value: "*", state: "everyone" },
      { kind: "workspace", value: "ws-1", state: "this_type" },
      { kind: "image", value: "*", state: "admins_only" },
      { kind: "agent", value: "claude_code", state: "everyone" },
      { kind: "model_provider", value: "mp-1", state: "this_type" },
      { kind: "feature", value: "*", state: "everyone" },
      { kind: "feature", value: "ssh_key", state: "blocked" },
      { kind: "feature", value: "api_token", state: "everyone" },
      { kind: "policy", value: "pol-1", state: "this_type" },
      { kind: "egress_host", value: "api.example.test", state: "blocked" },
    ]);
    renderGrid();
    expect(within(await row("workspace", "*")).getByText(EXPLAIN.ALL_WORKSPACES)).toBeInTheDocument();
    expect(await within(await row("workspace", "ws-1")).findByText("Portfolio tools")).toBeInTheDocument();
    expect(within(await row("image", "*")).getByText(EXPLAIN.ALL_IMAGES)).toBeInTheDocument();
    expect(within(await row("agent", "claude_code")).getByText("Claude Code")).toBeInTheDocument();
    expect(await within(await row("model_provider", "mp-1")).findByText("Desk gateway")).toBeInTheDocument();
    expect(within(await row("feature", "*")).getByText(EXPLAIN.SSH_AND_TOKENS)).toBeInTheDocument();
    expect(within(await row("feature", "ssh_key")).getByText(EXPLAIN.SSH_KEYS)).toBeInTheDocument();
    expect(within(await row("feature", "api_token")).getByText(EXPLAIN.API_TOKENS)).toBeInTheDocument();
    expect(await within(await row("policy", "pol-1")).findByText("Read-only research")).toBeInTheDocument();
    expect(within(await row("egress_host", "api.example.test")).getByText("api.example.test").className).toContain("font-mono");
  });

  it("a security admin never asks for the super-admin-only model provider roster", async () => {
    answer([{ kind: "model_provider", value: "mp-1", state: "this_type" }]);
    renderGrid(false);
    expect(within(await row("model_provider", "mp-1")).getByText("mp-1")).toBeInTheDocument();
    expect(getModelProvidersMock).not.toHaveBeenCalled();
  });

  it("a server label (G-4) wins over a client-side name, on any kind", async () => {
    answer([
      { kind: "workspace_provider", value: "ado-1", state: "this_type", label: "Azure DevOps · example-org" },
      // A security admin can't read the model provider roster, but the
      // server's own label still names the row.
      { kind: "model_provider", value: "mp-1", state: "this_type", label: "Corp gateway" },
    ]);
    renderGrid(false);
    expect(within(await row("workspace_provider", "ado-1")).getByText("Azure DevOps · example-org")).toBeInTheDocument();
    expect(within(await row("model_provider", "mp-1")).getByText("Corp gateway")).toBeInTheDocument();
    expect(getModelProvidersMock).not.toHaveBeenCalled();
  });
});

describe("ExplainGrid — families (G-1, G-3)", () => {
  it("names the default row 'All {family}' on every kind packet A didn't", async () => {
    answer(
      ["egress_host", "secret", "agent", "workspace_provider", "model_provider", "policy"].map((kind) => ({
        kind,
        value: "*",
        state: "everyone",
      })),
    );
    renderGrid();
    expect(within(await row("egress_host", "*")).getByText(EXPLAIN.ALL_EGRESS_HOSTS)).toBeInTheDocument();
    expect(within(await row("secret", "*")).getByText(EXPLAIN.ALL_SECRETS)).toBeInTheDocument();
    expect(within(await row("agent", "*")).getByText(EXPLAIN.ALL_AGENTS)).toBeInTheDocument();
    expect(within(await row("workspace_provider", "*")).getByText(EXPLAIN.ALL_GIT_PROVIDERS)).toBeInTheDocument();
    expect(within(await row("model_provider", "*")).getByText(EXPLAIN.ALL_MODEL_PROVIDERS)).toBeInTheDocument();
    expect(within(await row("policy", "*")).getByText(EXPLAIN.ALL_POLICIES)).toBeInTheDocument();
  });
});

describe("ExplainGrid — several audiences (G-5)", () => {
  function audienceGrants(names: string[]) {
    return names.map((n, i) => grant({ id: `a${i}`, subject: n, capability: "workspace", value: "ws-2", effect: "allow" }));
  }

  it("two audiences read 'A and B', three read 'A, B and C'", async () => {
    answer(
      [{ kind: "workspace", value: "ws-2", state: "not_available", restricted: true }],
      audienceGrants(["ann", "bo"]),
    );
    renderGrid();
    expect(within(await row("workspace", "ws-2")).getByText(EXPLAIN.ONLY("ann and bo"))).toBeInTheDocument();
    cleanup();

    answer(
      [{ kind: "workspace", value: "ws-2", state: "not_available", restricted: true }],
      audienceGrants(["ann", "bo", "cy"]),
    );
    renderGrid();
    expect(within(await row("workspace", "ws-2")).getByText(EXPLAIN.ONLY("ann, bo and cy"))).toBeInTheDocument();
  });

  it("four or more names the first two and counts the rest, with the full list a mouse hover and, as real sr-only text, a screen reader away", async () => {
    const names = ["ann", "bo", "cy", "dee"];
    answer([{ kind: "workspace", value: "ws-2", state: "not_available", restricted: true }], audienceGrants(names));
    renderGrid();
    const cellEl = within(await row("workspace", "ws-2"));
    // The truncated text: a mouse-only hover (title), no accessible name of
    // its own (F1 — an aria-label here couldn't reach a keyboard or screen
    // reader user, since the span is plain and non-focusable).
    const visible = await cellEl.findByText(EXPLAIN.ONLY("ann, bo and 2 more"));
    expect(visible).toHaveAttribute("title", names.join(", "));
    expect(visible).toHaveAttribute("aria-hidden", "true");
    expect(visible).not.toHaveAttribute("aria-label");
    // The full, untruncated list: real text content (sr-only), found the same
    // way a screen reader's text query would find it — not an attribute.
    const full = cellEl.getByText(EXPLAIN.ONLY(names.join(", ")));
    expect(full).toHaveClass("sr-only");
  });
});

describe("ExplainGrid — legend and footer (G-6)", () => {
  it("shows the one true legend and packet A's own second footer", async () => {
    answer([{ kind: "agent", value: "*", state: "everyone" }]);
    renderGrid();
    await row("agent", "*");
    expect(screen.getByText(EXPLAIN.LEGEND)).toBeInTheDocument();
    expect(screen.getByText(EXPLAIN.FOOTER_OTHER_SIDE)).toBeInTheDocument();
  });
});

describe("ExplainGrid — Add (G-7)", () => {
  it("opens Permissions' own Add a grant dialog, Who and Capability fixed, writes the grant and reloads", async () => {
    answer([{ kind: "agent", value: "*", state: "everyone" }]);
    renderGrid();
    await row("agent", "*");

    // Only "agent" answered any rows, so it is the grid's one family.
    await userEvent.click(screen.getByRole("button", { name: EXPLAIN.ADD }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(PERM.ADD_TITLE)).toBeInTheDocument();
    // Who and Capability are fixed, not picked.
    expect(within(dialog).getByText("Portfolio manager")).toBeInTheDocument();
    expect(within(dialog).getByText(KIND.agent.label)).toBeInTheDocument();
    expect(within(dialog).queryByRole("combobox", { name: PERM.FIELD_CAPABILITY })).not.toBeInTheDocument();

    upsertGrantMock.mockResolvedValue({ updated: false });
    explainMock.mockClear();
    await userEvent.type(within(dialog).getByLabelText(KIND.agent.valueLabel), "claude-code");
    await userEvent.click(within(dialog).getByRole("button", { name: PERM.ADD_CTA }));

    expect(upsertGrantMock).toHaveBeenCalledWith({
      subject_type: "user_type",
      subject: PM,
      capability: "agent",
      value: "claude-code",
      effect: "allow",
    });
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(explainMock).toHaveBeenCalledTimes(1);
  });

  it("a deny for a user type warns with Permissions' own wall sentence before writing it", async () => {
    answer([{ kind: "agent", value: "*", state: "everyone" }]);
    renderGrid();
    await row("agent", "*");
    await userEvent.click(screen.getByRole("button", { name: EXPLAIN.ADD }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText(KIND.agent.valueLabel), "codex");
    await userEvent.click(within(dialog).getByRole("button", { name: PERM.EFFECT_DENY }));
    await userEvent.click(within(dialog).getByRole("button", { name: PERM.ADD_CTA }));

    expect(await screen.findByText(PERM.TYPE_DENY_TITLE)).toBeInTheDocument();
    expect(screen.getByText(PERM.TYPE_DENY_BODY)).toBeInTheDocument();
    expect(upsertGrantMock).not.toHaveBeenCalled();
  });

  it("a refused add shows the server's sentence in the dialog, which stays open (F2)", async () => {
    answer([{ kind: "agent", value: "*", state: "everyone" }]);
    renderGrid();
    await row("agent", "*");
    await userEvent.click(screen.getByRole("button", { name: EXPLAIN.ADD }));
    const dialog = await screen.findByRole("dialog");
    upsertGrantMock.mockRejectedValue(new HttpError(403, "security admins only"));
    await userEvent.type(within(dialog).getByLabelText(KIND.agent.valueLabel), "claude-code");
    await userEvent.click(within(dialog).getByRole("button", { name: PERM.ADD_CTA }));

    // The packet's "Refused" state (§2): the server's own sentence, verbatim,
    // in the dialog — never a toast, and the dialog does not close.
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("security admins only");
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(toastErrorMock).not.toHaveBeenCalled();
  });
});
