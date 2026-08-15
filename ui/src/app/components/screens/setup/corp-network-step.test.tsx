/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import type { ComponentProps } from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { HostProxyDetection, SiteConfig } from "../../../lib/types";
import { T, EGRESS_SUGGEST } from "../../../lib/integrations";
import { HttpError } from "../../../lib/api/core";
import { OperatorProvider } from "../../wardyn/operator-context";
import {
  CorpNetworkStep,
  compactEndpoint,
  hasUserinfo,
  isProxyConfigured,
  proxyDetected,
  type CorpStepActions,
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

// The zero-value gate: nothing probed yet. Most tests don't care about gate
// reporting, so this keeps their props terse; the tests that DO care pass
// their own gate/onGateChange override.
type GateSlice = Pick<CorpNetworkState, "proxyProbe" | "probeRunning" | "customDraft" | "redirectProbes">;
function unsetGate(): GateSlice {
  return { probeRunning: false, customDraft: "", redirectProbes: {} };
}

// Stateful mini-orchestrator: customDraft (and the rest of the gate slice) is
// CONTROLLED from setup-screen in the real app — typing in the custom field
// only works when onGateChange patches loop back into `gate`. Every patch is
// also forwarded to the test's own spy when one was passed.
function Harness(props: Partial<ComponentProps<typeof CorpNetworkStep>>) {
  const { gate: initial, onGateChange: spy, ...rest } = props;
  const [gate, setGate] = React.useState<GateSlice>({ ...unsetGate(), ...initial });
  return (
    <CorpNetworkStep
      status={baseStatus()}
      siteConfig={null}
      reloadSiteConfig={vi.fn().mockResolvedValue(undefined)}
      saveSiteConfig={vi.fn().mockResolvedValue(undefined)}
      {...rest}
      gate={gate}
      onGateChange={(patch) => {
        spy?.(patch);
        setGate((g) => ({ ...g, ...patch }));
      }}
    />
  );
}

function renderStep(props: Partial<ComponentProps<typeof CorpNetworkStep>> = {}) {
  const saveSiteConfig = vi.fn().mockResolvedValue(undefined);
  const reloadSiteConfig = vi.fn().mockResolvedValue(undefined);
  const utils = render(
    <MemoryRouter>
      <OperatorProvider operator>
        <Harness saveSiteConfig={saveSiteConfig} reloadSiteConfig={reloadSiteConfig} {...props} />
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

  // UI-SETUP-8: switching to URL mode on a secret-only proxy used to keep
  // asking `configured` (either field) but display ONLY upstream_proxy_url —
  // true, so-configured, but undefined, so the status line rendered
  // "Chaining through" a Mono with nothing in it: an affirmative claim naming
  // no value at all.
  it("switching to URL mode on a secret-only proxy still names the secret — never 'Chaining through' nothing", async () => {
    renderStep({ siteConfig: { upstream_proxy_secret_ref: "my-proxy-secret" } });
    await userEvent.click(screen.getByRole("button", { name: /enter a url instead/i }));
    expect(screen.getByText(/chaining through the url in secret/i)).toBeInTheDocument();
    expect(screen.getByText("my-proxy-secret")).toBeInTheDocument();
  });

  // UI-SETUP-7: Save on an empty Proxy URL field unconditionally PUTs both
  // upstream_proxy_url AND upstream_proxy_secret_ref undefined — fine when
  // nothing was configured, a silent delete of a real corporate proxy
  // otherwise (exactly what switching from the secret field via "Enter a URL
  // instead" without typing anything leaves on screen).
  it("Save is disabled on an empty URL once a proxy is already configured — never a silent clear", async () => {
    renderStep({ siteConfig: { upstream_proxy_secret_ref: "my-proxy-secret" } });
    await userEvent.click(screen.getByRole("button", { name: /enter a url instead/i }));
    expect(screen.getByLabelText(/proxy url/i)).toHaveValue("");
    expect(screen.getByRole("button", { name: /^save$/i })).toBeDisabled();
  });

  it("'Use a stored secret instead' disclosure toggles the secret-name field into view", async () => {
    renderStep();
    expect(screen.queryByLabelText(/secret name/i)).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: /use a stored secret instead/i }));
    expect(screen.getByLabelText(/secret name/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /add secret…/i })).toBeInTheDocument();
  });

  // W12-W12-C-3: upstream_proxy_secret_ref has no lifecycle integrity — a
  // secret it names can be deleted or rotated away from the Secrets screen,
  // which has no idea this reference exists, and this step kept claiming
  // "Chaining through the URL in secret X" forever after. secretNames (the
  // store's own live list, already fetched by the orchestrator) is a
  // presence check that catches it.
  it("a configured secret ref that no longer resolves in the store renders a warning, not a stale success claim", () => {
    renderStep({ siteConfig: { upstream_proxy_secret_ref: "corp-proxy" }, secretNames: [] });
    expect(screen.getByText(/no longer exists in the store/i)).toBeInTheDocument();
    expect(screen.getByText("corp-proxy")).toBeInTheDocument();
    expect(screen.queryByText(/chaining through the url in secret/i)).not.toBeInTheDocument();
  });

  it("a configured secret ref that DOES resolve keeps the normal 'Chaining through' claim", () => {
    renderStep({ siteConfig: { upstream_proxy_secret_ref: "corp-proxy" }, secretNames: ["corp-proxy"] });
    expect(screen.getByText(/chaining through the url in secret/i)).toBeInTheDocument();
    expect(screen.queryByText(/no longer exists in the store/i)).not.toBeInTheDocument();
  });

  it("secretNames omitted (standalone render) never claims a real ref is dangling — unknown, not empty", () => {
    renderStep({ siteConfig: { upstream_proxy_secret_ref: "corp-proxy" } }); // no secretNames prop at all
    expect(screen.getByText(/chaining through the url in secret/i)).toBeInTheDocument();
    expect(screen.queryByText(/no longer exists in the store/i)).not.toBeInTheDocument();
  });

  // W12-W12-C-3 (overwrite half): AddSecretDialog only warns before an
  // overwrite when it's HANDED the existing-names list — the corp-network
  // step's own "Add secret…" dialog used to open with none, so saving over
  // an in-use name here silently clobbered it with no warning at all (every
  // other AddSecretDialog caller in the app already passes this).
  it("'Add secret…' passes the store's real names through, so saving over an in-use name warns first", async () => {
    // Default secretName state is "upstream-proxy-url" (the dialog opens
    // locked to it) — put that exact name in the store already.
    renderStep({ secretNames: ["upstream-proxy-url"] });
    await userEvent.click(screen.getByRole("button", { name: /use a stored secret instead/i }));
    await userEvent.click(screen.getByRole("button", { name: /add secret…/i }));
    expect(await screen.findByText(/already exists/i)).toBeInTheDocument();
  });

  // W13-S1-1: the server's graded host_proxy check (Detail/Fix) is the ONLY
  // place that explains a loopback-bound proxy is unreachable from a sandbox —
  // and it used to render only on the Review step, which sits BEHIND this
  // step's own mandatory gate. Surface it here, where the operator is stuck.
  it("a loopback-bound host_proxy check's Detail/Fix render in place of EVIDENCE_NONE when nothing was detected", () => {
    renderStep({
      status: baseStatus({
        checks: [
          {
            id: "host_proxy",
            label: "Host proxy",
            status: "warn",
            detail: "Detected an env/shell/OS proxy setting. 127.0.0.1:8080 is bound to loopback, which a sandbox cannot reach.",
            fix: "Run `wardyn setup proxy-relay <listen-port> <proxy-port>` on the host and store the relay address as the upstream-proxy secret.",
          },
        ],
      }),
    });
    expect(screen.queryByText(T.EVIDENCE_NONE)).not.toBeInTheDocument();
    expect(screen.getByText(/bound to loopback/i)).toBeInTheDocument();
    expect(screen.getByText(/wardyn setup proxy-relay/i)).toBeInTheDocument();
  });

  it("a loopback-bound host_proxy check's Detail/Fix render BESIDE detected evidence rows too, not only in their absence", () => {
    const detection: HostProxyDetection = {
      has_credentials: false,
      http_proxy: { value: "http://127.0.0.1:8080", source: "env", has_credentials: false },
    };
    renderStep({
      status: baseStatus({
        host_proxy: detection,
        checks: [
          {
            id: "host_proxy",
            label: "Host proxy",
            status: "warn",
            detail: "127.0.0.1:8080 is bound to loopback, which a sandbox cannot reach.",
            fix: "Run `wardyn setup proxy-relay` on the host.",
          },
        ],
      }),
    });
    // The row is still real evidence (also shown by the single-candidate
    // "Use detected proxy" suggestion below it — getAllByText per the same
    // two-places convention as the differing-candidates case above)...
    expect(screen.getAllByText("http://127.0.0.1:8080").length).toBeGreaterThan(0);
    // ...but the warn diagnosis sits right beside it, not hidden behind Review.
    expect(screen.getByText(/bound to loopback/i)).toBeInTheDocument();
  });

  // W13-S1-1 (part 2): Save alone can never get back to "no proxy" once one is
  // configured (it disables itself on an empty field to guard against an
  // accidental clear) — an unreachable saved proxy needs its own, explicit way
  // out right on the screen that set it, or the mandatory gate downstream
  // never clears either.
  it("'Remove proxy' clears a configured URL — the explicit exit Save alone can't provide", async () => {
    const { saveSiteConfig } = renderStep({ siteConfig: { upstream_proxy_url: "http://127.0.0.1:8080" } });
    await userEvent.click(screen.getByRole("button", { name: /^remove proxy$/i }));
    await waitFor(() =>
      expect(saveSiteConfig).toHaveBeenCalledWith(
        expect.objectContaining({ upstream_proxy_url: undefined, upstream_proxy_secret_ref: undefined }),
      ),
    );
  });

  it("'Remove proxy' does not render when nothing is configured", () => {
    renderStep();
    expect(screen.queryByRole("button", { name: /^remove proxy$/i })).not.toBeInTheDocument();
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

  // UI-SETUP-5: this block's own "Re-check" used to reload site-config only
  // (reloadSiteConfig), which carries no host-proxy detection at all — the
  // rows above it could never actually refresh from the button sitting right
  // there. onRecheck is the orchestrator's FULL recheck (status + site-config,
  // the same one the persistent host-status strip's identically-labelled
  // button already calls).
  it("Re-check calls the orchestrator's full recheck, not merely reloadSiteConfig", async () => {
    const onRecheck = vi.fn();
    renderStep({ onRecheck });
    await userEvent.click(screen.getByRole("button", { name: /^re-check$/i }));
    expect(onRecheck).toHaveBeenCalledTimes(1);
  });

  it("falls back to reloadSiteConfig when onRecheck is omitted (standalone/test renders)", async () => {
    const { reloadSiteConfig } = renderStep();
    reloadSiteConfig.mockClear(); // drop the mount-effect call, isolate the button click
    await userEvent.click(screen.getByRole("button", { name: /^re-check$/i }));
    expect(reloadSiteConfig).toHaveBeenCalledTimes(1);
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
    const btn = screen.getByRole("button", { name: /^test connectivity$/i });
    await userEvent.click(btn);

    const runningBtn = await screen.findByRole("button", { name: /^testing…$/i });
    expect(runningBtn).toBeDisabled();

    resolve({ state: "reached", detail: T.TEST_OK });
    expect(await screen.findByText("Reached · via proxy")).toBeInTheDocument();
    expect(screen.getByText(T.TEST_OK)).toBeInTheDocument();
    expect(screen.getByText(T.TEST_STANDING)).toBeInTheDocument();
  });

  it("proxy Test: blocked and no_runner read their own real detail, never a generic failure", async () => {
    testProxyMock.mockResolvedValueOnce({ state: "blocked", detail: T.TEST_BLOCKED });
    renderStep();
    await userEvent.click(screen.getByRole("button", { name: /^test connectivity$/i }));
    expect(await screen.findByText("Blocked")).toBeInTheDocument();
    expect(screen.getByText(T.TEST_BLOCKED)).toBeInTheDocument();
  });

  it("proxy Test: no_runner (e.g. a 404-mapped older server) never reads as a pass", async () => {
    testProxyMock.mockResolvedValueOnce({ state: "no_runner", detail: T.TEST_NORUNNER });
    renderStep();
    await userEvent.click(screen.getByRole("button", { name: /^test connectivity$/i }));
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
    await userEvent.click(screen.getByRole("button", { name: /^test connectivity$/i }));

    await screen.findByRole("button", { name: /^test connectivity$/i });
    expect(screen.queryByText("Blocked")).not.toBeInTheDocument();
    expect(screen.queryByText(/^Reached/)).not.toBeInTheDocument();
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

  it("zero redirects renders the quiet empty-state line — no box, no check icon dressed as an answer (no rung demands a visit)", async () => {
    renderStep();
    await userEvent.click(screen.getByRole("tab", { name: /egress redirection/i }));
    expect(screen.getByText(T.EGRESS_DESC)).toBeInTheDocument();
    expect(screen.getByText(T.EGRESS_SEEN_EMPTY)).toBeInTheDocument();
    expect(screen.queryByText("No redirects on this network")).not.toBeInTheDocument();
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

  // WIRE-3: an integration-backed redirect used to render identically to an
  // untokened one — the operator concluded the token was lost.
  it("an integration-sourced token renders its own distinct chip, not the bare-secret one", async () => {
    renderEgress({
      egress_redirects: [{ from: "artifactory.corp.internal", to: "mirror.corp.internal", token_integration_ref: "corp-artifactory" }],
    });
    await userEvent.click(screen.getByRole("tab", { name: /egress redirection/i }));
    expect(screen.queryByText("token")).not.toBeInTheDocument();
    expect(screen.getByText("integration")).toHaveAttribute(
      "title",
      expect.stringContaining("token via integration: corp-artifactory"),
    );
  });

  // WIRE-3: typing a bare secret name into the expanded editor's Token field
  // and saving used to leave the row's OWN token_integration_ref standing
  // (spread from `...r`) alongside the freshly-set token_secret_ref — a
  // both-token-sources body the server hard-400s.
  it("saving a bare token typed over an integration-sourced row clears token_integration_ref", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const { saveSiteConfig, container } = renderEgress({
      egress_redirects: [{ from: "artifactory.corp.internal", to: "mirror.corp.internal", token_integration_ref: "corp-artifactory" }],
    });
    await user.click(screen.getByRole("tab", { name: /egress redirection/i }));
    await user.click(screen.getByText("artifactory.corp.internal"));
    // Two "Token secret name" fields are on screen at once (this row's editor
    // + the always-present AddRedirectForm below it) — the edit form's is the
    // one with the stable id the component ships (corp-network-egress.tsx).
    const tokenInput = container.querySelector<HTMLInputElement>("#eg-edit-token")!;
    await user.type(tokenInput, "artifactory-token");
    await user.click(screen.getByRole("button", { name: /^save$/i }));

    await waitFor(() =>
      expect(saveSiteConfig).toHaveBeenCalledWith(
        expect.objectContaining({
          egress_redirects: [
            expect.objectContaining({ token_secret_ref: "artifactory-token", token_integration_ref: undefined }),
          ],
        }),
      ),
    );
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

  // UI-SETUP-14: the "From" <Field label> pointed htmlFor="eg-add-from" at an
  // id the combobox trigger never carried — every sibling field here (To,
  // Token secret name) is wired correctly, so a screen-reader user tabbing
  // this form heard THOSE announced by name and this one only by its
  // placeholder content.
  it("the From field has an accessible name — getByLabelText resolves it, not just a placeholder", async () => {
    renderStep();
    await userEvent.click(screen.getByRole("tab", { name: /egress redirection/i }));
    expect(screen.getByLabelText("From")).toBeInTheDocument();
    expect(screen.getByLabelText("From")).toHaveAttribute("role", "combobox");
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

  // UI-SETUP-9: the fire-once guard used to be a ref INSIDE EgressTab, which
  // the step's tab ternary unmounts on every switch away — a fresh mount
  // reset the ref to 0 while the lifted testAllSignal counter (owned by
  // CorpNetworkStep) still held the LAST dispatch, so returning to the tab
  // read it as new and re-fired the whole real-sandbox sweep, unrequested.
  it("leaving the Egress tab and coming back does not re-fire 'Test all' — each dispatch runs exactly once", async () => {
    testRedirectMock.mockResolvedValue({ state: "reached", detail: "reachable via the mirror" });
    let actions: CorpStepActions | null = null;
    renderStep({
      siteConfig: {
        egress_redirects: [{ from: "https://registry.npmjs.org", to: "https://artifactory.corp.internal/api/npm/npm-remote" }],
      },
      registerActions: (a) => {
        actions = a;
      },
    });

    await act(async () => actions!.testRedirects());
    await waitFor(() => expect(testRedirectMock).toHaveBeenCalledTimes(1));

    // Switch away (unmounts EgressTab) and back (remounts it fresh).
    await userEvent.click(screen.getByRole("tab", { name: /host proxy/i }));
    await userEvent.click(screen.getByRole("tab", { name: /egress redirection/i }));

    // Still exactly one real probe — no unrequested re-sweep from the remount.
    expect(testRedirectMock).toHaveBeenCalledTimes(1);
  });
});

// ------------------------------------------------------------
// The redirect editor's identity — keyed by `from`, matching testStates/
// redirectProbes, not a positional array index.
// ------------------------------------------------------------
describe("Egress redirect editor — expansion keyed by `from`, not a shifting index (UI-SETUP-2)", () => {
  // A minimal stateful stand-in for the orchestrator's own siteConfig
  // round-trip: mutate()/saveSiteConfig "land" by writing straight back into
  // React state, so a Remove/Save click is actually REFLECTED in what
  // renders next — renderStep()'s mocked saveSiteConfig never feeds back,
  // which would hide this exact bug (the list would just stay [A,B,C]).
  function StatefulEgress({ initial }: { initial: SiteConfig["egress_redirects"] }) {
    const [redirects, setRedirects] = React.useState(initial);
    return (
      <MemoryRouter>
        <OperatorProvider operator>
          <CorpNetworkStep
            status={baseStatus()}
            siteConfig={{ egress_redirects: redirects }}
            reloadSiteConfig={vi.fn().mockResolvedValue(undefined)}
            saveSiteConfig={async (next) => {
              setRedirects(next.egress_redirects ?? []);
            }}
            gate={unsetGate()}
            onGateChange={vi.fn()}
            tab="egress"
          />
        </OperatorProvider>
      </MemoryRouter>
    );
  }

  it("removing an earlier row while a later one is expanded keeps editing the SAME row, and leaves its sibling untouched", async () => {
    const redirects: SiteConfig["egress_redirects"] = [
      { from: "a.example.com", to: "mirror-a.corp.internal" },
      { from: "b.example.com", to: "mirror-b.corp.internal" },
      { from: "c.example.com", to: "mirror-c.corp.internal" },
    ];
    render(<StatefulEgress initial={redirects} />);

    // Expand B.
    await userEvent.click(screen.getByText("b.example.com"));
    expect(await screen.findByDisplayValue("b.example.com")).toBeInTheDocument();

    // Remove A — with an index-keyed expansion this shifts B into A's old
    // slot and C into B's, so the STILL-mounted expanded editor (key=1) would
    // silently start showing/saving C's values instead.
    await userEvent.click(screen.getByRole("button", { name: /remove a\.example\.com redirect/i }));
    await waitFor(() => expect(screen.queryByText("a.example.com")).not.toBeInTheDocument());

    // Still editing B's own values.
    expect(screen.getByDisplayValue("b.example.com")).toBeInTheDocument();
    expect(screen.getByDisplayValue("mirror-b.corp.internal")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: /^save$/i }));

    // C survived, unedited and undestroyed — never overwritten by B's values.
    expect(await screen.findByTitle("c.example.com → mirror-c.corp.internal")).toBeInTheDocument();
    expect(screen.queryByTitle("c.example.com → mirror-b.corp.internal")).not.toBeInTheDocument();
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
describe("Gate reporting — proxy test and redirect tests report upward (no visit tracking: no rung demands one)", () => {
  it("a reached proxy test calls onGateChange with the full result (probeRunning batched off in the same patch)", async () => {
    testProxyMock.mockResolvedValueOnce({ state: "reached", detail: "reached in 42ms" });
    const onGateChange = vi.fn();
    renderStep({ onGateChange });
    await userEvent.click(screen.getByRole("button", { name: /^test connectivity$/i }));
    await screen.findByText("Reached · via proxy");
    expect(onGateChange).toHaveBeenCalledWith({ proxyProbe: { state: "reached", detail: "reached in 42ms" }, probeRunning: false });
    // ...and it announced the in-flight window first, so the gate can render
    // "Probe in flight" instead of a stale block reason.
    expect(onGateChange).toHaveBeenCalledWith({ probeRunning: true });
  });

  it("a FAILED REQUEST never reports a probe VERDICT — only the running window opens and closes", async () => {
    testProxyMock.mockRejectedValueOnce(new Error("403 operator role required"));
    const onGateChange = vi.fn();
    renderStep({ onGateChange });
    await userEvent.click(screen.getByRole("button", { name: /^test connectivity$/i }));
    await screen.findByText("Not tested");
    for (const call of onGateChange.mock.calls) {
      expect(call[0]).not.toHaveProperty("proxyProbe");
    }
  });

  it("re-entering the step seeds the proxy panel from gate.proxyProbe instead of a false 'Not tested'", () => {
    renderStep({ gate: { ...unsetGate(), proxyProbe: { state: "reached", detail: "reached in 42ms" } } });
    expect(screen.getByText("Reached · via proxy")).toBeInTheDocument();
    expect(screen.getByText("reached in 42ms")).toBeInTheDocument();
    // No re-test needed to see it — the button is idle, offering a re-test.
    expect(screen.getByRole("button", { name: /^test again$/i })).toBeEnabled();
  });

  // UI-SETUP-3: re-entering the step MID-probe (jump to another rail step and
  // back while the first click's sandbox is still out) used to seed only from
  // gate.proxyProbe, which is undefined until the probe resolves — the panel
  // read "Not tested" with an ENABLED Test button while the footer, reading
  // the same gate.probeRunning the orchestrator carries, said "Probe in
  // flight". Clicking the panel's button launched a duplicate probe.
  it("re-entering the step MID-probe seeds 'Testing…' (disabled) instead of a false 'Not tested' with an enabled button", () => {
    renderStep({ gate: { ...unsetGate(), probeRunning: true } });
    expect(screen.getByText(/starting a throwaway sandbox/i)).toBeInTheDocument();
    expect(screen.queryByText("Not tested")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^testing…$/i })).toBeDisabled();
  });

  it("switching tabs reports NOTHING — the gate has no visit rung to feed", async () => {
    const onGateChange = vi.fn();
    renderStep({ onGateChange });
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

  // The bug "Test all" hit with 2+ redirects: onProbeResult used to
  // materialize its patch from the render-time gate.redirectProbes, so
  // concurrent results clobbered each other and only the LAST survived — the
  // mandatory gate could never unlock even with every row green. Both `from`
  // keys must accumulate regardless of resolve order.
  it("two configured redirects, both tested via 'Test all': BOTH `from` keys accumulate in the gate", async () => {
    testRedirectMock.mockImplementation(async (from: string) => ({ state: "reached", detail: `reached ${from}` }));
    const onGateChange = vi.fn();
    renderStep({
      siteConfig: {
        egress_redirects: [
          { from: "https://registry.npmjs.org", to: "https://artifactory.corp.internal/api/npm/npm-remote" },
          { from: "https://pypi.org/simple", to: "https://artifactory.corp.internal/api/pypi/simple" },
        ],
      },
      onGateChange,
    });
    await userEvent.click(screen.getByRole("tab", { name: /egress redirection/i }));
    await userEvent.click(await screen.findByRole("button", { name: /^test all$/i }));

    await waitFor(() => expect(testRedirectMock).toHaveBeenCalledTimes(2));
    expect(await screen.findAllByText("Reached")).toHaveLength(2);

    // The gate's LATEST accumulated state carries both — never just whichever
    // redirect's probe happened to resolve last.
    const lastPatch = onGateChange.mock.calls.at(-1)![0] as { redirectProbes: Record<string, unknown> };
    expect(lastPatch.redirectProbes).toEqual({
      "https://registry.npmjs.org": { state: "reached", detail: "reached https://registry.npmjs.org" },
      "https://pypi.org/simple": { state: "reached", detail: "reached https://pypi.org/simple" },
    });
  });
});

// ------------------------------------------------------------
// Custom-URL block — the escape for a host with no public internet. Only
// surfaced after a real failure, never up front (T.CUSTOM_URL_WHY), and a
// pass through it must never wear the verified treatment (T.CUSTOM_CAVEAT).
// ------------------------------------------------------------
describe("Custom-URL block — only after a blocked result, weaker claim on a pass, inline server reject", () => {
  it("does not appear before testing, or once the test reaches — only a real 'blocked' surfaces it", async () => {
    testProxyMock.mockResolvedValueOnce({ state: "reached", detail: "reached in 42ms" });
    renderStep();
    expect(screen.queryByPlaceholderText(/nexus\.corp\.internal/i)).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: /^test connectivity$/i }));
    await screen.findByText("Reached · via proxy");
    expect(screen.queryByPlaceholderText(/nexus\.corp\.internal/i)).not.toBeInTheDocument();
  });

  it("no_runner does not offer the custom-URL block either — there is nothing to retry with", async () => {
    testProxyMock.mockResolvedValueOnce({ state: "no_runner", detail: T.TEST_NORUNNER });
    renderStep();
    await userEvent.click(screen.getByRole("button", { name: /^test connectivity$/i }));
    await screen.findByText("Can't test here");
    expect(screen.queryByPlaceholderText(/nexus\.corp\.internal/i)).not.toBeInTheDocument();
  });

  it("appears after a blocked result; a custom pass renders 'Request completed' + the caveat, never the success chip", async () => {
    testProxyMock.mockResolvedValueOnce({ state: "blocked", detail: T.TEST_BLOCKED });
    renderStep();
    await userEvent.click(screen.getByRole("button", { name: /^test connectivity$/i }));
    await screen.findByText("Blocked");
    expect(screen.getByText("No public endpoint will answer here?")).toBeInTheDocument();
    expect(screen.getByText(T.CUSTOM_URL_WHY)).toBeInTheDocument();

    testProxyMock.mockResolvedValueOnce({ state: "reached", detail: T.TEST_OK_CUSTOM, custom: true });
    const field = screen.getByPlaceholderText(/nexus\.corp\.internal/i);
    await userEvent.type(field, "https://intranet.example.com");
    await userEvent.click(screen.getByRole("button", { name: /^test this url$/i }));

    expect(await screen.findByText("Request completed")).toBeInTheDocument();
    expect(screen.getByText(T.CUSTOM_CAVEAT)).toBeInTheDocument();
    // The weaker claim must never wear the builtin pass's clothes.
    expect(screen.queryByText(/^Reached ·/)).not.toBeInTheDocument();
    expect(testProxyMock).toHaveBeenLastCalledWith("https://intranet.example.com");
    // The retry box goes away with the rest of the blocked-only UI.
    expect(screen.queryByPlaceholderText(/nexus\.corp\.internal/i)).not.toBeInTheDocument();
  });

  it("the retry button stays disabled until a URL is typed", async () => {
    testProxyMock.mockResolvedValueOnce({ state: "blocked", detail: T.TEST_BLOCKED });
    renderStep();
    await userEvent.click(screen.getByRole("button", { name: /^test connectivity$/i }));
    await screen.findByText("Blocked");
    expect(screen.getByRole("button", { name: /^test this url$/i })).toBeDisabled();
  });

  it("a server-rejected custom URL renders INLINE with the server's own message — the blocked result stays, no gate update", async () => {
    testProxyMock.mockResolvedValueOnce({ state: "blocked", detail: T.TEST_BLOCKED });
    const onGateChange = vi.fn();
    renderStep({ onGateChange });
    await userEvent.click(screen.getByRole("button", { name: /^test connectivity$/i }));
    await screen.findByText("Blocked");
    onGateChange.mockClear();

    testProxyMock.mockRejectedValueOnce(
      new HttpError(400, "url: must be a plain http(s) URL with a real host, and no shell metacharacters"),
    );
    await userEvent.type(screen.getByPlaceholderText(/nexus\.corp\.internal/i), "ftp://mirror.corp.internal/pub");
    await userEvent.click(screen.getByRole("button", { name: /^test this url$/i }));

    expect(
      await screen.findByText(/Rejected: “ftp:\/\/mirror\.corp\.internal\/pub” — url: must be a plain http\(s\) URL/),
    ).toBeInTheDocument();
    expect(screen.getByText(T.CUSTOM_REJECT_WHY)).toBeInTheDocument();
    // The failed result that revealed the block is still on screen — the
    // operator is mid-recovery, not starting over.
    expect(screen.getByText("Blocked")).toBeInTheDocument();
    expect(screen.getByText(T.TEST_BLOCKED)).toBeInTheDocument();
    // No verdict was observed, so no proxyProbe patch ever reports upward
    // (customDraft keystrokes and the probeRunning window do, and should).
    for (const call of onGateChange.mock.calls) {
      expect(call[0]).not.toHaveProperty("proxyProbe");
    }
  });

  it("an intercepted blocked renders apart — its own chip, the what-it-means box, and the custom escape", async () => {
    testProxyMock.mockResolvedValueOnce({ state: "blocked", detail: "Something answered…", intercepted: true });
    renderStep();
    await userEvent.click(screen.getByRole("button", { name: /^test connectivity$/i }));
    expect(await screen.findByText("Blocked · intercepted")).toBeInTheDocument();
    expect(screen.getByText(T.INTERCEPT_MEANS)).toBeInTheDocument();
    expect(screen.getByText(T.PROBE_ENDPOINTS)).toBeInTheDocument();
    // Intercepted is a blocked flavor: the custom-URL escape applies to it too.
    expect(screen.getByText("No public endpoint will answer here?")).toBeInTheDocument();
  });
});

// ------------------------------------------------------------
// One Test button per screen + the footer's fix-it actions. While the
// orchestrator's gate (gateResult) is offering an action, the panel's own
// button — and the custom block's — is suppressed: the gate row is the one
// launch point. The actions the step registers are what that row dispatches.
// ------------------------------------------------------------
describe("One launch point — gateResult hides the panel button; registered actions drive the step", () => {
  const offWithProbe = { on: false, head: "x", reason: "y", tone: "warning", action: { label: "Test connectivity", kind: "probe" } } as const;

  it("hides the panel's Test button while the gate row carries the action, and shows it again once the gate is on", () => {
    renderStep({ gateResult: offWithProbe });
    expect(screen.queryByRole("button", { name: /^test connectivity$/i })).not.toBeInTheDocument();
    // The hint/result column still renders — the block loses only the button.
    expect(screen.getByText("Not tested")).toBeInTheDocument();

    cleanup();
    renderStep({
      gate: { ...unsetGate(), proxyProbe: { state: "reached", detail: "ok", via: "proxy" } },
      gateResult: { on: true },
    });
    expect(screen.getByRole("button", { name: /^test again$/i })).toBeInTheDocument();
  });

  it("suppresses the custom block's own button too — Enter in the field fires the probe instead", async () => {
    // Seeded straight into blocked: the custom block is visible, buttonless.
    renderStep({
      gate: { ...unsetGate(), proxyProbe: { state: "blocked", detail: T.TEST_BLOCKED } },
      gateResult: offWithProbe,
    });
    expect(screen.getByText("No public endpoint will answer here?")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^test this url$/i })).not.toBeInTheDocument();

    testProxyMock.mockResolvedValueOnce({ state: "reached", detail: T.TEST_OK_CUSTOM, custom: true });
    await userEvent.type(screen.getByPlaceholderText(/nexus\.corp\.internal/i), "https://intranet.example.com{Enter}");
    expect(await screen.findByText("Request completed")).toBeInTheDocument();
    expect(testProxyMock).toHaveBeenLastCalledWith("https://intranet.example.com");
  });

  it("registers probe/probeCustom/openEgress/testRedirects, and they drive the real machinery", async () => {
    let actions: CorpStepActions | null = null;
    testProxyMock.mockResolvedValue({ state: "blocked", detail: T.TEST_BLOCKED });
    testRedirectMock.mockResolvedValue({ state: "reached", detail: "reachable via the mirror" });
    renderStep({
      siteConfig: {
        egress_redirects: [
          { from: "https://registry.npmjs.org", to: "https://artifactory.corp.internal/api/npm/npm-remote" },
          { from: "https://pypi.org/simple", to: "https://artifactory.corp.internal/api/pypi/simple" },
        ],
      },
      gateResult: offWithProbe,
      registerActions: (a) => {
        actions = a;
      },
    });
    expect(actions).not.toBeNull();

    // probe(): the builtin check, no argument.
    await act(async () => actions!.probe());
    await screen.findByText("Blocked");
    expect(testProxyMock).toHaveBeenLastCalledWith(undefined);

    // probeCustom(): probes what's typed (the harness loops customDraft back).
    testProxyMock.mockResolvedValueOnce({ state: "reached", detail: T.TEST_OK_CUSTOM, custom: true });
    await userEvent.type(screen.getByPlaceholderText(/nexus\.corp\.internal/i), "https://intranet.example.com");
    await act(async () => actions!.probeCustom());
    await screen.findByText("Request completed");
    expect(testProxyMock).toHaveBeenLastCalledWith("https://intranet.example.com");

    // testRedirects(): switches to the egress tab and fires EVERY row's probe.
    await act(async () => actions!.testRedirects());
    expect(await screen.findAllByText("Reached")).toHaveLength(2);
    expect(testRedirectMock).toHaveBeenCalledTimes(2);

    // openEgress(): just the tab switch (already there from the sweep above —
    // prove it works from the proxy tab too).
    await userEvent.click(screen.getByRole("tab", { name: /host proxy/i }));
    await act(async () => actions!.openEgress());
    expect(await screen.findByRole("button", { name: /\+ add redirect/i })).toBeInTheDocument();
  });
});
