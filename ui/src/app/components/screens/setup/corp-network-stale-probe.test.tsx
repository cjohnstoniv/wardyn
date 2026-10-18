/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A probe verdict must not outlive the target it tested (#2557, #2573), and a detected
// proxy whose password the server masked must never be saved as shown (#2572).
import * as React from "react";
import type { ComponentProps } from "react";
import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { HostProxyDetection } from "../../../lib/types";
import { T } from "../../../lib/integrations";
import { OperatorProvider } from "../../wardyn/operator-context";
import { CorpNetworkStep } from "./corp-network-step";
import type { CorpNetworkState } from "./steps";
import { baseStatus } from "../../../lib/test-fixtures";
import { setField } from "../../../../test/set-field";

vi.mock("../../../lib/api/health", () => ({ health: { testProxy: vi.fn(), testRedirect: vi.fn() } }));
vi.mock("../../../lib/api/secrets", () => ({ secrets: { setSecret: vi.fn() } }));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

type GateSlice = Pick<CorpNetworkState, "proxyProbe" | "probeRunning" | "customDraft" | "redirectProbes">;
const reached = { state: "reached", detail: "reached in 42ms" } as const;

function Harness(props: Partial<ComponentProps<typeof CorpNetworkStep>> & { spy?: (patch: Partial<GateSlice>) => void }) {
  const { gate: initial, spy, ...rest } = props;
  const [gate, setGate] = React.useState<GateSlice>({ probeRunning: false, customDraft: "", redirectProbes: {}, ...initial });
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

function renderStep(props: ComponentProps<typeof Harness> = {}) {
  const saveSiteConfig = vi.fn().mockResolvedValue(undefined);
  const spy = vi.fn();
  render(
    <MemoryRouter>
      <OperatorProvider operator>
        <Harness saveSiteConfig={saveSiteConfig} spy={spy} {...props} />
      </OperatorProvider>
    </MemoryRouter>,
  );
  return { saveSiteConfig, spy };
}

describe("a saved change drops the verdict it made stale", () => {
  it("removing the proxy forgets the held proxy probe (#2573)", async () => {
    const { spy } = renderStep({
      siteConfig: { upstream_proxy_url: "http://proxy-a.corp.acme.com:8080" },
      gate: { proxyProbe: reached, probeRunning: false, customDraft: "", redirectProbes: {} },
    });
    expect(screen.getByText(/reached in 42ms/i)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: /^remove proxy$/i }));
    await waitFor(() => expect(spy).toHaveBeenCalledWith({ proxyProbe: undefined }));
    expect(screen.queryByText(/reached in 42ms/i)).not.toBeInTheDocument();
  });

  it("editing a redirect's To forgets that redirect's verdict (#2557)", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const { spy } = renderStep({
      siteConfig: { egress_redirects: [{ from: "a.example.com", to: "mirror-a.corp.internal" }] },
      gate: { probeRunning: false, customDraft: "", redirectProbes: { "a.example.com": reached } },
      tab: "egress",
    });
    await user.click(screen.getByText("a.example.com"));
    setField(await screen.findByLabelText("To"), "mirror-typo.corp.internal");
    await user.click(screen.getByRole("button", { name: /^save$/i }));
    await waitFor(() => expect(spy).toHaveBeenCalledWith({ redirectProbes: {} }));
  });

  it("saving a redirect with an unchanged To keeps its verdict", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    const { saveSiteConfig, spy } = renderStep({
      siteConfig: { egress_redirects: [{ from: "a.example.com", to: "mirror-a.corp.internal" }] },
      gate: { probeRunning: false, customDraft: "", redirectProbes: { "a.example.com": reached } },
      tab: "egress",
    });
    await user.click(screen.getByText("a.example.com"));
    await user.click(await screen.findByRole("button", { name: /^save$/i }));
    await waitFor(() => expect(saveSiteConfig).toHaveBeenCalled());
    expect(spy).not.toHaveBeenCalledWith(expect.objectContaining({ redirectProbes: expect.anything() }));
  });
});

describe("a detected proxy whose password was masked (#2572)", () => {
  const masked: HostProxyDetection = {
    has_credentials: true,
    https_proxy: { value: "http://alice:***@proxy.corp:8080", source: "env", has_credentials: true },
  };

  it("offers no one-click save; 'Use this' prefills the username only and asks for the password", async () => {
    const { saveSiteConfig } = renderStep({ status: baseStatus({ host_proxy: masked }) });
    expect(screen.queryByRole("button", { name: /use detected proxy/i })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: /^use this$/i }));
    expect(screen.getByLabelText(/proxy url/i)).toHaveValue("http://alice@proxy.corp:8080");
    expect(screen.getByText(T.CRED_PASSWORD_MASKED)).toBeInTheDocument();
    expect(saveSiteConfig).not.toHaveBeenCalled();
  });

  it("an unparseable value withheld by the server has no Use button", () => {
    renderStep({
      status: baseStatus({
        host_proxy: {
          has_credentials: true,
          https_proxy: { value: "<redacted: unparseable proxy URL>", source: "env", has_credentials: true },
        },
      }),
    });
    expect(screen.queryByRole("button", { name: /^use this$/i })).not.toBeInTheDocument();
  });
});
