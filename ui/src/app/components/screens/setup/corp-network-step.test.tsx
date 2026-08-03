/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { ComponentProps } from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { HostProxyDetection, SiteConfig } from "../../../lib/types";
import { T, EGRESS_SUGGEST } from "../../../lib/integrations";
import { OperatorProvider } from "../../wardyn/operator-context";
import {
  CorpNetworkStep,
  compactEndpoint,
  hasUserinfo,
  isProxyConfigured,
  proxyDetected,
} from "./corp-network-step";
import type { CorpNetworkState } from "./steps";
import { baseStatus } from "./test-fixtures";

const testProxyMock = vi.fn();
const testRedirectMock = vi.fn();
vi.mock("../../../lib/api/health", () => ({
  health: {
    testProxy: (...a: unknown[]) => testProxyMock(...a),
    testRedirect: (...a: unknown[]) => testRedirectMock(...a),
  },
}));

const setSecretMock = vi.fn();
vi.mock("../../../lib/api/secrets", () => ({
  secrets: { setSecret: (...a: unknown[]) => setSecretMock(...a) },
}));

// The zero-value gate: nothing probed, tab never visited. Most tests don't
// care about gate reporting, so this keeps their props terse; the tests that
// DO care pass their own gate/onGateChange override.
function unsetGate(): Pick<CorpNetworkState, "proxyProbe" | "egressVisited" | "redirectProbes"> {
  return { egressVisited: false, redirectProbes: {} };
}

function renderStep(props: Partial<ComponentProps<typeof CorpNetworkStep>> = {}) {
  const saveSiteConfig = vi.fn().mockResolvedValue(undefined);
  const reloadSiteConfig = vi.fn().mockResolvedValue(undefined);
  const utils = render(
    <MemoryRouter>
      <OperatorProvider operator>
        <CorpNetworkStep
          status={baseStatus()}
          siteConfig={null}
          reloadSiteConfig={reloadSiteConfig}
          saveSiteConfig={saveSiteConfig}
          gate={unsetGate()}
          onGateChange={vi.fn()}
          {...props}
        />
      </OperatorProvider>
    </MemoryRouter>,
  );
  return { ...utils, saveSiteConfig, reloadSiteConfig };
}

beforeEach(() => {
  testProxyMock.mockReset();
  testRedirectMock.mockReset();
  setSecretMock.mockReset().mockResolvedValue(undefined);
});

// ------------------------------------------------------------
// Pure helpers
// ------------------------------------------------------------
describe("pure helpers", () => {
  it("compactEndpoint leaves a short endpoint whole, scheme dropped", () => {
    expect(compactEndpoint("https://registry.npmjs.org")).toBe("registry.npmjs.org");
    expect(compactEndpoint("10.40.2.11:8443")).toBe("10.40.2.11:8443"); // bare host:port, no scheme
  });

  it("compactEndpoint elides only the MIDDLE of a long path, never the host", () => {
    const long = "https://artifactory.corp.internal/api/pypi/pypi-remote-mirror-extra-long-segment/simple";
    const out = compactEndpoint(long);
    expect(out.startsWith("artifactory.corp.internal")).toBe(true); // host intact
    expect(out).toContain("…");
    expect(out).not.toBe(long.replace("https://", "")); // actually shortened
  });

  it("compactEndpoint never truncates a bare long host with no path", () => {
    const longHost = "a-very-long-subdomain-name-that-keeps-going.corp.internal";
    expect(compactEndpoint(longHost)).toBe(longHost);
  });

  it("hasUserinfo detects a username:password in a URL, and only that", () => {
    expect(hasUserinfo("http://ops-egress:secret@proxy.corp.acme.com:8080")).toBe(true);
    expect(hasUserinfo("http://proxy.corp.acme.com:8080")).toBe(false);
    expect(hasUserinfo("not a url")).toBe(false);
  });

  it("isProxyConfigured / proxyDetected read SiteConfig honestly", () => {
    expect(isProxyConfigured(null)).toBe(false);
    expect(isProxyConfigured({ upstream_proxy_url: "http://p:8080" })).toBe(true);
    expect(isProxyConfigured({ upstream_proxy_secret_ref: "s" })).toBe(true);
    expect(proxyDetected(undefined)).toBe(false);
    expect(proxyDetected({ has_credentials: false })).toBe(false);
    expect(
      proxyDetected({ has_credentials: false, http_proxy: { value: "http://x:8080", source: "env", has_credentials: false } }),
    ).toBe(true);
  });
});

