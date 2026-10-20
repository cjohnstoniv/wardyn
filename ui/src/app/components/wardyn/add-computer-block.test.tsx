/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Add this computer" (mock M-ATT-4, #3322). The strings are written out, not read from the copy module, so a
// drift in them fails this suite. Both /healthz forms (CLI sign-in on and off) are pinned: neither may show
// `serve`, `--proxy-image`, `wdn_` or `WARDYN_TOKEN=`.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { AddComputerBlock } from "./add-computer-block";

const ORIGIN = window.location.origin;
const SH = "curl -fsSL https://github.com/cjohnstoniv/wardyn/releases/download/v0.10.0/install-cli.sh | sh";
const PS = "irm https://github.com/cjohnstoniv/wardyn/releases/download/v0.10.0/install-cli.ps1 | iex";
const SIGN_IN_NOTE = "A browser tab opens. Approve the sign-in there once.";
const NO_CLI_SIGN_IN =
  "This server does not offer CLI sign-in yet. Set WARDYN_TOKEN to a personal API token first; a personal token works for setup. A Wardyn admin can invite you to create one, and you claim it under Your account.";
const WSL2 = "Runners run in WSL2. Open your WSL2 distribution and run all three commands there.";
const NO_INSTALL = "This deployment didn't report its version, so there is no install line here. See Other ways to install.";

function healthz(body: Record<string, unknown> | null) {
  const fetchMock = vi.fn(async () => (body === null ? new Response("no", { status: 503 }) : new Response(JSON.stringify({ status: "ok", ...body }), { status: 200 })));
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

/** Nothing the block draws may show a start command, a proxy image, a token value or a token assignment. */
function expectNoForbidden(root: HTMLElement) {
  const text = root.textContent ?? "";
  expect(text).not.toMatch(/\bserve\b/);
  expect(text).not.toContain("--proxy-image");
  expect(text).not.toContain("wdn_");
  expect(text).not.toContain("WARDYN_TOKEN=");
}

beforeEach(() => vi.unstubAllGlobals());
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe("Add this computer: the three steps", () => {
  it("with CLI sign-in on: title, lead, the three steps with their commands and notes, and nothing forbidden", async () => {
    healthz({ version: "0.10.0", cli_sign_in: true });
    const { container } = render(<AddComputerBlock />);
    expect(screen.getByRole("heading", { name: "Add this computer" })).toBeInTheDocument();
    expect(screen.getByText("Run these three commands in a terminal on the computer you are adding. It becomes a runner for your own runs.")).toBeInTheDocument();
    expect(await screen.findByText(SH)).toBeInTheDocument();
    expect(screen.getAllByRole("heading", { level: 3 }).map((h) => h.textContent)).toEqual(["Install the CLI", "Sign in", "Set up the runner"]);
    expect(screen.getByText("macOS, Linux and WSL2")).toBeInTheDocument();
    expect(screen.getByText(`wardyn login ${ORIGIN}`)).toBeInTheDocument();
    expect(screen.getByText(SIGN_IN_NOTE)).toBeInTheDocument();
    expect(screen.queryByText(NO_CLI_SIGN_IN)).not.toBeInTheDocument();
    expect(screen.getByText("wardyn runner setup")).toBeInTheDocument();
    expect(screen.getByText("It checks this computer, registers and claims the runner, starts it, and waits up to 60 seconds for it to connect. Run it again to upgrade.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Other ways to install" })).toHaveAttribute("href", expect.stringContaining("docs/runner-registration.md"));
    // No MDM section where a person adds only their own computer.
    expect(screen.queryByText("Managed install (MDM)")).not.toBeInTheDocument();
    expectNoForbidden(container);
  });

  it("the other operating system is folded under 'Another operating system'", async () => {
    healthz({ version: "0.10.0", cli_sign_in: true });
    const user = userEvent.setup();
    render(<AddComputerBlock />);
    await screen.findByText(SH);
    const fold = screen.getByText("Another operating system").closest("details") as HTMLDetailsElement;
    expect(fold.open).toBe(false);
    expect(within(fold).getByText("Windows (PowerShell)")).toBeInTheDocument();
    expect(within(fold).getByText(PS)).toBeInTheDocument();
    await user.click(screen.getByText("Another operating system"));
    expect(fold.open).toBe(true);
  });

  it("with CLI sign-in off, step 2 adds the WARDYN_TOKEN sentence in place of the browser note, and nothing forbidden", async () => {
    healthz({ version: "0.10.0", cli_sign_in: false });
    const { container } = render(<AddComputerBlock />);
    expect(await screen.findByText(NO_CLI_SIGN_IN)).toBeInTheDocument();
    expect(screen.queryByText(SIGN_IN_NOTE)).not.toBeInTheDocument();
    expect(screen.getByText(`wardyn login ${ORIGIN}`)).toBeInTheDocument();
    expectNoForbidden(container);
  });

  it("an older server that says nothing about CLI sign-in reads as off", async () => {
    healthz({ version: "0.10.0" });
    render(<AddComputerBlock />);
    expect(await screen.findByText(NO_CLI_SIGN_IN)).toBeInTheDocument();
  });

  it("an advertised mirror is used by both install lines", async () => {
    healthz({ version: "0.10.0", cli_sign_in: true, cli_artifact_base: "https://mirror.example.com/wardyn" });
    render(<AddComputerBlock />);
    expect(await screen.findByText("curl -fsSL https://mirror.example.com/wardyn/v0.10.0/install-cli.sh | WARDYN_ARTIFACT_BASE=https://mirror.example.com/wardyn sh")).toBeInTheDocument();
    expect(screen.getByText("$env:WARDYN_ARTIFACT_BASE='https://mirror.example.com/wardyn'; irm https://mirror.example.com/wardyn/v0.10.0/install-cli.ps1 | iex")).toBeInTheDocument();
  });
});

