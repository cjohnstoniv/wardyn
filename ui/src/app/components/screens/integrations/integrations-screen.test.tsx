/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { baseStatus } from "../setup/test-fixtures";
import { T } from "../../../lib/integrations";
import { OperatorProvider } from "../../wardyn/operator-context";

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({ setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) } }));

const getSiteConfigMock = vi.fn();
const putSiteConfigMock = vi.fn();
vi.mock("../../../lib/api/health", () => ({
  health: {
    getSiteConfig: (...a: unknown[]) => getSiteConfigMock(...a),
    putSiteConfig: (...a: unknown[]) => putSiteConfigMock(...a),
  },
}));

const listSecretsMock = vi.fn();
const deleteSecretMock = vi.fn();
const setSecretMock = vi.fn();
vi.mock("../../../lib/api/secrets", () => ({
  secrets: {
    listSecrets: (...a: unknown[]) => listSecretsMock(...a),
    deleteSecret: (...a: unknown[]) => deleteSecretMock(...a),
    setSecret: (...a: unknown[]) => setSecretMock(...a),
  },
}));

const harnessDisconnectMock = vi.fn();
vi.mock("../../../lib/api/harness-auth", () => ({
  harnessAuth: {
    harnessDisconnect: (...a: unknown[]) => harnessDisconnectMock(...a),
    harnessLogin: vi.fn(),
    harnessCredentialPaste: vi.fn(),
  },
}));

import { IntegrationsScreen } from "./integrations-screen";

function renderScreen(operator = true) {
  return render(
    <MemoryRouter>
      <OperatorProvider operator={operator}>
        <IntegrationsScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

describe("IntegrationsScreen — the four category sections", () => {
  beforeEach(() => {
    putSiteConfigMock.mockReset().mockResolvedValue(undefined);
    deleteSecretMock.mockReset().mockResolvedValue(undefined);
  });

  it("renders a populated AI row and each other category's own empty line", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus({ secrets: { present: ["anthropic-api-key"], github_app: false } }));
    getSiteConfigMock.mockResolvedValue({});
    listSecretsMock.mockResolvedValue(["anthropic-api-key"]);

    renderScreen();

    await screen.findByText("Anthropic (API key)");
    expect(screen.getByText("anthropic · api key")).toBeInTheDocument();
    // The other three sections are empty — each shows its OWN T.EMPTY_* line.
    expect(screen.getByText(T.EMPTY_SCM)).toBeInTheDocument();
    expect(screen.getByText(T.EMPTY_MIRROR)).toBeInTheDocument();
    expect(screen.getByText(T.EMPTY_PROXY)).toBeInTheDocument();
  });

  it("shows the big empty state (title + body + CTA) only when NOTHING is configured", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus());
    getSiteConfigMock.mockResolvedValue({});
    listSecretsMock.mockResolvedValue([]);

    renderScreen();

    await screen.findByText(T.EMPTY_TITLE);
    expect(screen.getByText(T.EMPTY_BODY, { exact: false })).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: /add integration/i }).length).toBeGreaterThan(0);
    // The four per-category sections do not render in this branch.
    expect(screen.queryByText(T.EMPTY_SCM)).toBeNull();
  });

  it("renders all four sections at once when every category has a row", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ secrets: { present: ["anthropic-api-key", "git-pat-github-com"], github_app: false } }),
    );
    getSiteConfigMock.mockResolvedValue({
      scm_hosts: ["github.com"],
      artifact_overrides: { npm: { base_url: "https://artifactory.corp.internal/api/npm/x" } },
      upstream_proxy_secret_ref: "upstream-proxy-url",
    });
    listSecretsMock.mockResolvedValue(["anthropic-api-key", "git-pat-github-com"]);

    renderScreen();

    await screen.findByText("Anthropic (API key)");
    expect(screen.getByText("GitHub")).toBeInTheDocument();
    expect(screen.getByText("artifactory.corp.internal")).toBeInTheDocument();
    // "Host proxy" names both the category section AND the singleton row —
    // assert the row via its distinguishing mono type-line instead.
    expect(screen.getByText("corporate proxy")).toBeInTheDocument();
    expect(screen.getByText(T.FOOTNOTE)).toBeInTheDocument();
  });

  // The impossible-as-fact chip: muted, and carries the VERBATIM reason as its
  // tooltip — never a re-typed or paraphrased copy of it.
  it("an impossible capability renders as a muted chip whose tooltip is the verbatim reason", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus({ secrets: { present: ["anthropic-api-key"], github_app: false } }));
    getSiteConfigMock.mockResolvedValue({});
    listSecretsMock.mockResolvedValue(["anthropic-api-key"]);

    renderScreen();

    const codexChip = await screen.findByText("Codex CLI");
    expect(codexChip).toHaveAttribute("title", T.X_KEY_CODEX);
    expect(codexChip.className).toMatch(/opacity-60/);
  });
});