// ------------------------------------------------------------
// Host proxy tab — the six states the mock draws
// ------------------------------------------------------------
describe("Host proxy tab — panel states", () => {
  it("empty: nothing detected, nothing configured", () => {
    renderStep();
    expect(screen.getByText(T.EVIDENCE_NONE)).toBeInTheDocument();
    expect(screen.getByText(T.NOT_CONFIGURED)).toBeInTheDocument();
    expect(screen.queryByText("Use detected proxy")).not.toBeInTheDocument();
  });

  it("detected: a single candidate offers one-click 'Use detected proxy'", async () => {
    const detection: HostProxyDetection = {
      has_credentials: false,
      http_proxy: { value: "http://proxy.corp.acme.com:8080", source: "env", has_credentials: false },
    };
    const { saveSiteConfig } = renderStep({ status: baseStatus({ host_proxy: detection }) });
    expect(screen.getByText(T.NOT_CONFIGURED)).toBeInTheDocument();
    const lead = screen.getByRole("button", { name: /use detected proxy/i });
    await userEvent.click(lead);
    await waitFor(() =>
      expect(saveSiteConfig).toHaveBeenCalledWith(expect.objectContaining({ upstream_proxy_url: "http://proxy.corp.acme.com:8080" })),
    );
  });

  it("choice: differing candidates offer radio rows + 'Use selected', never the single-lead button", () => {
    const detection: HostProxyDetection = {
      has_credentials: false,
      http_proxy: { value: "http://proxy-a.corp.acme.com:8080", source: "env", has_credentials: false },
      https_proxy: { value: "http://proxy-b.corp.acme.com:8443", source: "env", has_credentials: false },
    };
    renderStep({ status: baseStatus({ host_proxy: detection }) });
    expect(screen.queryByRole("button", { name: /use detected proxy/i })).not.toBeInTheDocument();
    expect(screen.getByText(/detected values differ/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /use selected/i })).toBeInTheDocument();
    // Each value appears at least twice on purpose: once in the evidence block
    // (which always lists everything found) and once as its own radio choice.
    expect(screen.getAllByText("http://proxy-a.corp.acme.com:8080").length).toBeGreaterThan(0);
    expect(screen.getAllByText("http://proxy-b.corp.acme.com:8443").length).toBeGreaterThan(0);
  });

  it("configured: an existing plain URL shows the success status line, editable", () => {
    renderStep({ siteConfig: { upstream_proxy_url: "http://proxy.corp.acme.com:8080" } });
    expect(screen.getByText("http://proxy.corp.acme.com:8080")).toBeInTheDocument();
    expect(screen.getByLabelText(/proxy url/i)).toHaveValue("http://proxy.corp.acme.com:8080");
  });

  it("cred: typing a URL with a username/password auto-switches the field to masked input and shows CRED_URL_NOTE", async () => {
    renderStep();
    const field = screen.getByLabelText(/proxy url/i);
    expect(field).toHaveAttribute("type", "text");
    await userEvent.type(field, "http://ops-egress:secret123@proxy.corp.acme.com:8080");
    expect(field).toHaveAttribute("type", "password");
    expect(screen.getByText(T.CRED_URL_NOTE)).toBeInTheDocument();
    expect(screen.getByText(T.WRITE_ONLY)).toBeInTheDocument();
  });

  it("cred: Save stores the raw value as a secret and references it, never as a plain URL", async () => {
    const { saveSiteConfig } = renderStep();
    const field = screen.getByLabelText(/proxy url/i);
    await userEvent.type(field, "http://ops-egress:secret123@proxy.corp.acme.com:8080");
    await userEvent.click(screen.getByRole("button", { name: /^save$/i }));
    await waitFor(() => expect(setSecretMock).toHaveBeenCalledWith("upstream-proxy-url", "http://ops-egress:secret123@proxy.corp.acme.com:8080"));
    await waitFor(() =>
      expect(saveSiteConfig).toHaveBeenCalledWith(
        expect.objectContaining({ upstream_proxy_secret_ref: "upstream-proxy-url", upstream_proxy_url: undefined }),
      ),
    );
  });

  it("secret: a siteConfig with only a secret ref opens the disclosure and never shows the URL (write-only)", () => {
    renderStep({ siteConfig: { upstream_proxy_secret_ref: "my-proxy-secret" } });
    expect(screen.getByText("my-proxy-secret")).toBeInTheDocument();
    expect(screen.getByText(T.SECRET_INSTEAD_HINT)).toBeInTheDocument();
    expect(screen.queryByLabelText(/proxy url/i)).not.toBeInTheDocument();
  });

  it("'Use a stored secret instead' disclosure toggles the secret-name field into view", async () => {
    renderStep();
    expect(screen.queryByLabelText(/secret name/i)).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: /use a stored secret instead/i }));
    expect(screen.getByLabelText(/secret name/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /add secret…/i })).toBeInTheDocument();
  });
});

