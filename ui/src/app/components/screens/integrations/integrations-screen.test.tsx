/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The list, base-component model (B3): three DERIVED sections (AI providers /
// Source control / Connections), a base-summary row (secret · egress ·
// delivery), a probe chip, and "Adopt to edit" as the ONLY promotion for a
// derived row — no more silent adopt-on-checkbox-toggle.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { baseStatus } from "../setup/test-fixtures";
import { T } from "../../../lib/integrations";
import { OperatorProvider } from "../../wardyn/operator-context";
import type { WireIntegration } from "../../../lib/types/setup";

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({ setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) } }));

const getSiteConfigMock = vi.fn();
vi.mock("../../../lib/api/health", () => ({
  health: { getSiteConfig: (...a: unknown[]) => getSiteConfigMock(...a), putSiteConfig: vi.fn() },
}));

const listSecretsMock = vi.fn();
vi.mock("../../../lib/api/secrets", () => ({ secrets: { listSecrets: (...a: unknown[]) => listSecretsMock(...a) } }));

vi.mock("../../../lib/api/harness-auth", () => ({
  harnessAuth: { harnessDisconnect: vi.fn(), harnessLogin: vi.fn(), harnessCredentialPaste: vi.fn() },
}));

const listMock = vi.fn();
const adoptIntegrationMock = vi.fn();
const putIntegrationMock = vi.fn();
const removeIntegrationMock = vi.fn();
const testIntegrationMock = vi.fn();
vi.mock("../../../lib/api/integrations", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../../lib/api/integrations")>();
  return {
    ...actual,
    integrationsApi: { ...actual.integrationsApi, adoptIntegration: (...a: unknown[]) => adoptIntegrationMock(...a) },
    genericIntegrationsApi: {
      ...actual.genericIntegrationsApi,
      list: (...a: unknown[]) => listMock(...a),
      put: (...a: unknown[]) => putIntegrationMock(...a),
      remove: (...a: unknown[]) => removeIntegrationMock(...a),
      test: (...a: unknown[]) => testIntegrationMock(...a),
    },
  };
});

import { IntegrationsScreen } from "./integrations-screen";
import { harnessAuth } from "../../../lib/api/harness-auth";

function renderScreen(operator = true) {
  return render(
    <MemoryRouter>
      <OperatorProvider operator={operator}>
        <IntegrationsScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

const AI_ROW: WireIntegration = {
  id: "anthropic_api_key",
  name: "Anthropic API key",
  kind: "anthropic_api_key",
  source: "legacy",
  secrets: [{ role: "api_key", secret_name: "anthropic-api-key", delivery: { mode: "proxy_header", header: "x-api-key", format: "%s" } }],
  egress: [],
};

const SCM_ROW: WireIntegration = {
  id: "git_host:gitlab.com",
  name: "gitlab.com",
  kind: "git_host",
  source: "stored",
  secrets: [{ role: "pat", secret_name: "gitlab-pat" }],
};

const FEED_ROW: WireIntegration = {
  id: "corp-artifactory",
  name: "Corp Artifactory",
  kind: "artifactory",
  source: "stored",
  egress: ["artifactory.corp.internal"],
  secrets: [{ role: "api_key", secret_name: "artifactory-token", delivery: { mode: "proxy_header", header: "Authorization", format: "Bearer %s" } }],
  probe: { method: "GET", url: "https://artifactory.corp.internal/" },
  probe_status: { state: "passed" },
};

describe("IntegrationsScreen — three sections", () => {
  beforeEach(() => {
    getSiteConfigMock.mockReset().mockResolvedValue({});
    listSecretsMock.mockReset().mockResolvedValue([]);
  });

  it("buckets rows into AI providers / Source control / Connections and shows each empty section's own line", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus());
    listMock.mockResolvedValue([AI_ROW]);

    renderScreen();

    await screen.findByRole("region", { name: "AI providers" });
    expect(screen.getByText("Anthropic API key")).toBeInTheDocument();
    expect(screen.getByText(T.EMPTY_SCM)).toBeInTheDocument();
    // ui-integrations-8: Connections' empty state used to fall back to the bare
    // literal "None." — it now explains itself like its two siblings.
    expect(screen.getByText(T.EMPTY_OTHER)).toBeInTheDocument();
    expect(screen.queryByText("None.")).not.toBeInTheDocument();
    expect(screen.getByRole("region", { name: "Connections" })).toBeInTheDocument();
  });

  // ui-integrations-9: the detail page wraps "stored" in a bordered Chip; the
  // list row used to drop to a bare <span> for the identical state.
  it("the 'stored' indicator renders as a bordered Chip, matching the detail page", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus());
    listMock.mockResolvedValue([FEED_ROW]);

    renderScreen();

    const stored = await screen.findByText("stored");
    expect(stored.className).toMatch(/rounded-md/);
    expect(stored.className).toMatch(/\bborder\b/);
  });

  it("shows the base-summary line and the probe chip", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus());
    listMock.mockResolvedValue([FEED_ROW]);

    renderScreen();

    await screen.findByText("Corp Artifactory");
    expect(screen.getByText("1 secret · 1 host · proxy-injected")).toBeInTheDocument();
    expect(screen.getByText("verified")).toBeInTheDocument();
  });

  it("a row with no probe_status reads 'not tested', never a silent pass", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus());
    listMock.mockResolvedValue([SCM_ROW]);

    renderScreen();

    await screen.findByText("gitlab.com");
    expect(screen.getByText("not tested")).toBeInTheDocument();
  });

  it("shows the empty state only when nothing is configured at all", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus());
    listMock.mockResolvedValue([]);

    renderScreen();

    await screen.findByText(T.EMPTY_TITLE);
    expect(screen.getByText(T.CORP_POINTER)).toBeInTheDocument();
  });
});

