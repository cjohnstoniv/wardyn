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
const testProxyMock = vi.fn();
const testRedirectMock = vi.fn();
vi.mock("../../../lib/api/health", () => ({
  health: {
    getSiteConfig: (...a: unknown[]) => getSiteConfigMock(...a),
    putSiteConfig: (...a: unknown[]) => putSiteConfigMock(...a),
    testProxy: (...a: unknown[]) => testProxyMock(...a),
    testRedirect: (...a: unknown[]) => testRedirectMock(...a),
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
    // T.EMPTY_MIRROR was retired with the Corporate-network restructure;
    // T.EMPTY_EGRESS is its replacement (integrations-screen.tsx CATEGORY_EMPTY).
    expect(screen.getByText(T.EMPTY_EGRESS)).toBeInTheDocument();
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
    // assert the row via its own compact "wardyn-proxy → …" mono line instead.
    expect(screen.getByText("wardyn-proxy")).toBeInTheDocument();
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

  // hideCategories (Corporate network restructure): additive and opt-in — the
  // two tests above never pass it, so their "keeps everything else" coverage
  // is untouched. This is the ONE caller that does (integrations-step.tsx).
  it("hideCategories omits host_proxy + artifact_mirror entirely, even when both have rows", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus({ secrets: { present: ["anthropic-api-key"], github_app: false } }));
    getSiteConfigMock.mockResolvedValue({
      artifact_overrides: { npm: { base_url: "https://artifactory.corp.internal/api/npm/x" } },
      upstream_proxy_secret_ref: "upstream-proxy-url",
    });
    listSecretsMock.mockResolvedValue(["anthropic-api-key"]);

    render(
      <MemoryRouter>
        <IntegrationsScreen embedded hideCategories={["host_proxy", "artifact_mirror"]} />
      </MemoryRouter>,
    );

    await screen.findByText("Anthropic (API key)");
    // Both hidden categories have a real row (per the siteConfig above) —
    // their content, and the proxy banner, must not render anyway.
    expect(screen.queryByText("artifactory.corp.internal")).not.toBeInTheDocument();
    expect(screen.queryByText("wardyn-proxy")).not.toBeInTheDocument();
    expect(screen.queryByText(T.EMPTY_EGRESS)).not.toBeInTheDocument();
    expect(screen.queryByText(T.EMPTY_PROXY)).not.toBeInTheDocument();
    // AI still renders — hiding two categories must not blank the whole page.
    expect(screen.getByText(T.FOOTNOTE)).toBeInTheDocument();
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

// BUG FIX (this wave): the category previously derived rows ONLY from the
// legacy artifact_overrides map, so anything saved through the Corporate
// network step (which writes egress_redirects) rendered nothing at all.
describe("IntegrationsScreen — Egress redirection rows (current egress_redirects shape)", () => {
  it("a redirect saved via the Corporate network step's shape now appears, renamed to 'Egress redirection'", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus());
    getSiteConfigMock.mockResolvedValue({
      egress_redirects: [{ from: "https://registry.npmjs.org", to: "https://artifactory.corp.internal/api/npm/npm-remote", ecosystem: "npm" }],
    });
    listSecretsMock.mockResolvedValue([]);

    renderScreen();

    expect(await screen.findByText("Egress redirection")).toBeInTheDocument();
    // Old label is gone.
    expect(screen.queryByText("Artifact mirrors")).not.toBeInTheDocument();
    expect(screen.getByText("registry.npmjs.org")).toBeInTheDocument();
  });

  it("compacts a long redirect path (host survives, ellipsis appears) but keeps the full from/to/token in the row's title", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus());
    const from = "https://pypi.org/simple";
    const to = "https://artifactory.corp.internal/api/pypi/pypi-remote/simple";
    getSiteConfigMock.mockResolvedValue({ egress_redirects: [{ from, to, token_secret_ref: "artifactory-token" }] });
    listSecretsMock.mockResolvedValue([]);

    renderScreen();

    const row = await screen.findByTitle(`${from} → ${to} · token: artifactory-token`);
    const toEl = within(row).getByText(/artifactory\.corp\.internal/);
    // Host survives verbatim in the compacted text…
    expect(toEl.textContent).toContain("artifactory.corp.internal");
    // …but it's shorter than the real value, elided with an ellipsis, scheme dropped.
    expect(toEl.textContent!.length).toBeLessThan(to.length);
    expect(toEl.textContent).toContain("…");
    expect(toEl.textContent).not.toContain("https://");
  });

  it("a redirect with no ecosystem gets the muted 'network only' chip; one WITH an ecosystem does not", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus());
    getSiteConfigMock.mockResolvedValue({
      egress_redirects: [
        { from: "telemetry.vendor-sdk.io", to: "10.40.2.11:8443" },
        { from: "https://registry.npmjs.org", to: "https://artifactory.corp.internal/api/npm/npm-remote", ecosystem: "npm" },
      ],
    });
    listSecretsMock.mockResolvedValue([]);

    renderScreen();

    await screen.findByText("registry.npmjs.org");
    // Exactly one row (the network-only one) carries the chip.
    expect(screen.getAllByText("network only")).toHaveLength(1);
  });
});