// ------------------------------------------------------------
// Evidence rows — "Use this" wiring, and NO_PROXY's exception
// ------------------------------------------------------------
describe("Evidence — 'Use this' per row, NO_PROXY carries the note instead", () => {
  it("clicking a row's 'Use this' saves that row's exact value", async () => {
    const detection: HostProxyDetection = {
      has_credentials: false,
      http_proxy: { value: "http://proxy.corp.acme.com:8080", source: "env", has_credentials: false },
      https_proxy: { value: "http://proxy.corp.acme.com:8080", source: "env", has_credentials: false },
    };
    const { saveSiteConfig } = renderStep({ status: baseStatus({ host_proxy: detection }) });
    const useButtons = screen.getAllByRole("button", { name: /^use this$/i });
    await userEvent.click(useButtons[0]);
    await waitFor(() =>
      expect(saveSiteConfig).toHaveBeenCalledWith(expect.objectContaining({ upstream_proxy_url: "http://proxy.corp.acme.com:8080" })),
    );
  });

  it("NO_PROXY renders with T.NOPROXY_NOTE and never gets its own 'Use this' button", () => {
    const detection: HostProxyDetection = {
      has_credentials: false,
      http_proxy: { value: "http://proxy.corp.acme.com:8080", source: "env", has_credentials: false },
      no_proxy: { value: "localhost,127.0.0.1,.corp.acme.com", source: "env", has_credentials: false },
    };
    renderStep({ status: baseStatus({ host_proxy: detection }) });
    expect(screen.getByText(T.NOPROXY_NOTE)).toBeInTheDocument();
    // Exactly one usable row (http_proxy) — NO_PROXY must not add a second.
    expect(screen.getAllByRole("button", { name: /^use this$/i })).toHaveLength(1);
  });
});

