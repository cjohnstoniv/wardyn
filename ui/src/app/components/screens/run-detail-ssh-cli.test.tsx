/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// "Get the CLI" and the folded SSH lane of the attach card (M-ATT-1; ATT1-Card, ATT1-States,
// ATT1-Phone). The attach command and the first-time line are pinned in run-detail-ssh.test.tsx.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, within, fireEvent } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { RunDetail } from "../../lib/types";

const healthMock = vi.fn();
vi.mock("../../lib/api/health", () => ({
  health: { health: (...a: unknown[]) => healthMock(...a) },
}));
vi.mock("../../lib/api/ssh-keys", () => ({
  sshKeys: { listKeys: () => Promise.resolve([{ fingerprint: "SHA256:x", principal: "alice@example.com", name: "k", public_key: "", created_at: "" }]) },
}));
vi.mock("../../lib/api/runs", () => ({
  runs: { attachTicket: vi.fn() },
}));

import { ConnectSSHCard } from "./run-detail-ssh";
import { INSTALL_DOCS_URL } from "./run-detail/get-the-cli";
import { OperatorProvider } from "../wardyn/operator-context";
import { RUN_CLI } from "../wardyn/copy";
import { aheadByHours } from "../../lib/test-clock";

const OWNER = "alice@example.com";
const run: RunDetail = {
  id: "11111111-1111-1111-1111-111111111111",
  created_at: aheadByHours(-1),
  updated_at: aheadByHours(-1),
  created_by: OWNER,
  agent: "claude-code",
  repo: "acme/widgets",
  task: "do the thing",
  confinement_class: "CC2",
  state: "RUNNING",
  spiffe_id: "spiffe://wardyn.local/agent-run/11111111-1111-1111-1111-111111111111",
  runner_target: "docker",
  placement: "",
  interactive: false,
};
const REL = "https://github.com/cjohnstoniv/wardyn/releases/download/v0.10.0";
const MIRROR = "https://artifacts.example.com/wardyn";

function renderCard() {
  return render(
    <MemoryRouter>
      <OperatorProvider operator principal={OWNER}>
        <ConnectSSHCard run={run} />
      </OperatorProvider>
    </MemoryRouter>,
  );
}
const codeLines = () => Array.from(document.querySelectorAll("code")).map((c) => c.textContent);
const setUA = (ua: string) => Object.defineProperty(navigator, "userAgent", { value: ua, configurable: true });
const realUA = navigator.userAgent;

beforeEach(() => healthMock.mockReset());
afterEach(() => setUA(realUA));