describe("IntegrationsScreen — Adopt to edit is the only promotion", () => {
  beforeEach(() => {
    getSetupStatusMock.mockResolvedValue(baseStatus());
    getSiteConfigMock.mockResolvedValue({});
    listSecretsMock.mockResolvedValue([]);
    adoptIntegrationMock.mockReset().mockResolvedValue(undefined);
    putIntegrationMock.mockReset().mockResolvedValue(undefined);
  });

  it("a derived (source: legacy) row shows 'Adopt to edit', not a default-for control", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    listMock.mockResolvedValue([AI_ROW]);
    renderScreen();

    await screen.findByText("Anthropic API key");
    expect(screen.getByRole("button", { name: "Adopt to edit" })).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: `${AI_ROW.name} actions` }));
    expect(screen.queryByRole("menuitem", { name: /default for agent runs/i })).not.toBeInTheDocument();
  });

  it("clicking Adopt to edit calls adoptIntegration and reloads — no PUT as a side effect", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    listMock.mockResolvedValue([AI_ROW]);
    renderScreen();
    await screen.findByText("Anthropic API key");

    await user.click(screen.getByRole("button", { name: "Adopt to edit" }));

    await waitFor(() => expect(adoptIntegrationMock).toHaveBeenCalledWith("anthropic_api_key"));
    expect(putIntegrationMock).not.toHaveBeenCalled();
  });

  it("a STORED AI row's kebab default-for item PUTs directly — no adopt call at all", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const stored = { ...AI_ROW, source: "stored" as const, default_for: [] };
    listMock.mockResolvedValueOnce([stored]).mockResolvedValueOnce([{ ...stored, default_for: ["agent_runs"] }]);
    renderScreen();
    await screen.findByText("Anthropic API key");

    await user.click(screen.getByRole("button", { name: `${AI_ROW.name} actions` }));
    await user.click(await screen.findByRole("menuitem", { name: /set default for agent runs/i }));

    await waitFor(() =>
      expect(putIntegrationMock).toHaveBeenCalledWith("anthropic_api_key", expect.objectContaining({ default_for: ["agent_runs"] })),
    );
    expect(adoptIntegrationMock).not.toHaveBeenCalled();
  });
});