// ------------------------------------------------------------
// Test probes — five states, both buttons disable+relabel while running
// ------------------------------------------------------------
describe("Test probes — real states, never a fake pass, running disables+relabels on BOTH buttons", () => {
  it("proxy Test: idle -> running (disabled, relabeled) -> reached", async () => {
    let resolve!: (v: { state: string; detail: string }) => void;
    testProxyMock.mockReturnValue(new Promise((r) => (resolve = r)));
    renderStep();

    expect(screen.getByText("Not tested")).toBeInTheDocument();
    const btn = screen.getByRole("button", { name: /^test proxy$/i });
    await userEvent.click(btn);

    const runningBtn = await screen.findByRole("button", { name: /^testing…$/i });
    expect(runningBtn).toBeDisabled();

    resolve({ state: "reached", detail: T.TEST_OK });
    expect(await screen.findByText("Reached")).toBeInTheDocument();
    expect(screen.getByText(T.TEST_OK)).toBeInTheDocument();
    expect(screen.getByText(T.TEST_STANDING)).toBeInTheDocument();
  });

  it("proxy Test: blocked and no_runner read their own real detail, never a generic failure", async () => {
    testProxyMock.mockResolvedValueOnce({ state: "blocked", detail: T.TEST_BLOCKED });
    renderStep();
    await userEvent.click(screen.getByRole("button", { name: /^test proxy$/i }));
    expect(await screen.findByText("Blocked")).toBeInTheDocument();
    expect(screen.getByText(T.TEST_BLOCKED)).toBeInTheDocument();
  });

  it("proxy Test: no_runner (e.g. a 404-mapped older server) never reads as a pass", async () => {
    testProxyMock.mockResolvedValueOnce({ state: "no_runner", detail: T.TEST_NORUNNER });
    renderStep();
    await userEvent.click(screen.getByRole("button", { name: /^test proxy$/i }));
    expect(await screen.findByText("Can't test here")).toBeInTheDocument();
    expect(screen.getByText(T.TEST_NORUNNER)).toBeInTheDocument();
    // The "tested from a real sandbox" note only makes sense once a probe
    // actually ran — it must not appear on the one state where none did.
    expect(screen.queryByText(T.TEST_STANDING)).not.toBeInTheDocument();
  });

  it("a FAILED REQUEST is not a probe verdict — it must never render as 'Blocked'", async () => {
    // The whole point of these buttons is that a result means something. A 403,
    // a restarted wardynd, or a malformed payload never reached the network at
    // all, so reporting "Blocked" would blame a firewall that is working fine.
    testProxyMock.mockRejectedValueOnce(new Error("403 operator role required"));
    renderStep();
    await userEvent.click(screen.getByRole("button", { name: /^test proxy$/i }));

    await screen.findByRole("button", { name: /^test proxy$/i });
    expect(screen.queryByText("Blocked")).not.toBeInTheDocument();
    expect(screen.queryByText("Reached")).not.toBeInTheDocument();
    // ...and it falls back to untested, so the operator can retry.
    expect(screen.getByText("Not tested")).toBeInTheDocument();
  });

  it("redirect row Test: disables + relabels to 'Testing…' while running — the mock left this always-on; both buttons must match", async () => {
    let resolve!: (v: { state: string; detail: string }) => void;
    testRedirectMock.mockReturnValue(new Promise((r) => (resolve = r)));
    renderStep({
      siteConfig: { egress_redirects: [{ from: "https://registry.npmjs.org", to: "https://artifactory.corp.internal/api/npm/npm-remote" }] },
    });
    await userEvent.click(screen.getByRole("tab", { name: /egress redirection/i }));

    const testBtn = await screen.findByRole("button", { name: /^test$/i });
    await userEvent.click(testBtn);

    const runningBtn = await screen.findByRole("button", { name: /^testing…$/i });
    expect(runningBtn).toBeDisabled();

    resolve({ state: "bypass", detail: T.TEST_BYPASS });
    expect(await screen.findByText("Redirect not enforced")).toBeInTheDocument();
  });
});