describe("Add this computer: heading levels and focus", () => {
  it("by default the title is an h2 and the steps h3; with level 4 they are h4 and h5", async () => {
    healthz({ version: "0.10.0", cli_sign_in: true });
    const { unmount } = render(<AddComputerBlock />);
    await screen.findByText(SH);
    expect(screen.getByRole("heading", { name: "Add this computer", level: 2 })).toBeInTheDocument();
    expect(screen.getAllByRole("heading", { level: 3 })).toHaveLength(3);
    unmount();
    healthz({ version: "0.10.0", cli_sign_in: true });
    render(<AddComputerBlock level={4} />);
    await screen.findByText(SH);
    expect(screen.getByRole("heading", { name: "Add this computer", level: 4 })).toBeInTheDocument();
    expect(screen.getAllByRole("heading", { level: 5 }).map((h) => h.textContent)).toEqual(["Install the CLI", "Sign in", "Set up the runner"]);
  });

  it("when Try again succeeds, the button goes and focus moves to the first step's title", async () => {
    healthz(null);
    const user = userEvent.setup();
    render(<AddComputerBlock />);
    const retry = await screen.findByRole("button", { name: "Try again" });
    // A retry that fails again leaves the button, and focus, where they are.
    await user.click(retry);
    expect(await screen.findByRole("button", { name: "Try again" })).toHaveFocus();
    healthz({ version: "0.10.0", cli_sign_in: true });
    await user.click(screen.getByRole("button", { name: "Try again" }));
    await screen.findByText(SH);
    expect(screen.queryByRole("button", { name: "Try again" })).not.toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Install the CLI" })).toHaveFocus();
  });
});

