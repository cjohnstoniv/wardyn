/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { baseStatus } from "../setup/test-fixtures";
import { AI_TYPES, SUBSCRIPTION_LANE_META } from "../../../lib/integrations";
import { T } from "../../../lib/integrations";
import { OperatorProvider } from "../../wardyn/operator-context";

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({ setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) } }));

const getSiteConfigMock = vi.fn();
const putSiteConfigMock = vi.fn();
// No testProxy/testRedirect here on purpose: the Test probes left this page with
// Host proxy / Egress redirection. Corporate network owns them, and its own
// suite (corp-network-step.test.tsx) is where they're covered.
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

describe("IntegrationsScreen — the two category sections", () => {
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
    // The other section is empty — it shows its OWN T.EMPTY_* line.
    expect(screen.getByText(T.EMPTY_SCM)).toBeInTheDocument();
    // …and there is no third or fourth section to be empty.
    expect(screen.queryByRole("region", { name: "Egress redirection" })).not.toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Host proxy" })).not.toBeInTheDocument();
  });

  it("shows the big empty state (title + body + CTA) only when NOTHING is configured", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus());
    getSiteConfigMock.mockResolvedValue({});
    listSecretsMock.mockResolvedValue([]);

    renderScreen();

    await screen.findByText(T.EMPTY_TITLE);
    expect(screen.getByText(T.EMPTY_BODY, { exact: false })).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: /add integration/i }).length).toBeGreaterThan(0);
    // The per-category sections do not render in this branch.
    expect(screen.queryByText(T.EMPTY_SCM)).toBeNull();
    // The pointer at Corporate network does — where the two removed sections
    // went is exactly what an empty page invites someone to ask.
    expect(screen.getByText(T.CORP_POINTER)).toBeInTheDocument();
  });

  // The consolidation, pinned where it is most visible: a site config carrying
  // a mirror AND a proxy renders neither here. Both live on Corporate network.
  it("renders both sections, and no mirror/proxy row even when the site config has them", async () => {
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ secrets: { present: ["anthropic-api-key", "git-pat-github-com"], github_app: false } }),
    );
    getSiteConfigMock.mockResolvedValue({
      scm_hosts: ["github.com"],
      egress_redirects: [{ from: "https://registry.npmjs.org", to: "https://artifactory.corp.internal/api/npm/x" }],
      artifact_overrides: { pip: { base_url: "https://artifactory.corp.internal/api/pip/x" } },
      upstream_proxy_secret_ref: "upstream-proxy-url",
    });
    listSecretsMock.mockResolvedValue(["anthropic-api-key", "git-pat-github-com"]);

    renderScreen();

    await screen.findByText("Anthropic (API key)");
    expect(screen.getByText("GitHub")).toBeInTheDocument();
    expect(screen.queryByText("artifactory.corp.internal")).not.toBeInTheDocument();
    expect(screen.queryByText("registry.npmjs.org")).not.toBeInTheDocument();
    // The Host proxy row's own compact "wardyn-proxy → …" mono line is gone too.
    expect(screen.queryByText("wardyn-proxy")).not.toBeInTheDocument();
    // No Test button survives on this page (T.FOOTNOTE now names one exception).
    expect(screen.queryByRole("button", { name: "Test" })).not.toBeInTheDocument();
    expect(screen.getByText(T.CORP_POINTER)).toBeInTheDocument();
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
    // The banner names ONE place, so it takes you there — the old "or add the
    // Host proxy integration here" alternative went with the category.
    expect(screen.getByRole("button", { name: /^open corporate network$/i })).toBeInTheDocument();
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
    // …except the page-side pointer at Corporate network — inside Getting
    // Started the step renders its own, backwards-pointing half of that pair
    // (T.EMBED_SCOPE_NOTE), and two of them stacked would just be a duplicate.
    expect(screen.queryByText(T.CORP_POINTER)).not.toBeInTheDocument();
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

// The seam that actually broke, twice, pinned end to end through the screen:
// Add → search → pick. The first break sent an Anthropic click to the old
// category grid ("AI provider or SCM host?"); the second sent it to the API-key
// connect panel with the Claude subscription nowhere in sight. The law both
// broke: never re-ask an answered question, never skip a real one.
describe("IntegrationsScreen — the Add handoff seam", () => {
  it("Add → Anthropic lands on the type panel with Claude subscription visible, never the category grid", async () => {
    const user = userEvent.setup();
    getSetupStatusMock.mockResolvedValue(baseStatus());
    renderScreen();
    await screen.findByText(T.EMPTY_TITLE);

    await user.click(screen.getAllByRole("button", { name: /add integration/i })[0]);
    await user.type(screen.getByRole("textbox", { name: /search integration types/i }), "anthropic");
    await user.click(await screen.findByText("Anthropic", { exact: true }));

    // The real question Anthropic leaves open — key or subscription — with the
    // subscription actually offered. And the dead grid stays dead.
    expect(await screen.findByText(AI_TYPES.anthropic_subscription.title)).toBeInTheDocument();
    expect(screen.queryByText(T.CAT_AI)).not.toBeInTheDocument();
    expect(screen.queryByText(T.CAT_SCM)).not.toBeInTheDocument();
  });

  it("searching 'subscription' finds the Claude subscription directly and opens preselected on it", async () => {
    const user = userEvent.setup();
    getSetupStatusMock.mockResolvedValue(baseStatus());
    renderScreen();
    await screen.findByText(T.EMPTY_TITLE);

    await user.click(screen.getAllByRole("button", { name: /add integration/i })[0]);
    await user.type(screen.getByRole("textbox", { name: /search integration types/i }), "subscription");
    await user.click(await screen.findByText("Claude subscription"));

    // Preselection makes the managed-vs-host-login lane choice render — it
    // only exists on the SELECTED row.
    expect(await screen.findByText(SUBSCRIPTION_LANE_META.managed.title)).toBeInTheDocument();
  });

  it("Add → OpenAI skips the type panel outright — no question is left to ask", async () => {
    const user = userEvent.setup();
    getSetupStatusMock.mockResolvedValue(baseStatus());
    renderScreen();
    await screen.findByText(T.EMPTY_TITLE);

    await user.click(screen.getAllByRole("button", { name: /add integration/i })[0]);
    await user.type(screen.getByRole("textbox", { name: /search integration types/i }), "codex");
    await user.click(await screen.findByText("OpenAI", { exact: true }));

    expect(screen.queryByText(AI_TYPES.anthropic_subscription.title)).not.toBeInTheDocument();
    expect(screen.queryByText(T.CAT_AI)).not.toBeInTheDocument();
  });
});