describe("IntegrationsScreen — viewer role disables writes", () => {
  beforeEach(() => {
    getSetupStatusMock.mockResolvedValue(baseStatus());
    getSiteConfigMock.mockResolvedValue({});
    listSecretsMock.mockResolvedValue([]);
    listMock.mockResolvedValue([{ ...AI_ROW, source: "stored" as const }]);
  });

  it("disables Add, Adopt-equivalent kebab writes, and shows the viewer banner", async () => {
    renderScreen(false);
    await screen.findByText("Anthropic API key");

    expect(screen.getByText("Viewer role")).toBeInTheDocument();
    const addButtons = screen.getAllByRole("button", { name: /add integration/i });
    expect(addButtons.length).toBeGreaterThan(0);
    addButtons.forEach((b) => expect(b).toBeDisabled());
  });

  it("a row's Delete action is disabled and names the operator-only reason", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen(false);
    await screen.findByText("Anthropic API key");

    await user.click(screen.getByRole("button", { name: `${AI_ROW.name} actions` }));
    const del = await screen.findByRole("menuitem", { name: /delete integration/i });
    expect(del).toHaveAttribute("data-disabled");
    expect(within(del).getByText(/requires the operator role/i)).toBeInTheDocument();
  });

  // ui-integrations-6: "Adopt to edit" used to disable with a hover-only
  // `title`, unreachable by keyboard/AT — it now names the reason as visible
  // button content, same as every other disabled control on this screen.
  it("a viewer's disabled 'Adopt to edit' names the reason visibly, not just via title", async () => {
    listMock.mockResolvedValue([AI_ROW]); // source: "legacy" — derived, shows Adopt to edit
    renderScreen(false);
    await screen.findByText("Anthropic API key");

    const adopt = screen.getByRole("button", { name: /adopt to edit/i });
    expect(adopt).toBeDisabled();
    expect(within(adopt).getByText(/requires the operator role/i)).toBeInTheDocument();
    expect(adopt).not.toHaveAttribute("title");
  });
});

// Bug report 2026-08-15: deleting the managed Claude subscription "and it still
// appeared" — the row is DERIVED from the captured harness login, not stored, so
// a plain delete has nothing to remove and the list re-derives it. deleteWireRow
// already disconnects the login when handed the provider; the call site must pass
// it (managed subscription -> "anthropic"), or delete is a no-op that reappears.
describe("IntegrationsScreen — deleting a harness-derived subscription disconnects the login", () => {
  beforeEach(() => {
    getSiteConfigMock.mockResolvedValue({});
    listSecretsMock.mockResolvedValue([]);
    (harnessAuth.harnessDisconnect as ReturnType<typeof vi.fn>).mockReset().mockResolvedValue(undefined);
    removeIntegrationMock.mockReset().mockResolvedValue(undefined);
  });

  it("routes a managed-subscription delete to harnessDisconnect (by the row's derived nature, not a stale captured flag)", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    getSetupStatusMock.mockResolvedValue(baseStatus()); // no captured flag in status — the row's non-stored derived nature is what matters
    const managed: WireIntegration = {
      id: "anthropic_subscription:managed",
      name: "Claude subscription (managed)",
      kind: "anthropic_subscription",
      source: "legacy", // DERIVED — projected from the harness login, never stored
      egress: ["api.anthropic.com"],
    };
    listMock.mockResolvedValue([managed]);
    renderScreen();
    await screen.findByText("Claude subscription (managed)");

    await user.click(screen.getByRole("button", { name: /Claude subscription \(managed\) actions/i }));
    await user.click(await screen.findByRole("menuitem", { name: /delete integration/i }));
    // the confirm is honest about the real consequence (disconnect, not a config delete)
    expect(await screen.findByText(/loses model access until you log in again/i)).toBeInTheDocument();
    await user.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: /delete integration/i }));

    await waitFor(() => expect(harnessAuth.harnessDisconnect).toHaveBeenCalledWith("anthropic"));
    expect(removeIntegrationMock).not.toHaveBeenCalled();
  });
});

describe("IntegrationsScreen — the proxy banner", () => {
  it("appears only when a proxy was detected and nothing is connected yet", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ host_proxy: { has_credentials: false, http_proxy: { value: "proxy.corp:8080", source: "env", has_credentials: false } } }),
    );
    getSiteConfigMock.mockResolvedValue({});
    listSecretsMock.mockResolvedValue([]);
    listMock.mockResolvedValue([]);

    renderScreen();
    await waitFor(() => expect(screen.getByText(T.PROXY_BANNER)).toBeInTheDocument());
    expect(screen.getByRole("button", { name: /^open corporate network$/i })).toBeInTheDocument();
  });
});