describe("Add this computer: where focus goes after Try again", () => {
  /** A /healthz whose answer the test releases: the retry is "in flight" until then. */
  function deferredHealthz() {
    let release: (body: Record<string, unknown> | null) => void = () => {};
    const answer = new Promise<Record<string, unknown> | null>((r) => (release = r));
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        const body = await answer;
        return body === null ? new Response("no", { status: 503 }) : new Response(JSON.stringify({ status: "ok", ...body }), { status: 200 });
      }),
    );
    return release;
  }

  it("success with focus still on Try again moves it to the first step's title", async () => {
    healthz(null);
    const user = userEvent.setup();
    render(<AddComputerBlock mdm={<input aria-label="elsewhere" />} />);
    await user.click(await screen.findByRole("button", { name: "Try again" }));
    await screen.findByRole("button", { name: "Try again" });
    const release = deferredHealthz();
    await user.click(screen.getByRole("button", { name: "Try again" }));
    expect(screen.getByRole("button", { name: "Try again" })).toHaveFocus();
    release({ version: "0.10.0", cli_sign_in: true });
    await screen.findByText(SH);
    expect(screen.getByRole("heading", { name: "Install the CLI" })).toHaveFocus();
  });

  it("success after focus moved elsewhere leaves it there", async () => {
    healthz(null);
    const user = userEvent.setup();
    render(<AddComputerBlock mdm={<input aria-label="elsewhere" />} />);
    await screen.findByRole("button", { name: "Try again" });
    const release = deferredHealthz();
    await user.click(screen.getByRole("button", { name: "Try again" }));
    const elsewhere = screen.getByLabelText("elsewhere");
    elsewhere.focus();
    release({ version: "0.10.0", cli_sign_in: true });
    await screen.findByText(SH);
    expect(elsewhere).toHaveFocus();
  });

  it("a repeated failure leaves focus on Try again", async () => {
    healthz(null);
    const user = userEvent.setup();
    render(<AddComputerBlock />);
    await screen.findByRole("button", { name: "Try again" });
    const release = deferredHealthz();
    await user.click(screen.getByRole("button", { name: "Try again" }));
    release(null);
    await waitFor(() => expect(screen.getByRole("button", { name: "Try again" })).toHaveFocus());
    expect(screen.getByRole("heading", { name: "Install the CLI" })).not.toHaveFocus();
  });

  it("unmounting mid-retry neither throws nor takes focus", async () => {
    healthz(null);
    const user = userEvent.setup();
    const { unmount } = render(<AddComputerBlock />);
    await screen.findByRole("button", { name: "Try again" });
    const release = deferredHealthz();
    await user.click(screen.getByRole("button", { name: "Try again" }));
    unmount();
    const outside = document.body.appendChild(document.createElement("input"));
    outside.focus();
    release({ version: "0.10.0", cli_sign_in: true });
    await new Promise((r) => setTimeout(r, 20));
    expect(outside).toHaveFocus();
    outside.remove();
  });
});

describe("Add this computer: no install line", () => {
  it("a /healthz that did not answer says so with Try again, which asks again", async () => {
    healthz(null);
    const user = userEvent.setup();
    render(<AddComputerBlock />);
    expect(await screen.findByText(NO_INSTALL)).toBeInTheDocument();
    // The commands that do not depend on the version are still there, and so is the way out.
    expect(screen.getByText("wardyn runner setup")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Other ways to install" })).toBeInTheDocument();
    // Nothing is claimed about CLI sign-in from a /healthz that said nothing.
    expect(screen.queryByText(NO_CLI_SIGN_IN)).not.toBeInTheDocument();
    // ...nor does it promise the browser sign-in.
    expect(screen.queryByText(SIGN_IN_NOTE)).not.toBeInTheDocument();
    healthz({ version: "0.10.0", cli_sign_in: true });
    await user.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByText(SH)).toBeInTheDocument();
  });

  it("a source build names no release: same sentence, no Try again", async () => {
    healthz({ version: "0.10.0+abc123", cli_sign_in: true });
    render(<AddComputerBlock />);
    expect(await screen.findByText(NO_INSTALL)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Try again" })).not.toBeInTheDocument();
  });
});

describe("Add this computer: viewing from Windows", () => {
  it("says to run the commands in WSL2, leads with the shell line and has no operating-system fold", async () => {
    vi.spyOn(window.navigator, "userAgent", "get").mockReturnValue("Mozilla/5.0 (Windows NT 10.0; Win64; x64)");
    healthz({ version: "0.10.0", cli_sign_in: true });
    render(<AddComputerBlock />);
    expect(await screen.findByText(SH)).toBeInTheDocument();
    expect(screen.getByText(WSL2)).toBeInTheDocument();
    expect(screen.queryByText("Another operating system")).not.toBeInTheDocument();
    expect(screen.queryByText(PS)).not.toBeInTheDocument();
  });

  it("elsewhere there is no WSL2 note", async () => {
    healthz({ version: "0.10.0", cli_sign_in: true });
    render(<AddComputerBlock />);
    await screen.findByText(SH);
    expect(screen.queryByText(WSL2)).not.toBeInTheDocument();
  });
});

describe("Add this computer: Managed install (MDM)", () => {
  it("folds what the page passes under the MDM title and lead, closed", async () => {
    healthz({ version: "0.10.0", cli_sign_in: true });
    render(<AddComputerBlock mdm={<p>token form</p>} />);
    await screen.findByText(SH);
    const mdm = screen.getByText("Managed install (MDM)").closest("details") as HTMLDetailsElement;
    expect(mdm.open).toBe(false);
    expect(within(mdm).getByText("For computers set up by device management: create a registration token for a person and deliver it to their computer.")).toBeInTheDocument();
    expect(within(mdm).getByText("token form")).toBeInTheDocument();
  });
});