// ------------------------------------------------------------
// Egress redirection tab — row density, network-only chip, combobox
// ------------------------------------------------------------
describe("Egress redirection — rows, network-only chip, the From combobox", () => {
  const redirects: SiteConfig["egress_redirects"] = [
    {
      from: "https://pypi.org/simple",
      to: "https://artifactory.corp.internal/api/pypi/pypi-remote-mirror-extra-long-segment/simple",
      ecosystem: "pip",
      token_secret_ref: "artifactory-token",
    },
    { from: "https://ghcr.io", to: "https://registry.corp.internal/ghcr-remote" }, // no ecosystem -> network only
  ];

  function renderEgress(sc: SiteConfig = { egress_redirects: redirects }) {
    const utils = renderStep({ siteConfig: sc });
    return utils;
  }

  it("shows EMPTY_EGRESS with no redirects, EGRESS_DESC always", async () => {
    renderStep();
    await userEvent.click(screen.getByRole("tab", { name: /egress redirection/i }));
    expect(screen.getByText(T.EGRESS_DESC)).toBeInTheDocument();
    expect(screen.getByText(T.EMPTY_EGRESS)).toBeInTheDocument();
  });

  it("a long endpoint's row elides the middle, and the row's title carries the FULL from/to values", async () => {
    renderEgress();
    await userEvent.click(screen.getByRole("tab", { name: /egress redirection/i }));
    // Host is legible, un-elided; the compact form contains an ellipsis.
    const rows = screen.getAllByTitle(/artifactory\.corp\.internal\/api\/pypi\/pypi-remote-mirror-extra-long-segment\/simple/);
    expect(rows.length).toBeGreaterThan(0);
    expect(rows[0]).toHaveAttribute(
      "title",
      "https://pypi.org/simple → https://artifactory.corp.internal/api/pypi/pypi-remote-mirror-extra-long-segment/simple · token: artifactory-token",
    );
  });

  it("network-only chip appears on a redirect with no ecosystem, never on one with an ecosystem", async () => {
    renderEgress();
    await userEvent.click(screen.getByRole("tab", { name: /egress redirection/i }));
    expect(screen.getAllByText("network only")).toHaveLength(1);
  });

  it("the From combobox lists the suggested sources (label = URL, ecosystem a muted hint)", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderStep();
    await user.click(screen.getByRole("tab", { name: /egress redirection/i }));
    await user.click(screen.getByRole("combobox"));
    for (const [url] of EGRESS_SUGGEST.slice(0, 3)) {
      expect(await screen.findByText(url)).toBeInTheDocument();
    }
  });

  it("a CUSTOM host can be typed and saved — redirecting a private host or IP is the point of this tab", async () => {
    // The suggestions are a shortcut, not the menu. Without a CommandInput the
    // combobox was select-only, so an internal host or a bare IP — the reason
    // this stopped being "artifact registries" — could not be entered at all.
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const { saveSiteConfig } = renderStep();
    await user.click(screen.getByRole("tab", { name: /egress redirection/i }));
    await user.click(screen.getByRole("combobox"));

    await user.type(screen.getByPlaceholderText(/https:\/\/…, host, or IP/i), "telemetry.vendor-sdk.io");
    await user.click(await screen.findByText("use as typed"));

    await user.type(screen.getByPlaceholderText(/artifactory\.corp\.internal/i), "10.40.2.11:8443");
    await user.click(screen.getByRole("button", { name: /\+ add redirect/i }));

    await waitFor(() =>
      expect(saveSiteConfig).toHaveBeenCalledWith(
        expect.objectContaining({
          egress_redirects: [
            // No ecosystem: nothing writes a tool config for an arbitrary host,
            // so it lands in the network-only tier rather than silently
            // claiming a config file it can't generate.
            expect.objectContaining({ from: "telemetry.vendor-sdk.io", to: "10.40.2.11:8443", ecosystem: undefined }),
          ],
        }),
      ),
    );
  });

  it("adding a redirect from a suggested npm source auto-sets its ecosystem", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const { saveSiteConfig } = renderStep();
    await user.click(screen.getByRole("tab", { name: /egress redirection/i }));
    await user.click(screen.getByRole("combobox"));
    await user.click(await screen.findByText("https://registry.npmjs.org"));
    await user.type(screen.getByPlaceholderText(/artifactory\.corp\.internal/i), "https://artifactory.corp.internal/api/npm/npm-remote");
    await user.click(screen.getByRole("button", { name: /\+ add redirect/i }));
    await waitFor(() =>
      expect(saveSiteConfig).toHaveBeenCalledWith(
        expect.objectContaining({
          egress_redirects: [
            expect.objectContaining({ from: "https://registry.npmjs.org", ecosystem: "npm" }),
          ],
        }),
      ),
    );
  });
});

