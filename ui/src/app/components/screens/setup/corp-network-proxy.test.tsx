/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// corp-network-proxy.test.tsx — the Host proxy sub-tab's own tests, split out
// of corp-network-step.test.tsx (which stays the source-of-truth test file for
// the orchestrator + Egress tab) alongside corp-network-proxy.tsx's own split
// from corp-network-step.tsx. These tests render the FULL CorpNetworkStep
// (the Host proxy tab has no standalone test harness of its own — same as
// before the split), so the render harness below is duplicated rather than
// shared, matching the existing corp-network-egress.tsx precedent of tests
// living beside the seam they cover.
import * as React from "react";
import type { ComponentProps } from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { HostProxyDetection } from "../../../lib/types";
import { T } from "../../../lib/integrations";
import { HttpError } from "../../../lib/api/core";
import { OperatorProvider } from "../../wardyn/operator-context";
import { CorpNetworkStep } from "./corp-network-step";
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