describe("Get the CLI — the install line", () => {
  it("shows the release's macOS/Linux line first, with the version sub-line, and folds Windows", async () => {
    healthMock.mockResolvedValue({ status: "ok", version: "0.10.0" });
    renderCard();
    await screen.findByText(RUN_CLI.VERSION("0.10.0"));
    expect(screen.getByText(RUN_CLI.UNIX)).toBeInTheDocument();
    const lines = codeLines();
    expect(lines).toContain(`curl -fsSL ${REL}/install-cli.sh | sh`);
    expect(lines).toContain(`irm ${REL}/install-cli.ps1 | iex`);
    // Windows sits inside the folded "Another operating system" disclosure, which is closed.
    const folded = screen.getByText(RUN_CLI.OTHER_OS).closest("details")!;
    expect(folded).not.toHaveAttribute("open");
    expect(within(folded).getByText(`irm ${REL}/install-cli.ps1 | iex`)).toBeInTheDocument();
    expect(within(folded).queryByText(/curl -fsSL/)).toBeNull();
    expect(screen.queryByText(RUN_CLI.MIRROR_NOTE)).toBeNull();
  });

  it("leads with the PowerShell line for a Windows viewer and folds the other", async () => {
    setUA("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/130 Safari/537.36");
    healthMock.mockResolvedValue({ status: "ok", version: "0.10.0" });
    renderCard();
    await screen.findByText(RUN_CLI.VERSION("0.10.0"));
    const folded = screen.getByText(RUN_CLI.OTHER_OS).closest("details")!;
    expect(within(folded).getByText(`curl -fsSL ${REL}/install-cli.sh | sh`)).toBeInTheDocument();
    expect(within(folded).queryByText(/irm /)).toBeNull();
    expect(screen.getByText(`irm ${REL}/install-cli.ps1 | iex`).closest("details")).toBeNull();
  });

  // Both forms are pinned: the mirror is the script's own URL AND WARDYN_ARTIFACT_BASE, on both lines.
  it("uses the advertised mirror twice per line and says so", async () => {
    healthMock.mockResolvedValue({ status: "ok", version: "0.10.0", cli_artifact_base: MIRROR });
    renderCard();
    await screen.findByText(RUN_CLI.MIRROR_NOTE);
    const lines = codeLines();
    expect(lines).toContain(`curl -fsSL ${MIRROR}/v0.10.0/install-cli.sh | WARDYN_ARTIFACT_BASE=${MIRROR} sh`);
    expect(lines).toContain(`$env:WARDYN_ARTIFACT_BASE='${MIRROR}'; irm ${MIRROR}/v0.10.0/install-cli.ps1 | iex`);
    expect(lines.some((l) => l?.includes("github.com/cjohnstoniv/wardyn/releases"))).toBe(false);
  });

  it("copies the line it shows", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    healthMock.mockResolvedValue({ status: "ok", version: "0.10.0" });
    renderCard();
    await screen.findByText(RUN_CLI.VERSION("0.10.0"));
    fireEvent.click(screen.getByRole("button", { name: RUN_CLI.COPY_INSTALL_UNIX }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(`curl -fsSL ${REL}/install-cli.sh | sh`));
  });

  it("links Other ways to install to the ranked docs", async () => {
    healthMock.mockResolvedValue({ status: "ok", version: "0.10.0" });
    renderCard();
    const link = await screen.findByRole("link", { name: RUN_CLI.OTHER_WAYS });
    expect(link).toHaveAttribute("href", INSTALL_DOCS_URL);
    expect(screen.getByText(RUN_CLI.OTHER_WAYS_BODY)).toBeInTheDocument();
  });
});

describe("Get the CLI — no install line", () => {
  // Revert proof: drop the `loaded` gate in GetTheCli and the first assertion goes red.
  it("draws nothing in Get the CLI until the first /healthz result arrives", async () => {
    let resolve!: (v: unknown) => void;
    healthMock.mockReturnValue(new Promise((r) => (resolve = r)));
    renderCard();
    await waitFor(() => expect(healthMock).toHaveBeenCalled());
    expect(screen.queryByText(RUN_CLI.GET_TITLE)).toBeNull();
    expect(screen.queryByRole("button", { name: RUN_CLI.TRY_AGAIN })).toBeNull();
    expect(screen.queryByText(RUN_CLI.NO_INSTALL_LINE)).toBeNull();
    resolve({});
    await screen.findByRole("button", { name: RUN_CLI.TRY_AGAIN });
  });

  it("a mirror the card rejects claims neither a version nor a missing one", async () => {
    healthMock.mockResolvedValue({ status: "ok", version: "0.10.0", cli_artifact_base: "https://a.example/~x" });
    renderCard();
    await screen.findByText(RUN_CLI.GET_TITLE);
    expect(screen.queryByText(RUN_CLI.NO_INSTALL_LINE)).toBeNull();
    expect(screen.queryByText(RUN_CLI.VERSION("0.10.0"))).toBeNull();
    expect(screen.queryByRole("button", { name: RUN_CLI.TRY_AGAIN })).toBeNull();
    expect(codeLines().some((l) => l?.includes("install-cli"))).toBe(false);
    expect(screen.getByRole("link", { name: RUN_CLI.OTHER_WAYS })).toBeInTheDocument();
  });

  it("answered without a version: says so, no Try again, command and first-time line stay", async () => {
    healthMock.mockResolvedValue({ status: "ok" });
    renderCard();
    await screen.findByText(RUN_CLI.NO_INSTALL_LINE);
    expect(screen.queryByRole("button", { name: RUN_CLI.TRY_AGAIN })).toBeNull();
    expect(screen.getByText(`wardyn attach ${run.id}`)).toBeInTheDocument();
    expect(codeLines().some((l) => l?.includes("install-cli"))).toBe(false);
    expect(screen.getByRole("link", { name: RUN_CLI.OTHER_WAYS })).toBeInTheDocument();
  });

  it("a source build's version names no release either", async () => {
    healthMock.mockResolvedValue({ status: "ok", version: "0.10.0+9f3c1ab2d-dirty" });
    renderCard();
    await screen.findByText(RUN_CLI.NO_INSTALL_LINE);
  });

  it("/healthz did not answer: same panel plus Try again, which asks again and then draws the line", async () => {
    healthMock.mockResolvedValueOnce({}).mockResolvedValue({ status: "ok", version: "0.10.0" });
    renderCard();
    fireEvent.click(await screen.findByRole("button", { name: RUN_CLI.TRY_AGAIN }));
    await screen.findByText(RUN_CLI.VERSION("0.10.0"));
    expect(healthMock).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole("button", { name: RUN_CLI.TRY_AGAIN })).toBeNull();
  });
});