describe("IntegrationsScreen — viewer role disables writes", () => {
  beforeEach(() => {
    getSetupStatusMock.mockResolvedValue(baseStatus({ secrets: { present: ["anthropic-api-key"], github_app: false } }));
    getSiteConfigMock.mockResolvedValue({});
    listSecretsMock.mockResolvedValue(["anthropic-api-key"]);
  });

  it("disables 'Add integration' and shows the viewer banner", async () => {
    renderScreen(false);
    await screen.findByText("Anthropic (API key)");

    expect(screen.getByText("Viewer role")).toBeInTheDocument();
    expect(screen.getByText(T.VIEWER_LINE)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /add integration/i })).toBeDisabled();
  });

  it("a row's Delete/Rotate actions are disabled and each names the operator-only reason", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderScreen(false);
    await screen.findByText("Anthropic (API key)");

    const menuBtn = screen.getByRole("button", { name: /Anthropic \(API key\) actions/i });
    await user.click(menuBtn);

    const rotate = await screen.findByRole("menuitem", { name: /rotate credential/i });
    const del = screen.getByRole("menuitem", { name: /delete integration/i });
    expect(rotate).toHaveAttribute("data-disabled");
    expect(del).toHaveAttribute("data-disabled");
    expect(within(rotate).getByText(/requires the operator role/i)).toBeInTheDocument();
    expect(within(del).getByText(/requires the operator role/i)).toBeInTheDocument();
  });
});

describe("IntegrationsScreen — the proxy banner", () => {
  it("appears only when a proxy was detected and nothing is connected yet", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ host_proxy: { has_credentials: false, http_proxy: { value: "proxy.corp:8080", source: "env", has_credentials: false } } }),
    );
    getSiteConfigMock.mockResolvedValue({});
    listSecretsMock.mockResolvedValue([]);

    renderScreen();
    await waitFor(() => expect(screen.getByText(T.PROXY_BANNER)).toBeInTheDocument());
  });
});

describe("IntegrationsScreen — embedded mode (Getting Started's Integrations step)", () => {
  it("drops the PageHeader and the tab strip but keeps everything else", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus({ secrets: { present: ["anthropic-api-key"], github_app: false } }));
    getSiteConfigMock.mockResolvedValue({});
    listSecretsMock.mockResolvedValue(["anthropic-api-key"]);

    render(
      <MemoryRouter>
        <IntegrationsScreen embedded />
      </MemoryRouter>,
    );

    await screen.findByText("Anthropic (API key)");
    // PageHeader (the h1 title + T.LEDE description) is gone.
    expect(screen.queryByRole("heading", { name: "Integrations", level: 1 })).not.toBeInTheDocument();
    expect(screen.queryByText(T.LEDE)).not.toBeInTheDocument();
    // The Integrations/Tools tab strip is gone — there's only ever the one
    // (integrations) view embedded.
    expect(screen.queryByRole("tab", { name: "Tools" })).not.toBeInTheDocument();
    // Everything else survives: category rows, the footnote.
    expect(screen.getByText(T.EMPTY_SCM)).toBeInTheDocument();
    expect(screen.getByText(T.FOOTNOTE)).toBeInTheDocument();
  });

  it("calls onChanged after a REAL mutation reloads (not the initial mount) — no redundant recheck on every visit", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus({ secrets: { present: ["anthropic-api-key"], github_app: false } }));
    getSiteConfigMock.mockResolvedValue({});
    listSecretsMock.mockResolvedValue(["anthropic-api-key"]);
    deleteSecretMock.mockResolvedValue(undefined);
    const onChanged = vi.fn();
    const user = userEvent.setup({ pointerEventsCheck: 0 });

    render(
      <MemoryRouter>
        <IntegrationsScreen embedded onChanged={onChanged} />
      </MemoryRouter>,
    );

    await screen.findByText("Anthropic (API key)");
    // Just mounting/loading once (visiting the Getting Started step) must NOT
    // cascade into the caller's own recheck.
    expect(onChanged).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: /Anthropic \(API key\) actions/i }));
    await user.click(await screen.findByRole("menuitem", { name: /delete integration/i }));
    await user.click(await screen.findByRole("button", { name: /^delete integration$/i }));

    // The delete's own reload (a REAL mutation) does fire it.
    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1));
  });
});

describe("IntegrationsScreen — Tools tab", () => {
  it("switching tabs shows the Tools table and the LAW footer", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus());
    getSiteConfigMock.mockResolvedValue({});
    listSecretsMock.mockResolvedValue([]);
    const user = userEvent.setup();

    renderScreen();
    await screen.findByText(T.EMPTY_TITLE);
    await user.click(screen.getByRole("tab", { name: "Tools" }));

    expect(await screen.findByText("git")).toBeInTheDocument();
    expect(screen.getByText(T.LAW)).toBeInTheDocument();
  });
});