describe("IntegrationsScreen — embedded mode", () => {
  beforeEach(() => {
    getSetupStatusMock.mockResolvedValue(baseStatus());
    getSiteConfigMock.mockResolvedValue({});
    listSecretsMock.mockResolvedValue([]);
  });

  it("drops the PageHeader but keeps the sections and footnote", async () => {
    listMock.mockResolvedValue([{ ...AI_ROW, source: "stored" as const }]);

    render(
      <MemoryRouter>
        <IntegrationsScreen embedded />
      </MemoryRouter>,
    );

    await screen.findByText("Anthropic API key");
    expect(screen.queryByRole("heading", { name: "Integrations", level: 1 })).not.toBeInTheDocument();
    expect(screen.queryByText(T.LEDE)).not.toBeInTheDocument();
    expect(screen.getByText(T.FOOTNOTE)).toBeInTheDocument();
    expect(screen.queryByText(T.CORP_POINTER)).not.toBeInTheDocument();
  });

  it("calls onChanged after a real mutation reloads, not on the initial mount", async () => {
    listMock.mockResolvedValue([{ ...AI_ROW, source: "stored" as const }]);
    removeIntegrationMock.mockResolvedValue(undefined);
    const onChanged = vi.fn();
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    render(
      <MemoryRouter>
        <IntegrationsScreen embedded onChanged={onChanged} />
      </MemoryRouter>,
    );

    await screen.findByText("Anthropic API key");
    expect(onChanged).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: `${AI_ROW.name} actions` }));
    await user.click(await screen.findByRole("menuitem", { name: /delete integration/i }));
    await user.click(await screen.findByRole("button", { name: /^delete integration$/i }));

    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1));
  });
});

describe("IntegrationsScreen — delete confirm carries the base blast radius", () => {
  it("shows the default-holder lines when the row pending delete holds both marks", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus());
    getSiteConfigMock.mockResolvedValue({});
    listSecretsMock.mockResolvedValue([]);
    listMock.mockResolvedValue([{ ...AI_ROW, source: "stored" as const, default_for: ["agent_runs", "wardyn_features"] }]);
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    renderScreen();
    await screen.findByText("Anthropic API key");

    await user.click(screen.getByRole("button", { name: `${AI_ROW.name} actions` }));
    await user.click(await screen.findByRole("menuitem", { name: /delete integration/i }));

    const dialog = within(await screen.findByRole("alertdialog"));
    expect(dialog.getByText(/first model call fails/i)).toBeInTheDocument();
    expect(dialog.getByText(/Composer loses its backend/i)).toBeInTheDocument();
  });
});

// ui-integrations-4: the "Off" badge (wire.disabled) used to be read-only —
// no control anywhere ever set or cleared it. A stored row's kebab now offers
// Enable/Disable, PUTting the flipped flag straight through actions.ts's
// toggleDisabled (same discipline as the default-for items just above it).
describe("IntegrationsScreen — a stored row can be disabled and re-enabled", () => {
  beforeEach(() => {
    getSetupStatusMock.mockResolvedValue(baseStatus());
    getSiteConfigMock.mockResolvedValue({});
    listSecretsMock.mockResolvedValue([]);
    putIntegrationMock.mockReset().mockResolvedValue(undefined);
  });

  it("offers 'Disable integration' for a stored, enabled row and PUTs disabled: true", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    listMock.mockResolvedValue([FEED_ROW]); // FEED_ROW: source "stored", non-AI kind
    renderScreen();
    await screen.findByText("Corp Artifactory");

    await user.click(screen.getByRole("button", { name: `${FEED_ROW.name} actions` }));
    await user.click(await screen.findByRole("menuitem", { name: /^disable integration$/i }));

    await waitFor(() =>
      expect(putIntegrationMock).toHaveBeenCalledWith("corp-artifactory", expect.objectContaining({ disabled: true })),
    );
  });

  it("offers 'Enable integration' for a stored, disabled row and PUTs disabled: false", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    listMock.mockResolvedValue([{ ...FEED_ROW, disabled: true }]);
    renderScreen();
    await screen.findByText("Corp Artifactory");

    await user.click(screen.getByRole("button", { name: `${FEED_ROW.name} actions` }));
    await user.click(await screen.findByRole("menuitem", { name: /^enable integration$/i }));

    await waitFor(() =>
      expect(putIntegrationMock).toHaveBeenCalledWith("corp-artifactory", expect.objectContaining({ disabled: false })),
    );
  });

  it("a derived (non-stored) row offers no Enable/Disable item — PUT would 409", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    listMock.mockResolvedValue([AI_ROW]); // source: "legacy"
    renderScreen();
    await screen.findByText("Anthropic API key");

    await user.click(screen.getByRole("button", { name: `${AI_ROW.name} actions` }));
    expect(screen.queryByRole("menuitem", { name: /disable integration/i })).not.toBeInTheDocument();
  });
});
