/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Split from corp-network-step.test.tsx (#195): that file was over the 800-line
// test gate. The trusted-CA count, pure helpers, probe-state, and
// egress-row/combobox describes stay there; the redirect-editor identity, the
// mandatory-step gate, gate reporting, one-launch-point actions, and the
// per-verdict headline describes live here.
import * as React from "react";
import type { ComponentProps } from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { SiteConfig } from "../../../lib/types";
import { T } from "../../../lib/integrations";
import { OperatorProvider } from "../../wardyn/operator-context";
import { CorpNetworkStep, type CorpStepActions } from "./corp-network-step";
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
});

// The redirect editor's identity — keyed by `from`, matching testStates/
// redirectProbes, not a positional array index.
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

// The gate — no escape but the honest ones (no_runner, or naming what still
// needs fixing). "Skip this step" and the "Manage in Integrations" link are
// GONE: this step is mandatory, and the very next step IS Integrations, so
// the cross-link was noise.
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

// The gate itself — reporting probe/visit facts upward via onGateChange, and
// seeding back from a prior visit's gate (the whole point of lifting this
// state: CorpNetworkStep unmounts when the operator navigates away).
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

  // Re-entering the step MID-probe (jump to another rail step and back while
  // the first click's sandbox is still out) must not seed only from
  // gate.proxyProbe, which is undefined until the probe resolves — otherwise
  // the panel reads "Not tested" with an ENABLED Test button while the
  // footer, reading the same gate.probeRunning the orchestrator carries,
  // says "Probe in flight", and clicking the panel's button launches a
  // duplicate probe.
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

  // "Test all" with 2+ redirects: onProbeResult must not materialize its
  // patch from the render-time gate.redirectProbes, or concurrent results
  // clobber each other and only the last survives — the mandatory gate could
  // then never unlock even with every row green. Both `from` keys must
  // accumulate regardless of resolve order.
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

// One Test button per screen + the footer's fix-it actions. While the
// orchestrator's gate (gateResult) is offering an action, the panel's own
// button — and the custom block's — is suppressed: the gate row is the one
// launch point. The actions the step registers are what that row dispatches.
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


// One verdict, one heading, one owner of the fix.
//
// The probe verdict must not render as a bare chip inside the PROXY panel
// while its headline and advice hang off the step footer — that would make
// a run that never started read under the proxy's heading with the proxy's
// instruction. Every branch below asserts the verdict names itself and names
// what to do next, in the panel, beside the result.
describe("Corporate network — each probe verdict under its own heading", () => {
  beforeEach(() => {
    cleanup();
    testProxyMock.mockReset();
    testRedirectMock.mockReset();
  });

  async function probe(result: unknown) {
    testProxyMock.mockResolvedValueOnce(result);
    renderStep();
    await userEvent.click(screen.getByRole("button", { name: /^test connectivity$/i }));
  }

  it("blocked: the proxy's own headline AND the proxy's own next action, in the panel", async () => {
    await probe({ state: "blocked", detail: T.TEST_BLOCKED });
    expect(await screen.findByText(T.GATE_HEAD_BLOCKED)).toBeInTheDocument();
    // The advice must render in the panel, not only in the step footer.
    expect(screen.getByText(T.GATE_BLOCKED)).toBeInTheDocument();
  });

  it("intercepted: its own headline, its own meaning, and its own next action — never the plain blocked one", async () => {
    await probe({ state: "blocked", detail: T.TEST_BLOCKED, intercepted: true });
    expect(await screen.findByText(T.GATE_HEAD_INTERCEPTED)).toBeInTheDocument();
    expect(screen.getByText(T.INTERCEPT_MEANS)).toBeInTheDocument();
    expect(screen.getByText(T.GATE_INTERCEPTED)).toBeInTheDocument();
    expect(screen.queryByText(T.GATE_HEAD_BLOCKED)).not.toBeInTheDocument();
  });

  it("timed_out: a RUNNER headline, not a proxy one — and the wire detail still names the cause", async () => {
    await probe({ state: "timed_out", detail: T.TEST_TIMED_OUT });
    expect(await screen.findByText(T.GATE_HEAD_TIMED_OUT)).toBeInTheDocument();
    // The chip the demo/e2e narration reads stays exactly where it was.
    expect(screen.getByText("Probe never reported back")).toBeInTheDocument();
    expect(screen.getByText(T.TEST_TIMED_OUT)).toBeInTheDocument();
    expect(screen.getByText(T.TIMED_OUT_NOTE)).toBeInTheDocument();
    // The whole point: no proxy headline, and no proxy instruction.
    expect(screen.queryByText(T.GATE_HEAD_BLOCKED)).not.toBeInTheDocument();
    expect(screen.queryByText(T.GATE_BLOCKED)).not.toBeInTheDocument();
  });

  it("not_run: a RUNNER headline, and the launch failure quoted verbatim", async () => {
    await probe({ state: "not_run", detail: T.TEST_NOT_RUN });
    expect(await screen.findByText(T.GATE_HEAD_NOT_RUN)).toBeInTheDocument();
    expect(screen.getByText("Never ran")).toBeInTheDocument();
    expect(screen.getByText(T.TEST_NOT_RUN)).toBeInTheDocument();
    expect(screen.getByText(T.NOT_RUN_NOTE)).toBeInTheDocument();
    expect(screen.queryByText(T.GATE_HEAD_BLOCKED)).not.toBeInTheDocument();
    expect(screen.queryByText(T.GATE_BLOCKED)).not.toBeInTheDocument();
  });

  it("no_runner: its own headline, and the sentence that says what to configure", async () => {
    await probe({ state: "no_runner", detail: T.TEST_NORUNNER });
    expect(await screen.findByText(T.GATE_HEAD_NORUNNER)).toBeInTheDocument();
    expect(screen.getByText(T.TEST_NORUNNER)).toBeInTheDocument();
  });

  it("reached: no headline and no advice — a pass is not a failure state", async () => {
    await probe({ state: "reached", detail: T.TEST_OK, via: "proxy" });
    expect(await screen.findByText("Reached · via proxy")).toBeInTheDocument();
    for (const head of [
      T.GATE_HEAD_BLOCKED,
      T.GATE_HEAD_INTERCEPTED,
      T.GATE_HEAD_TIMED_OUT,
      T.GATE_HEAD_NOT_RUN,
      T.GATE_HEAD_NORUNNER,
      T.GATE_HEAD_UNTESTED,
    ]) {
      expect(screen.queryByText(head)).not.toBeInTheDocument();
    }
  });

  it("only a blocked verdict is warning-toned — a runner verdict that proves nothing must not read as amber", async () => {
    const frame = (text: string) => screen.getByText(text).closest("div.rounded-lg.border")!;

    await probe({ state: "blocked", detail: T.TEST_BLOCKED });
    await screen.findByText(T.GATE_HEAD_BLOCKED);
    expect(frame(T.GATE_HEAD_BLOCKED).className).toContain("border-warning/30");

    cleanup();
    await probe({ state: "timed_out", detail: T.TEST_TIMED_OUT });
    await screen.findByText(T.GATE_HEAD_TIMED_OUT);
    expect(frame(T.GATE_HEAD_TIMED_OUT).className).not.toContain("border-warning");

    cleanup();
    await probe({ state: "not_run", detail: T.TEST_NOT_RUN });
    await screen.findByText(T.GATE_HEAD_NOT_RUN);
    expect(frame(T.GATE_HEAD_NOT_RUN).className).not.toContain("border-warning");
  });
});
