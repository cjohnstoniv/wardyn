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

  // ui-setup-2: the row's only edit affordance (click-to-expand) was a bare
  // div with no role/tabIndex/onKeyDown — unreachable and unactivatable from
  // the keyboard.
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