describe("IntegrationsScreen — Test probes (Host proxy row + Egress redirection rows)", () => {
  it("per-row Test on an egress redirect calls testRedirect(from, to), disables + relabels while running, then shows the real verdict", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus());
    getSiteConfigMock.mockResolvedValue({
      egress_redirects: [{ from: "https://registry.npmjs.org", to: "https://artifactory.corp.internal/api/npm/npm-remote" }],
    });
    listSecretsMock.mockResolvedValue([]);
    let resolveTest: (v: { state: "reached" | "blocked" | "bypass" | "no_runner"; detail: string }) => void = () => {};
    testRedirectMock.mockImplementation(
      () =>
        new Promise((resolve) => {
          resolveTest = resolve;
        }),
    );
    const user = userEvent.setup();

    renderScreen();
    await screen.findByText("registry.npmjs.org");

    await user.click(screen.getByRole("button", { name: "Test" }));

    expect(testRedirectMock).toHaveBeenCalledWith(
      "https://registry.npmjs.org",
      "https://artifactory.corp.internal/api/npm/npm-remote",
    );
    // Disables + relabels while running (matches the Corporate network step).
    expect(await screen.findByRole("button", { name: "Testing…" })).toBeDisabled();

    resolveTest({ state: "reached", detail: "Reached artifactory.corp.internal through the proxy in 240ms." });

    expect(await screen.findByText("Reached")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Test" })).not.toBeDisabled();
  });

  it("Test on the Host proxy row: the row is real and configured (not permanently empty) for a plain-URL proxy, and Blocked reads as its own verdict", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus());
    getSiteConfigMock.mockResolvedValue({ upstream_proxy_url: "http://proxy.corp.acme.com:8080" });
    listSecretsMock.mockResolvedValue([]);
    testProxyMock.mockResolvedValue({ state: "blocked", detail: T.TEST_BLOCKED });
    const user = userEvent.setup();

    renderScreen();
    // BUG FIX: a plain-URL proxy used to render as permanently empty here.
    await screen.findByText("wardyn-proxy");
    expect(screen.queryByText(T.EMPTY_PROXY)).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Test" }));

    expect(testProxyMock).toHaveBeenCalled();
    expect(await screen.findByText("Blocked")).toBeInTheDocument();
  });

  it("no_runner and bypass render as their own distinct verdicts, never a generic failure", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus());
    getSiteConfigMock.mockResolvedValue({
      egress_redirects: [{ from: "https://ghcr.io", to: "https://registry.corp.internal/ghcr-remote" }],
    });
    listSecretsMock.mockResolvedValue([]);
    testRedirectMock.mockResolvedValue({ state: "bypass", detail: T.TEST_BYPASS });
    const user = userEvent.setup();

    renderScreen();
    await screen.findByText("ghcr.io");
    await user.click(screen.getByRole("button", { name: "Test" }));

    expect(await screen.findByText("Redirect not enforced")).toBeInTheDocument();

    testRedirectMock.mockResolvedValue({ state: "no_runner", detail: T.TEST_NORUNNER });
    await user.click(screen.getByRole("button", { name: "Test" }));
    expect(await screen.findByText("Can't test here")).toBeInTheDocument();
  });

  it("a FAILED REQUEST is not a probe verdict — it must never render as 'Blocked' (Host proxy row)", async () => {
    // The whole point of the button is that a result means something. A 403,
    // a restarted wardynd, or a malformed payload never reached the network at
    // all, so reporting "Blocked" would blame a firewall that is working fine.
    getSetupStatusMock.mockResolvedValue(baseStatus());
    getSiteConfigMock.mockResolvedValue({ upstream_proxy_url: "http://proxy.corp.acme.com:8080" });
    listSecretsMock.mockResolvedValue([]);
    testProxyMock.mockRejectedValueOnce(new Error("403 operator role required"));
    const user = userEvent.setup();

    renderScreen();
    await screen.findByText("wardyn-proxy");
    await user.click(screen.getByRole("button", { name: "Test" }));

    await screen.findByRole("button", { name: "Test" });
    expect(screen.queryByText("Blocked")).not.toBeInTheDocument();
    expect(screen.queryByText("Reached")).not.toBeInTheDocument();
    // Falls back to untested, so the operator can retry.
    expect(screen.getByText("Not tested")).toBeInTheDocument();
  });

  it("a FAILED REQUEST is not a probe verdict — it must never render as 'Blocked' (Egress redirection row)", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus());
    getSiteConfigMock.mockResolvedValue({
      egress_redirects: [{ from: "https://registry.npmjs.org", to: "https://artifactory.corp.internal/api/npm/npm-remote" }],
    });
    listSecretsMock.mockResolvedValue([]);
    testRedirectMock.mockRejectedValueOnce(new Error("wardynd restarted mid-request"));
    const user = userEvent.setup();

    renderScreen();
    await screen.findByText("registry.npmjs.org");
    await user.click(screen.getByRole("button", { name: "Test" }));

    await screen.findByRole("button", { name: "Test" });
    expect(screen.queryByText("Blocked")).not.toBeInTheDocument();
    expect(screen.getByText("Not tested")).toBeInTheDocument();
  });

  it("viewer role disables both the proxy row's and an egress row's Test button", async () => {
    getSetupStatusMock.mockResolvedValue(baseStatus());
    getSiteConfigMock.mockResolvedValue({
      upstream_proxy_url: "http://proxy.corp.acme.com:8080",
      egress_redirects: [{ from: "https://ghcr.io", to: "https://registry.corp.internal/ghcr-remote" }],
    });
    listSecretsMock.mockResolvedValue([]);

    renderScreen(false);
    await screen.findByText("ghcr.io");

    for (const btn of screen.getAllByRole("button", { name: "Test" })) {
      expect(btn).toBeDisabled();
    }
  });
});