describe("Get the CLI — the org CA fingerprints", () => {
  it("shows nothing when /healthz publishes none", async () => {
    healthMock.mockResolvedValue({ status: "ok", version: "0.10.0" });
    renderCard();
    await screen.findByText(RUN_CLI.VERSION("0.10.0"));
    expect(screen.queryByText(RUN_CLI.CA_TITLE)).toBeNull();
  });

  it("shows one line per fingerprint in /healthz's order, labelled with the subject CN", async () => {
    healthMock.mockResolvedValue({
      status: "ok",
      version: "0.10.0",
      org_ca_fingerprints: [
        { subject: "Corp Root CA", sha256: "sha256:9d2f6c1a" },
        { subject: "Corp Issuing CA 2", sha256: "sha256:77aa01bc" },
      ],
    });
    renderCard();
    await screen.findByText(RUN_CLI.CA_TITLE);
    expect(screen.getByText((_, el) => el?.tagName === "P" && el.textContent === RUN_CLI.CA_BODY)).toBeInTheDocument();
    const rows = screen.getAllByRole("button", { name: RUN_CLI.COPY_FINGERPRINT }).map((b) => b.parentElement!.parentElement!.textContent);
    expect(rows).toEqual(["Corp Root CAsha256:9d2f6c1aCopy", "Corp Issuing CA 2sha256:77aa01bcCopy"]);
  });
});

describe("The SSH lane is demoted", () => {
  it("is a folded disclosure after Get the CLI and before UI apps", async () => {
    healthMock.mockResolvedValue({ status: "ok", version: "0.10.0", ssh: { enabled: true, advertise_addr: "wardyn.corp.example:2222" } });
    renderCard();
    const sshCmd = await screen.findByText(`ssh ${run.id}@wardyn.corp.example -p 2222`);
    const lane = sshCmd.closest("details")!;
    expect(lane).not.toHaveAttribute("open");
    expect(lane.querySelector("summary")!.textContent).toContain(RUN_CLI.SSH_TITLE);
    const after = (a: Node, b: Node) => !!(a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING);
    expect(after(screen.getByText(RUN_CLI.GET_TITLE), lane)).toBe(true);
    expect(after(lane, screen.getByText("UI apps"))).toBe(true);
  });

  it("keeps the OFF sentence inside the fold", async () => {
    healthMock.mockResolvedValue({ status: "ok", version: "0.10.0" });
    renderCard();
    const off = await screen.findByText(/Off on this deployment\. It gives you/);
    expect(off.closest("details")!.querySelector("summary")!.textContent).toContain(RUN_CLI.SSH_TITLE);
  });
});
