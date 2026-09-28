/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import type { ComponentProps } from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { SiteConfig } from "../../../lib/types";
import { T, EGRESS_SUGGEST } from "../../../lib/integrations";
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
import { baseStatus } from "../../../lib/test-fixtures";
import { SITE } from "../../wardyn/copy";

const testProxyMock = vi.fn();
const testRedirectMock = vi.fn();
vi.mock("../../../lib/api/health", () => ({
  health: {
    testProxy: (...a: unknown[]) => testProxyMock(...a),
    testRedirect: (...a: unknown[]) => testRedirectMock(...a),
  },
}));

const toastSuccessMock = vi.fn();
vi.mock("sonner", () => ({
  toast: { success: (...a: unknown[]) => toastSuccessMock(...a), error: vi.fn() },
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
  toastSuccessMock.mockReset();
});

// F22: the trusted-CA count (WARDYN_TRUSTED_CA_FILE), a bare number with no
// PEM content or host name — renders unconditionally, never redacted.
describe("the trusted-CA count", () => {
  // ticket: F22
  it("renders the pluralised count when the daemon reports one", () => {
    renderStep({ status: baseStatus({ trusted_ca_certs: 3 }) });
    expect(screen.getByText("3 trusted CA certificates")).toBeInTheDocument();
  });

  it("renders nothing when the daemon sends none (0/absent)", () => {
    renderStep({ status: baseStatus() });
    expect(screen.queryByText(/trusted CA certificate/)).not.toBeInTheDocument();
  });
});

// Pure helpers
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

// Test probes — five states, both buttons disable+relabel while running
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

  it("proxy Test: not_run (the probe sandbox never launched) never renders as 'Blocked'", async () => {
    testProxyMock.mockResolvedValueOnce({ state: "not_run", detail: T.TEST_NOT_RUN });
    renderStep();
    await userEvent.click(screen.getByRole("button", { name: /^test connectivity$/i }));
    expect(await screen.findByText("Never ran")).toBeInTheDocument();
    expect(screen.getByText(T.TEST_NOT_RUN)).toBeInTheDocument();
    expect(screen.getByText(T.NOT_RUN_NOTE)).toBeInTheDocument();
    // The distinguishing regression this state exists to fix: a launch
    // failure must never read as the classic corp-network "Blocked".
    expect(screen.queryByText("Blocked")).not.toBeInTheDocument();
    // Nothing was actually tested from a sandbox, so the standing note must
    // not appear either (same rule as no_runner).
    expect(screen.queryByText(T.TEST_STANDING)).not.toBeInTheDocument();
  });

  it("proxy Test: timed_out (the sandbox ran but never reported back) never renders as 'Blocked'", async () => {
    testProxyMock.mockResolvedValueOnce({ state: "timed_out", detail: T.TEST_TIMED_OUT });
    renderStep();
    await userEvent.click(screen.getByRole("button", { name: /^test connectivity$/i }));
    expect(await screen.findByText("Probe never reported back")).toBeInTheDocument();
    expect(screen.getByText(T.TEST_TIMED_OUT)).toBeInTheDocument();
    expect(screen.getByText(T.TIMED_OUT_NOTE)).toBeInTheDocument();
    // The distinguishing regression this state exists to fix: a run that
    // started and ran must never read as the classic corp-network "Blocked".
    expect(screen.queryByText("Blocked")).not.toBeInTheDocument();
    // Nothing was actually proven by a run that never reported completion, so
    // the standing note must not appear either (same rule as not_run).
    expect(screen.queryByText(T.TEST_STANDING)).not.toBeInTheDocument();
  });

  it("a reached verdict's warning renders as a caution line under it — the recording-upload risk, not a network verdict", async () => {
    testProxyMock.mockResolvedValueOnce({ state: "reached", detail: T.TEST_OK, via: "proxy", warning: T.TEST_OK_WARNING });
    renderStep();
    await userEvent.click(screen.getByRole("button", { name: /^test connectivity$/i }));
    expect(await screen.findByText("Reached · via proxy")).toBeInTheDocument();
    expect(screen.getByText(T.TEST_OK_WARNING)).toBeInTheDocument();
  });

  it("a reached verdict with no warning renders no caution line", async () => {
    testProxyMock.mockResolvedValueOnce({ state: "reached", detail: T.TEST_OK, via: "proxy" });
    renderStep();
    await userEvent.click(screen.getByRole("button", { name: /^test connectivity$/i }));
    expect(await screen.findByText("Reached · via proxy")).toBeInTheDocument();
    expect(screen.queryByText(T.TEST_OK_WARNING)).not.toBeInTheDocument();
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

// Egress redirection tab — row density, network-only chip, combobox
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

  // The row's only edit affordance (click-to-expand) must be keyboard
  // reachable and activatable, not a bare div with no role/tabIndex/onKeyDown.
  it("a redirect row is keyboard-focusable and Enter expands its editor (ui-setup-2)", async () => {
    const user = userEvent.setup();
    renderEgress();
    await user.click(screen.getByRole("tab", { name: /egress redirection/i }));

    // The row's computed accessible name also swallows its nested Test/Remove
    // buttons' text (one of which is literally "Remove ... ghcr.io redirect"),
    // so a name-based role query is ambiguous — key off the row's own title
    // instead, same identity the other row tests already use.
    const row = screen.getByTitle("https://ghcr.io → https://registry.corp.internal/ghcr-remote");
    expect(row).toHaveAttribute("role", "button");
    expect(row).toHaveAttribute("tabIndex", "0");
    row.focus();
    expect(row).toHaveFocus();
    await user.keyboard("{Enter}");
    // Expanding swaps the row for its editor, whose stable Cancel control only
    // exists once expanded.
    expect(screen.getByRole("button", { name: /^cancel$/i })).toBeInTheDocument();
  });

  // WIRE-3: an integration-backed redirect must not render identically to an
  // untokened one — that reads as the token having been lost.
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
  // and saving must clear the row's own token_integration_ref, not leave it
  // standing (spread from `...r`) alongside the freshly-set token_secret_ref
  // — a both-token-sources body the server hard-400s.
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

  // A duplicate `from` is producible today — no client/server guard existed,
  // and the rest of this file keys a redirect BY `from` (there is no server
  // id), so a collision would silently shadow one row's probe result with
  // the other's. Index-keying would shift every later row's verdict on
  // removal, so disabling Add on the collision is the smaller fix.
  it("Add is refused on a `from` collision with an existing redirect, and the reason is shown", async () => {
    // ticket: F3-F5
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderEgress();
    await user.click(screen.getByRole("tab", { name: /egress redirection/i }));
    await user.click(screen.getByRole("combobox"));
    // "https://pypi.org/simple" is already a well-known suggestion (not
    // "novel"), so it's picked from the list rather than typed as custom.
    await user.click(await screen.findByText("https://pypi.org/simple"));
    await user.type(screen.getByPlaceholderText(/artifactory\.corp\.internal/i), "https://mirror.corp.internal/pypi");

    expect(screen.getByRole("button", { name: /\+ add redirect/i })).toBeDisabled();
    expect(screen.getByText(/already exists for this from/i)).toBeInTheDocument();
  });

  // Negative control: a DIFFERENT `from` alongside the same redirects list is
  // never refused — the guard is a collision check, not a general lockout.
  it("negative control: a non-colliding `from` still enables Add, with no collision message", async () => {
    // ticket: F3-F5
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderEgress();
    await user.click(screen.getByRole("tab", { name: /egress redirection/i }));
    await user.click(screen.getByRole("combobox"));
    await user.type(screen.getByPlaceholderText(/https:\/\/…, host, or IP/i), "https://a-new-host.example.com");
    await user.click(await screen.findByText("use as typed"));
    await user.type(screen.getByPlaceholderText(/artifactory\.corp\.internal/i), "https://mirror.corp.internal/new");

    expect(screen.getByRole("button", { name: /\+ add redirect/i })).toBeEnabled();
    expect(screen.queryByText(/already exists for this from/i)).not.toBeInTheDocument();
  });

  // The client collision check must fold like the server's own
  // normalizeRedirectEndpoint (TrimSpace + lowercase scheme+authority) — a
  // different-CASE typed `from` is the SAME redirect once the server
  // normalizes it, and a client check that misses that hole lets the
  // operator create the exact silently-shadowed duplicate the collision
  // guard above exists to prevent.
  it("a different-case `from` (same authority once folded) is still a collision", async () => {
    // ticket: M1
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderEgress();
    await user.click(screen.getByRole("tab", { name: /egress redirection/i }));
    await user.click(screen.getByRole("combobox"));
    // Existing redirect is "https://pypi.org/simple" (lowercase) — typed
    // here with an upper-case authority, which is "novel" to the suggestion
    // list (exact-string match), so it goes through "use as typed".
    await user.type(screen.getByPlaceholderText(/https:\/\/…, host, or IP/i), "https://PyPI.org/simple");
    await user.click(await screen.findByText("use as typed"));
    await user.type(screen.getByPlaceholderText(/artifactory\.corp\.internal/i), "https://mirror.corp.internal/pypi");

    expect(screen.getByRole("button", { name: /\+ add redirect/i })).toBeDisabled();
    expect(screen.getByText(/already exists for this from/i)).toBeInTheDocument();
  });

  // Negative control: only the AUTHORITY folds — a path-case difference is a
  // genuinely different redirect target (paths are case-sensitive on most
  // servers), so it must NOT collide.
  it("negative control: a different-case PATH (same authority, different path case) does not collide", async () => {
    // ticket: M1
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    renderEgress();
    await user.click(screen.getByRole("tab", { name: /egress redirection/i }));
    await user.click(screen.getByRole("combobox"));
    await user.type(screen.getByPlaceholderText(/https:\/\/…, host, or IP/i), "https://pypi.org/Simple");
    await user.click(await screen.findByText("use as typed"));
    await user.type(screen.getByPlaceholderText(/artifactory\.corp\.internal/i), "https://mirror.corp.internal/pypi2");

    expect(screen.getByRole("button", { name: /\+ add redirect/i })).toBeEnabled();
    expect(screen.queryByText(/already exists for this from/i)).not.toBeInTheDocument();
  });

  // The "From" <Field label> must resolve to an id the combobox trigger
  // actually carries — every sibling field here (To, Token secret name)
  // is wired correctly, and a screen-reader user tabbing this form needs
  // this one announced by name too, not just by its placeholder content.
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

  // `egress_redirects` is compiled into the sidecar at dispatch, exactly
  // like the upstream proxy, so every redirect save (add, edit, remove)
  // must carry the "applies to runs started from now" note — a redirect
  // added mid-run must not read as already applied.
  describe("every redirect save carries the applies-from-now note", () => {
    // ticket: B2
    it("add", async () => {
      const user = userEvent.setup({ pointerEventsCheck: 0 });
      const { saveSiteConfig } = renderStep();
      await user.click(screen.getByRole("tab", { name: /egress redirection/i }));
      await user.click(screen.getByRole("combobox"));
      await user.click(await screen.findByText("https://registry.npmjs.org"));
      await user.type(screen.getByPlaceholderText(/artifactory\.corp\.internal/i), "https://artifactory.corp.internal/api/npm/npm-remote");
      await user.click(screen.getByRole("button", { name: /\+ add redirect/i }));
      await waitFor(() => expect(saveSiteConfig).toHaveBeenCalled());
      await waitFor(() => expect(toastSuccessMock).toHaveBeenCalledWith(expect.any(String), { description: SITE.SAVE_NOTE }));
    });

    it("remove", async () => {
      const user = userEvent.setup({ pointerEventsCheck: 0 });
      const { saveSiteConfig } = renderEgress({
        egress_redirects: [{ from: "https://registry.npmjs.org", to: "https://artifactory.corp.internal/api/npm/npm-remote" }],
      });
      await user.click(screen.getByRole("tab", { name: /egress redirection/i }));
      await user.click(screen.getByRole("button", { name: "Remove https://registry.npmjs.org redirect" }));
      await waitFor(() => expect(saveSiteConfig).toHaveBeenCalledWith(expect.objectContaining({ egress_redirects: [] })));
      await waitFor(() => expect(toastSuccessMock).toHaveBeenCalledWith(expect.any(String), { description: SITE.SAVE_NOTE }));
    });

    it("a failed save shows no note — the error toast is the only voice", async () => {
      const user = userEvent.setup({ pointerEventsCheck: 0 });
      const { saveSiteConfig } = renderEgress({
        egress_redirects: [{ from: "https://registry.npmjs.org", to: "https://artifactory.corp.internal/api/npm/npm-remote" }],
      });
      saveSiteConfig.mockRejectedValueOnce(new Error("boom"));
      await user.click(screen.getByRole("tab", { name: /egress redirection/i }));
      await user.click(screen.getByRole("button", { name: "Remove https://registry.npmjs.org redirect" }));
      await waitFor(() => expect(saveSiteConfig).toHaveBeenCalled());
      expect(toastSuccessMock).not.toHaveBeenCalled();
    });
  });

  // The fire-once guard can't live as a ref inside EgressTab: the step's tab
  // ternary unmounts EgressTab on every switch away, so a fresh mount would
  // reset a local ref to 0 while the lifted testAllSignal counter (owned by
  // CorpNetworkStep) still holds the LAST dispatch — returning to the tab
  // would then read as new and re-fire the whole real-sandbox sweep,
  // unrequested.
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