// ------------------------------------------------------------
// The gate — no escape but the honest ones (no_runner, or naming what still
// needs fixing). "Skip this step" and the "Manage in Integrations" link are
// GONE: this step is mandatory, and the very next step IS Integrations, so
// the cross-link was noise.
// ------------------------------------------------------------
describe("No skip control, no Integrations cross-link — this step is mandatory", () => {
  it("never renders a Skip button, configured or not", () => {
    const { rerender } = renderStep({ siteConfig: null });
    expect(screen.queryByRole("button", { name: /skip/i })).not.toBeInTheDocument();

    rerender(
      <MemoryRouter>
        <OperatorProvider operator>
          <CorpNetworkStep
            status={baseStatus()}
            siteConfig={{ upstream_proxy_url: "http://p:8080" }}
            reloadSiteConfig={vi.fn().mockResolvedValue(undefined)}
            saveSiteConfig={vi.fn().mockResolvedValue(undefined)}
            gate={unsetGate()}
            onGateChange={vi.fn()}
          />
        </OperatorProvider>
      </MemoryRouter>,
    );
    expect(screen.queryByRole("button", { name: /skip/i })).not.toBeInTheDocument();
  });

  it("never renders a 'Manage in Integrations' link", () => {
    renderStep();
    expect(screen.queryByRole("link", { name: /manage in integrations/i })).not.toBeInTheDocument();
  });
});

// ------------------------------------------------------------
// The gate itself — reporting probe/visit facts upward via onGateChange, and
// seeding back from a prior visit's gate (the whole point of lifting this
// state: CorpNetworkStep unmounts when the operator navigates away).
// ------------------------------------------------------------
describe("Gate reporting — proxy test, egress visit, and redirect tests all report upward", () => {
  it("a reached proxy test calls onGateChange with the full result", async () => {
    testProxyMock.mockResolvedValueOnce({ state: "reached", detail: "reached in 42ms" });
    const onGateChange = vi.fn();
    renderStep({ onGateChange });
    await userEvent.click(screen.getByRole("button", { name: /^test proxy$/i }));
    await screen.findByText("Reached");
    expect(onGateChange).toHaveBeenCalledWith({ proxyProbe: { state: "reached", detail: "reached in 42ms" } });
  });

  it("a FAILED REQUEST never reports a gate update — no verdict was actually observed", async () => {
    testProxyMock.mockRejectedValueOnce(new Error("403 operator role required"));
    const onGateChange = vi.fn();
    renderStep({ onGateChange });
    await userEvent.click(screen.getByRole("button", { name: /^test proxy$/i }));
    await screen.findByText("Not tested");
    expect(onGateChange).not.toHaveBeenCalled();
  });

  it("re-entering the step seeds the proxy panel from gate.proxyProbe instead of a false 'Not tested'", () => {
    renderStep({ gate: { ...unsetGate(), proxyProbe: { state: "reached", detail: "reached in 42ms" } } });
    expect(screen.getByText("Reached")).toBeInTheDocument();
    expect(screen.getByText("reached in 42ms")).toBeInTheDocument();
    // No re-test needed to see it — the button is idle, not mid-run.
    expect(screen.getByRole("button", { name: /^test proxy$/i })).toBeEnabled();
  });

  it("switching to the Egress redirection tab reports egressVisited", async () => {
    const onGateChange = vi.fn();
    renderStep({ onGateChange });
    await userEvent.click(screen.getByRole("tab", { name: /egress redirection/i }));
    expect(onGateChange).toHaveBeenCalledWith({ egressVisited: true });
    expect(onGateChange).toHaveBeenCalledTimes(1);
  });

  it("does not report egressVisited again when the gate already has it (re-entering an already-visited step)", async () => {
    const onGateChange = vi.fn();
    renderStep({ gate: { ...unsetGate(), egressVisited: true }, onGateChange });
    await userEvent.click(screen.getByRole("tab", { name: /egress redirection/i }));
    expect(onGateChange).not.toHaveBeenCalled();
  });

  it("testing a redirect reports its result upward keyed by `from`, merged with any prior entries", async () => {
    testRedirectMock.mockResolvedValueOnce({ state: "reached", detail: "reachable via the mirror" });
    const onGateChange = vi.fn();
    renderStep({
      siteConfig: { egress_redirects: [{ from: "https://registry.npmjs.org", to: "https://artifactory.corp.internal/api/npm/npm-remote" }] },
      gate: { ...unsetGate(), redirectProbes: { "https://pypi.org/simple": { state: "reached", detail: "old" } } },
      onGateChange,
    });
    await userEvent.click(screen.getByRole("tab", { name: /egress redirection/i }));
    await userEvent.click(await screen.findByRole("button", { name: /^test$/i }));
    await screen.findByText("Reached");
    expect(onGateChange).toHaveBeenCalledWith({
      redirectProbes: {
        "https://pypi.org/simple": { state: "reached", detail: "old" },
        "https://registry.npmjs.org": { state: "reached", detail: "reachable via the mirror" },
      },
    });
  });

  it("re-entering the step seeds each redirect row from gate.redirectProbes instead of resetting to 'Not tested'", async () => {
    renderStep({
      siteConfig: { egress_redirects: [{ from: "https://registry.npmjs.org", to: "https://artifactory.corp.internal/api/npm/npm-remote" }] },
      gate: { ...unsetGate(), redirectProbes: { "https://registry.npmjs.org": { state: "bypass", detail: "still reachable directly" } } },
    });
    await userEvent.click(screen.getByRole("tab", { name: /egress redirection/i }));
    expect(await screen.findByText("Redirect not enforced")).toBeInTheDocument();
    expect(screen.queryByText("Not tested")).not.toBeInTheDocument();
  });
});

// ------------------------------------------------------------
// Custom-URL retry — the escape for a host with no public internet. Only
// surfaced after a real failure, never up front (T.TEST_CUSTOM_HINT).
// ------------------------------------------------------------
describe("Custom-URL retry — only after a blocked result", () => {
  it("does not appear before testing, or once the test reaches — only a real 'blocked' surfaces it", async () => {
    testProxyMock.mockResolvedValueOnce({ state: "reached", detail: "reached in 42ms" });
    renderStep();
    expect(screen.queryByPlaceholderText(/an-internal-host-you-can-reach/i)).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: /^test proxy$/i }));
    await screen.findByText("Reached");
    expect(screen.queryByPlaceholderText(/an-internal-host-you-can-reach/i)).not.toBeInTheDocument();
  });

  it("no_runner does not offer the custom-URL retry either — there is nothing to retry with", async () => {
    testProxyMock.mockResolvedValueOnce({ state: "no_runner", detail: T.TEST_NORUNNER });
    renderStep();
    await userEvent.click(screen.getByRole("button", { name: /^test proxy$/i }));
    await screen.findByText("Can't test here");
    expect(screen.queryByPlaceholderText(/an-internal-host-you-can-reach/i)).not.toBeInTheDocument();
  });

  it("appears after a blocked result, and retrying with a URL calls testProxy(url)", async () => {
    testProxyMock.mockResolvedValueOnce({ state: "blocked", detail: T.TEST_BLOCKED });
    renderStep();
    await userEvent.click(screen.getByRole("button", { name: /^test proxy$/i }));
    await screen.findByText("Blocked");
    expect(screen.getByText(T.TEST_CUSTOM_HINT)).toBeInTheDocument();

    testProxyMock.mockResolvedValueOnce({ state: "reached", detail: "the request completed" });
    const field = screen.getByPlaceholderText(/an-internal-host-you-can-reach/i);
    await userEvent.type(field, "https://intranet.example.com");
    await userEvent.click(screen.getByRole("button", { name: /^test this url instead$/i }));

    expect(await screen.findByText("Reached")).toBeInTheDocument();
    expect(testProxyMock).toHaveBeenLastCalledWith("https://intranet.example.com");
    // Fixed now — the retry box goes away with the rest of the blocked-only UI.
    expect(screen.queryByPlaceholderText(/an-internal-host-you-can-reach/i)).not.toBeInTheDocument();
  });

  it("the retry button stays disabled until a URL is typed", async () => {
    testProxyMock.mockResolvedValueOnce({ state: "blocked", detail: T.TEST_BLOCKED });
    renderStep();
    await userEvent.click(screen.getByRole("button", { name: /^test proxy$/i }));
    await screen.findByText("Blocked");
    expect(screen.getByRole("button", { name: /^test this url instead$/i })).toBeDisabled();
  });
});
