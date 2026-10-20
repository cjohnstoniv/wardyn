/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// 0.9 Your account -> My runner and My pools (packet M-RN-2, mock M-POOLS v6). The strings are written out,
// not read from the copy modules, so a drift in them fails this suite.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { RemoteRunnerView, RunnerPool, RunnerPoolCatalogueEntry, RunnerPoolMemberView } from "../../../lib/types/runner-pools";
import type { RunnerRegistrationToken, RunnerView } from "../../../lib/types/runners";

const pools = {
  list: vi.fn(),
  listMine: vi.fn(),
  get: vi.fn(),
  members: vi.fn(),
  addMyRunner: vi.fn(),
  removeMyRunner: vi.fn(),
  addMyRemoteRunner: vi.fn(),
  removeMyRemoteRunner: vi.fn(),
  setMyDefaults: vi.fn(),
  clearMyDefaults: vi.fn(),
  removeMine: vi.fn(),
  usePolicy: vi.fn(),
  createMine: vi.fn(),
  myDefaults: vi.fn(),
};
const remoteMineMock = vi.fn();
vi.mock("../../../lib/api/runner-pools", () => ({
  runnerPools: new Proxy({}, { get: (_t, k: string) => (...a: unknown[]) => (pools as Record<string, (...x: unknown[]) => unknown>)[k](...a) }),
  remoteRunners: { listMine: (...a: unknown[]) => remoteMineMock(...a) },
}));
const navigateMock = vi.fn();
vi.mock("react-router-dom", async (orig) => ({ ...(await orig<typeof import("react-router-dom")>()), useNavigate: () => navigateMock }));

const listMineMock = vi.fn();
const updateMineMock = vi.fn();
const mintMineMock = vi.fn();
const claimMock = vi.fn();
const revokeMineMock = vi.fn();
vi.mock("../../../lib/api/runners", () => ({
  RUNNERS_DISABLED: "runners_disabled",
  runners: {
    listMine: (...a: unknown[]) => listMineMock(...a),
    mintMine: (...a: unknown[]) => mintMineMock(...a),
    claim: (...a: unknown[]) => claimMock(...a),
    revokeMine: (...a: unknown[]) => revokeMineMock(...a),
    updateMine: (...a: unknown[]) => updateMineMock(...a),
  },
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { toast } from "sonner";
import { HttpError } from "../../../lib/api/core";
import { aheadByHours } from "../../../lib/test-clock";
import { OperatorProvider } from "../../wardyn/operator-context";
import { MyRunnerCard } from "./my-runner-card";
import { MY_POOLS } from "../../wardyn/copy/runner-pools";
import { setField } from "../../../../test/set-field";

const FULL = "SHA256:3Jd1d8XqTHEWHOLEFINGERPRINTVALUEk2Q9";
const ABBREV = "SHA256:3Jd1d8…k2Q9";

function usageWith(runs: number): RunnerView["usage"] {
  const none = { runs: 0, cpu_millis: 0, memory_mib: 0, disk_mib: 0 };
  return { online: runs > 0, in_use: { ...none, runs }, available: none, disk_capped: true, reported: { cpu_millis: 8000, memory_mib: 16384, disk_mib: 512000 } };
}

function runner(over: Partial<RunnerView> = {}): RunnerView {
  return {
    id: "r-laptop",
    name: "laptop-7",
    state: "claimed",
    owner_kind: "person",
    trust_class: "personal",
    online: true,
    key_fingerprint: FULL,
    key_fingerprint_abbreviated: false,
    version: "0.9.0",
    created_at: aheadByHours(-72),
    claimed_at: aheadByHours(-71),
    last_seen_at: aheadByHours(0),
    posture: { mdm_managed: true, disk_encrypted: true, os: "darwin", os_version: "15.2", evidence: "mdm_file" },
    posture_reported_at: aheadByHours(-0.05),
    capacity: { cpu_millis_max: 4000, memory_mib_max: 8192 },
    classes: ["CC1"],
    allowed_roots: ["/Users/alice/src", "/Users/alice/scratch"],
    kind: "self_hosted",
    limits: { runs_at_once: 2, total: { cpu_millis: 4000, memory_mib: 8192, disk_mib: 102400 }, per_run: { cpu_millis: 2000, memory_mib: 4096, disk_mib: 51200 } },
    usage: usageWith(2),
    machine_name: "alice-mbp-16.corp.example.com",
    arch: "arm64",
    pools: [],
    ...over,
  };
}
const CLAIMED = runner();
const WAITING = runner({
  id: "r-mbp",
  name: "mbp-ben",
  state: "unclaimed",
  online: false,
  key_fingerprint: ABBREV,
  key_fingerprint_abbreviated: true,
  claimed_at: undefined,
  claim_expires_at: aheadByHours(23),
  created_at: aheadByHours(-0.03),
  minted_by: "alice",
  posture: undefined,
  capacity: undefined,
  classes: undefined,
  allowed_roots: undefined,
  usage: usageWith(0),
});
const TOKEN: RunnerRegistrationToken = {
  id: "t1",
  owner: "alice",
  owner_kind: "person",
  minted_by_kind: "human",
  break_glass: false,
  minted_by: "alice",
  token: "wdr_selfminted",
  created_at: aheadByHours(0),
  expires_at: aheadByHours(1),
  org_url: "https://org.example.com",
};

function renderCard(hash = "", admin = false) {
  return render(
    <MemoryRouter initialEntries={[`/account${hash}`]}>
      <OperatorProvider operator={admin} securityOperator={admin} operatorResolved principal="alice">
        <MyRunnerCard />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

const expand = async (user: ReturnType<typeof userEvent.setup>) => {
  await user.click(await screen.findByRole("button", { name: /^My runner/ }));
};

const G = 1024;
const POOL = (id: string, name: string, over: Partial<RunnerPool> = {}): RunnerPool => ({
  id, name, scope: "user", owner: "alice", state: "active", revision: 2, limits: { barriers: ["CC1", "CC2"], background: {}, interactive: {} }, created_at: aheadByHours(-90), updated_at: aheadByHours(-2), ...over,
});
const ENTRY = (p: RunnerPool): RunnerPoolCatalogueEntry => ({ id: p.id, name: p.name, scope: "user", owner: "alice", availability: "available", run_types: ["background", "interactive"], barrier_options: [] });
const SELF_MEMBER: RunnerPoolMemberView = { kind: "self_hosted", id: "r-laptop", name: "laptop-7", online: true, limits: CLAIMED.limits, usage: CLAIMED.usage };
const SHARED: RemoteRunnerView = {
  id: "shared-docker", kind: "remote_hosted", name: "shared-docker", people_pools: "everyone", revision: 3, created_at: aheadByHours(-200), updated_at: aheadByHours(-20), configured: true, online: true, pools: [],
  limits: { runs_at_once: 6, total: { cpu_millis: 24000, memory_mib: 96 * G, disk_mib: 1000 * G }, per_run: { cpu_millis: 4000, memory_mib: 16 * G, disk_mib: 100 * G } },
  usage: { online: true, in_use: { runs: 1, cpu_millis: 4000, memory_mib: 16 * G, disk_mib: 100 * G }, available: { runs: 5, cpu_millis: 20000, memory_mib: 80 * G, disk_mib: 900 * G }, disk_capped: true },
};
const REMOTE_MEMBER: RunnerPoolMemberView = { kind: "remote_hosted", id: "shared-docker", name: "shared-docker", online: true, configured: true, limits: SHARED.limits, usage: SHARED.usage };
const LAPTOP_POOL = POOL("p-laptop", "Laptop");
const HOME_POOL = POOL("p-home", "Home server", { limits: { barriers: ["CC1"], background: {}, max_concurrent_runs: 2 } });

/** The person's pools as the routes answer them: Laptop holds laptop-7 and shared-docker, Home server is empty. */
function withPools(defaultId?: string) {
  pools.list.mockResolvedValue({ pools: [ENTRY(LAPTOP_POOL), ENTRY(HOME_POOL)], ...(defaultId ? { default: { pool_id: defaultId, selection: "personal_default" } } : {}) });
  pools.get.mockImplementation(async (id: string) => (id === "p-laptop" ? LAPTOP_POOL : HOME_POOL));
  pools.members.mockImplementation(async (id: string) => (id === "p-laptop" ? [SELF_MEMBER, REMOTE_MEMBER] : []));
  remoteMineMock.mockResolvedValue([SHARED]);
}

beforeEach(() => {
  for (const m of [listMineMock, mintMineMock, claimMock, revokeMineMock, updateMineMock, remoteMineMock, navigateMock, ...Object.values(pools)]) m.mockReset();
  listMineMock.mockResolvedValue([CLAIMED]);
  pools.list.mockResolvedValue({ pools: [] });
  pools.listMine.mockResolvedValue({ pools: [] });
  remoteMineMock.mockResolvedValue([]);
});
afterEach(() => cleanup());

describe("My runner card", () => {
  // A5: the packet's rule is "no row, no card": with runners off the section is not a card with an empty state.
  it.each(["runners_disabled", "runner_session_required"])("renders nothing on a 403 %s", async (reason) => {
    listMineMock.mockRejectedValue(new HttpError(403, "runners are off", reason));
    const { container } = renderCard();
    await waitFor(() => expect(listMineMock).toHaveBeenCalled());
    await waitFor(() => expect(container).toBeEmptyDOMElement());
    expect(screen.queryByText("My runner")).toBeNull();
  });

  it("summarises: none, one, many, and a waiting runner wins", async () => {
    const summaries: [RunnerView[], string][] = [
      [[], "Not registered"],
      [[CLAIMED], "laptop-7 · Online"],
      [[CLAIMED, runner({ id: "r2", name: "desk-2", online: false })], "2 runners · 1 online"],
      [[CLAIMED, WAITING], "1 waiting for you to claim"],
    ];
    for (const [rows, text] of summaries) {
      listMineMock.mockResolvedValue(rows);
      const { unmount } = renderCard();
      expect(await screen.findByRole("button", { name: new RegExp(`^My runner\\s*${text}`) })).toBeInTheDocument();
      unmount();
    }
  });

  it("with nothing registered, says so and offers Register a runner", async () => {
    listMineMock.mockResolvedValue([]);
    const user = userEvent.setup();
    renderCard();
    await expand(user);
    expect(
      screen.getByText(
        "A computer you registered runs your own runs, several at once up to its Runs at once, under your organization's policy and audit. One runner per computer is enough. While it's offline it runs nothing new.",
      ),
    ).toBeInTheDocument();
    expect(screen.getByText("You haven't registered a runner.")).toBeInTheDocument();
    expect(screen.getByText("Register one, then put it in one of your pools. You can create a pool first.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Register a runner" })).toBeInTheDocument();
    // My pools is there before any runner is: a pool can be created first.
    expect(await screen.findByText("You haven't created a pool yet.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Create a pool" })).toBeInTheDocument();
  });

  it("a read failure says so with a Retry and no summary", async () => {
    listMineMock.mockRejectedValueOnce(new HttpError(500, "boom"));
    const user = userEvent.setup();
    renderCard();
    await expand(user);
    expect(await screen.findByText("Couldn't load your runners.")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("laptop-7")).toBeInTheDocument();
  });

  it("opens and focuses the card from the #my-runner deep link", async () => {
    renderCard("#my-runner");
    const section = (await screen.findByRole("button", { name: /^My runner/ })).closest("section") as HTMLElement;
    await waitFor(() => expect(section).toHaveFocus());
    expect(screen.getByText("laptop-7")).toBeInTheDocument();
  });
});

describe("My runner card: a claimed runner", () => {
  it("shows version, Machine, folders, posture and the whole fingerprint, each fact labelled as reported, with no copy control", async () => {
    const user = userEvent.setup();
    renderCard();
    await expand(user);
    expect(await screen.findByText("Version 0.9.0")).toBeInTheDocument();
    expect(screen.getByText("alice-mbp-16.corp.example.com")).toBeInTheDocument();
    expect(document.querySelector("[data-machine] [data-arch]")).toHaveTextContent("Architecture: arm64");
    expect(screen.getByText("Self-hosted")).toBeInTheDocument();
    expect(screen.getByText("/Users/alice/src")).toBeInTheDocument();
    expect(screen.getByText("Runs on it can use folders inside these only. They're set on the computer itself.")).toBeInTheDocument();
    expect(screen.getByText("macOS 15.2 · Managed by MDM · Disk encrypted")).toBeInTheDocument();
    expect(screen.getAllByText(/^Reported by this runner/).length).toBeGreaterThanOrEqual(3);
    expect(screen.getByText(FULL)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /copy/i })).toBeNull();
    expect(screen.queryByText("Use a folder from laptop-7")).toBeNull();
  });

  it("an offline runner says when it was last seen, or that it never connected; unreported facts are not guessed", async () => {
    listMineMock.mockResolvedValue([
      runner({ online: false, last_seen_at: aheadByHours(-3) }),
      runner({ id: "r2", name: "desk-2", online: false, last_seen_at: undefined, posture: undefined, capacity: undefined, classes: undefined, allowed_roots: undefined }),
    ]);
    const user = userEvent.setup();
    renderCard();
    await expand(user);
    expect(await screen.findByText("Last seen 3h ago")).toBeInTheDocument();
    expect(screen.getByText("Never connected")).toBeInTheDocument();
    expect(screen.getAllByText("Not reported yet").length).toBeGreaterThanOrEqual(2);
    expect(screen.queryByText("Disk not encrypted")).toBeNull();
  });

  // Board D2a: a runner that registered but never connected names the service and its log, whether or not
  // limits are set; one that dropped keeps today's notice.
  it("a runner that never connected names the service and its log, with or without limits", async () => {
    listMineMock.mockResolvedValue([
      runner({ id: "r-a", name: "mbp-dev", online: false, last_seen_at: undefined, limits: undefined, usage: usageWith(0) }),
      runner({ id: "r-b", name: "desk-9", online: false, last_seen_at: undefined, usage: usageWith(0) }),
      runner({ id: "r-c", name: "old-1", online: false, last_seen_at: aheadByHours(-3), usage: usageWith(0) }),
    ]);
    const user = userEvent.setup();
    renderCard();
    await expand(user);
    const rowOf = async (name: string) => (await screen.findByRole("heading", { name })).closest("li") as HTMLElement;
    for (const name of ["mbp-dev", "desk-9"]) {
      const notes = (await rowOf(name)).querySelectorAll("[data-offline-note]");
      expect(notes).toHaveLength(1);
      expect(notes[0].textContent).toBe("This runner registered but has never connected to this server. On that computer, check that wardyn-runnerd runs as a service (wardyn-runnerd setup installs one) and read its log: ~/Library/Logs/wardyn/runnerd.log on macOS, journalctl --user -u wardyn-runnerd on Linux. Behind a proxy, the service needs its own HTTPS_PROXY.");
      expect([...notes[0].querySelectorAll("code")].map((c) => c.textContent)).toEqual(["wardyn-runnerd setup", "~/Library/Logs/wardyn/runnerd.log", "journalctl --user -u wardyn-runnerd", "HTTPS_PROXY"]);
    }
    const old = await rowOf("old-1");
    expect(old.querySelectorAll("[data-offline-note]")).toHaveLength(1);
    expect(within(old).getByText("This runner is offline. Its capacity isn't counted as available until it reconnects.")).toBeInTheDocument();
    expect(within(old).queryByText(/never connected to this server/)).toBeNull();
  });

  it("revoking your own runner confirms with the run count and sends no not-mine marker", async () => {
    revokeMineMock.mockResolvedValue({ id: "r-laptop", state: "revoked", runs_killed: 2 });
    const user = userEvent.setup();
    renderCard();
    await expand(user);
    await user.click(await screen.findByRole("button", { name: "Revoke" }));
    const confirm = await screen.findByRole("alertdialog");
    expect(within(confirm).getByText("Revoke laptop-7?")).toBeInTheDocument();
    expect(within(confirm).getByText("2 runs are on it now.")).toBeInTheDocument();
    expect(within(confirm).getByRole("button", { name: "Cancel" })).toHaveFocus();
    await user.click(within(confirm).getByRole("button", { name: "Revoke runner" }));
    await waitFor(() => expect(revokeMineMock).toHaveBeenCalledWith("r-laptop", false));
  });

  // MR7r: an administrator's revoke is shown for the owner to see; the row has no action.
  it("a runner someone else revoked shows who and when, with no action", async () => {
    listMineMock.mockResolvedValue([runner({ state: "revoked", online: false, revoked_at: aheadByHours(-24), revoked_by: "sub-sec", revoked_by_email: "security@example.com" })]);
    const user = userEvent.setup();
    renderCard();
    await expand(user);
    expect(await screen.findByText(/laptop-7 was revoked by security@example.com .* Register it again to use it\./)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Revoke" })).toBeNull();
    expect(screen.getByRole("button", { name: /^My runner\s*Not registered/ })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Register a runner" })).toBeInTheDocument();
  });
});

describe("My runner card: a waiting runner and the claim", () => {
  beforeEach(() => listMineMock.mockResolvedValue([WAITING]));

  it("shows only the abbreviated fingerprint the server sent, with no copy control, and says whose token it came from", async () => {
    const user = userEvent.setup();
    renderCard();
    await expand(user);
    expect(await screen.findByText(ABBREV)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /copy/i })).toBeNull();
    expect(screen.getByText("From a token you created.")).toBeInTheDocument();
    expect(screen.getByText(/Registered .* · claim it within 23 hours/)).toBeInTheDocument();
    cleanup();
    listMineMock.mockResolvedValue([{ ...WAITING, minted_by: "sub-admin", minted_by_email: "admin@example.com" }]);
    renderCard();
    await expand(user);
    expect(await screen.findByText("From a token admin@example.com created for you.")).toBeInTheDocument();
  });

  it("the claim field starts empty (never filled from the row), is trimmed, and a mismatch marks it", async () => {
    claimMock.mockRejectedValueOnce(new HttpError(403, "fingerprint mismatch", "runner_claim_mismatch"));
    const user = userEvent.setup();
    renderCard();
    await expand(user);
    await user.click(await screen.findByRole("button", { name: "Claim" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Claim mbp-ben")).toBeInTheDocument();
    expect(within(dialog).getByText("wardyn runner claim", { selector: "code" })).toBeInTheDocument();
    const field = within(dialog).getByRole("textbox", { name: /Fingerprint from your computer/ });
    expect(field).toHaveValue("");
    expect(field).toHaveFocus();
    const submit = within(dialog).getByRole("button", { name: "Claim runner" });
    expect(submit).toBeDisabled();
    setField(field, `  ${FULL}  `);
    await user.click(submit);
    await waitFor(() => expect(claimMock).toHaveBeenCalledWith("r-mbp", FULL));
    expect(field).toHaveAttribute("aria-invalid", "true");
    expect(within(dialog).getByText(/That fingerprint doesn't match mbp-ben's key\. Check you're claiming the computer you registered\. If you didn't register mbp-ben, revoke it\./)).toBeInTheDocument();
    expect(field.getAttribute("aria-describedby")).toContain("claim-runner-mismatch");
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  it("a claim that matches closes the dialog and re-reads", async () => {
    claimMock.mockResolvedValue({ runner_id: "r-mbp", fingerprint: FULL, state: "claimed", org_url_sha256: "" });
    const user = userEvent.setup();
    renderCard();
    await expand(user);
    await user.click(await screen.findByRole("button", { name: "Claim" }));
    const dialog = await screen.findByRole("dialog");
    setField(within(dialog).getByRole("textbox", { name: /Fingerprint from your computer/ }), FULL);
    listMineMock.mockResolvedValue([{ ...CLAIMED, id: "r-mbp", name: "mbp-ben" }]);
    await user.click(within(dialog).getByRole("button", { name: "Claim runner" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() => expect(screen.getByRole("button", { name: /^My runner\s*mbp-ben · Online/ })).toBeInTheDocument());
  });

  // A7 at the card: the not-mine door marks the revoke.
  it("This isn't mine revokes with the not-mine marker, after a confirm that opens on Cancel", async () => {
    revokeMineMock.mockResolvedValue({ id: "r-mbp", state: "revoked", runs_killed: 0 });
    const user = userEvent.setup();
    renderCard();
    await expand(user);
    await user.click(await screen.findByRole("button", { name: "This isn't mine" }));
    const confirm = await screen.findByRole("alertdialog");
    expect(within(confirm).getByText("Revoke mbp-ben?")).toBeInTheDocument();
    expect(within(confirm).getByText(/It has never run anything as you, and once revoked it never can\./)).toBeInTheDocument();
    expect(within(confirm).getByRole("button", { name: "Cancel" })).toHaveFocus();
    await user.click(within(confirm).getByRole("button", { name: "Revoke runner" }));
    await waitFor(() => expect(revokeMineMock).toHaveBeenCalledWith("r-mbp", true));
  });
});

describe("My runner card: registering and the 5 second poll", () => {
  const register = async (user: ReturnType<typeof userEvent.setup>) => {
    await expand(user);
    await user.click(await screen.findByRole("button", { name: "Register a runner" }));
    return screen.findByText("wdr_selfminted");
  };

  it("shows the token once with the command from the server's org_url; Done hides it for good", async () => {
    listMineMock.mockResolvedValue([]);
    mintMineMock.mockResolvedValue(TOKEN);
    const user = userEvent.setup();
    renderCard();
    await register(user);
    expect(screen.getByText("Shown once. It works once, for 1 hour.")).toBeInTheDocument();
    expect(screen.getByText("wardyn-runnerd register --org https://org.example.com --token-file ~/wardyn-runner-token")).toBeInTheDocument();
    expect(screen.getByText("Registration token")).toHaveFocus();
    // With nothing registered yet the two sentences stay, above the token.
    const follows = (a: HTMLElement, b: HTMLElement) => !!(a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING);
    expect(follows(screen.getByText("You haven't registered a runner."), screen.getByText("Registration token"))).toBe(true);
    expect(follows(screen.getByText("Register one, then put it in one of your pools. You can create a pool first."), screen.getByText("Registration token"))).toBe(true);
    expect(screen.getByRole("button", { name: "Copy token" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Register a runner" })).toBeNull();
    await user.click(screen.getByRole("button", { name: "Done" }));
    expect(screen.queryByText("wdr_selfminted")).toBeNull();
    expect(screen.getByRole("button", { name: "Register a runner" })).toBeInTheDocument();
  });

  it("a refused register shows the server's sentence under the button", async () => {
    listMineMock.mockResolvedValue([]);
    mintMineMock.mockRejectedValue(new HttpError(429, "too many tokens"));
    const user = userEvent.setup();
    renderCard();
    await expand(user);
    await user.click(await screen.findByRole("button", { name: "Register a runner" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("too many tokens");
  });

  it("polls every 5 s while a token shows, announces the runner that registered once, and hides the spent token", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    try {
      listMineMock.mockResolvedValue([]);
      mintMineMock.mockResolvedValue(TOKEN);
      const user = userEvent.setup({ advanceTimers: (ms) => vi.advanceTimersByTime(ms) });
      renderCard();
      await register(user);
      const reads = listMineMock.mock.calls.length;
      listMineMock.mockResolvedValue([WAITING]);
      await act(async () => {
        vi.advanceTimersByTime(5000);
      });
      await waitFor(() => expect(listMineMock.mock.calls.length).toBeGreaterThan(reads));
      expect(await screen.findByText("mbp-ben registered. Claim it below.")).toBeInTheDocument();
      expect(screen.queryByText("wdr_selfminted")).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  it("does not poll with nothing waiting and no token", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    try {
      renderCard();
      await screen.findByRole("button", { name: /^My runner/ });
      const reads = listMineMock.mock.calls.length;
      await act(async () => {
        vi.advanceTimersByTime(20_000);
      });
      expect(listMineMock.mock.calls.length).toBe(reads);
    } finally {
      vi.useRealTimers();
    }
  });

  it("a token past its hour is replaced by the expiry sentence and Register returns", async () => {
    listMineMock.mockResolvedValue([]);
    mintMineMock.mockResolvedValue({ ...TOKEN, expires_at: aheadByHours(-0.01) });
    const user = userEvent.setup();
    renderCard();
    await expand(user);
    await user.click(await screen.findByRole("button", { name: "Register a runner" }));
    expect(await screen.findByText("This token has expired. Register again for a new one.")).toBeInTheDocument();
    expect(screen.queryByText("wdr_selfminted")).toBeNull();
    expect(screen.getByRole("button", { name: "Register a runner" })).toBeInTheDocument();
  });
});

describe("My runner card: resources and limits", () => {
  it("shows the limits the owner set, what is in use, and what the computer reports", async () => {
    const user = userEvent.setup();
    renderCard();
    await expand(user);
    expect(await screen.findByText("Limits you set")).toBeInTheDocument();
    expect(screen.getByText("Runs at once: 2")).toBeInTheDocument();
    expect(screen.getByText(/Running now: 2 of 2/)).toBeInTheDocument();
    expect(screen.getByRole("row", { name: /^CPU/ })).toHaveTextContent("4 CPUs");
    expect(screen.getByText(/This computer reports 8 CPUs · 16 GB memory · 500 GB disk\. Your limits can't be higher\./)).toBeInTheDocument();
  });

  it("Edit limits saves the whole set, and refuses a total above what the computer reports before asking the server", async () => {
    updateMineMock.mockResolvedValue(CLAIMED);
    const user = userEvent.setup();
    renderCard();
    await expand(user);
    await user.click(await screen.findByRole("button", { name: "Edit limits for laptop-7" }));
    const dialog = await screen.findByRole("dialog", { name: "Limits for laptop-7" });
    expect(within(dialog).getByText("They are prefilled from what this computer reports, and you can lower them. Every run on it stays within them.")).toBeInTheDocument();
    expect(within(dialog).getByText("Reported: 16")).toBeInTheDocument();
    const memory = within(dialog).getAllByRole("spinbutton", { name: "Memory (GB)" });
    setField(memory[0], "64");
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    expect(within(dialog).getAllByText("Not allowed: this computer reports 16 GB memory.").length).toBeGreaterThan(0);
    expect(updateMineMock).not.toHaveBeenCalled();
    setField(memory[0], "12");
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() =>
      expect(updateMineMock).toHaveBeenCalledWith("r-laptop", {
        runs_at_once: 2,
        total: { cpu_millis: 4000, memory_mib: 12288, disk_mib: 102400 },
        per_run: { cpu_millis: 2000, memory_mib: 4096, disk_mib: 51200 },
      }),
    );
  });

  it("a runner with no limits says they are required, suggests them from what it reports, and sets them up", async () => {
    listMineMock.mockResolvedValue([runner({ limits: null, usage: { ...usageWith(0), reported: { cpu_millis: 8000, memory_mib: 32768, disk_mib: 512000 } } })]);
    const user = userEvent.setup();
    renderCard();
    await expand(user);
    expect(await screen.findByText("Limits to set up")).toBeInTheDocument();
    expect(screen.getByText("Required")).toBeInTheDocument();
    expect(screen.getByText("Every runner needs limits before it can run anything: in total, per run, and how many runs at once.")).toBeInTheDocument();
    expect(
      screen.getByText(
        "Suggested: half of what it reports as the total (4 CPUs · 16 GB memory · 250 GB disk), 4 runs at once, and each run gets the total ÷ Runs at once (1 CPU · 4 GB memory · 62 GB disk).",
      ),
    ).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Set limits for laptop-7" }));
    const dialog = await screen.findByRole("dialog", { name: "Set up limits for laptop-7" });
    expect(within(dialog).getByRole("button", { name: "Save limits" })).toBeInTheDocument();
    // Per run follows the total until it is typed over.
    setField(within(dialog).getAllByRole("spinbutton", { name: "CPU (CPUs)" })[0], "8");
    expect(within(dialog).getAllByRole("spinbutton", { name: "CPU (CPUs)" })[1]).toHaveValue(2);
  });
});

describe("My pools", () => {
  it("lists the person's pools with their runners of both kinds, and says which pools each runner is in", async () => {
    withPools("p-laptop");
    const user = userEvent.setup();
    renderCard();
    await expand(user);
    const mine = (await screen.findByText("My pools")).closest("section") as HTMLElement;
    expect(within(mine).getByText(MY_POOLS.ONLY_YOU)).toBeInTheDocument();
    expect(within(mine).getByText(MY_POOLS.LEDE)).toBeInTheDocument();
    // #2856: the lede is body-size, not the 16px page default above the card's own title.
    expect(within(mine).getByText(MY_POOLS.LEDE).className).toContain("text-body");
    expect(MY_POOLS.ONLY_YOU).toBe("Only you can change these. Administrators can see them.");
    expect(MY_POOLS.LEDE).toContain("You can have up to 10 pools, for example Laptop and Home server.");
    const laptop = within(mine).getByRole("button", { name: "Laptop" }).closest("li") as HTMLElement;
    expect(within(laptop).getByText("✓ Default")).toBeInTheDocument();
    expect(within(laptop).queryByRole("button", { name: "Make Laptop your default" })).toBeNull();
    expect(laptop).toHaveTextContent("Interactive and background · Fence, Wall");
    expect(within(laptop).getByText("Remote-hosted")).toBeInTheDocument();
    const home = within(mine).getByRole("button", { name: "Home server" }).closest("li") as HTMLElement;
    expect(home).toHaveTextContent("Background only · Fence · Pool limits: 2 runs maximum");
    expect(home).toHaveTextContent("Runners in it: None yet");
    expect(within(home).getByRole("button", { name: "Make Home server your default" })).toBeInTheDocument();
    expect(within(mine).getByRole("button", { name: "Clear default" })).toBeInTheDocument();
    // The runner's own section.
    expect(screen.getByText("In 1 of 2")).toBeInTheDocument();
    expect(screen.getByText("Put laptop-7 in one of your pools to run your own runs on it.")).toBeInTheDocument();
    // Once on the runner, once on the pool.
    expect(screen.getAllByRole("button", { name: "Remove laptop-7 from Laptop" })).toHaveLength(2);
    expect(screen.getByRole("button", { name: "Add laptop-7 to Home server" })).toBeInTheDocument();
  });

  it("adds a runner to a pool, shows a refusal on that row, and removes one after a confirmation", async () => {
    withPools();
    pools.addMyRunner.mockRejectedValueOnce(new HttpError(422, "laptop-7 is not one of your runners in Home server.")).mockResolvedValueOnce({});
    pools.removeMyRunner.mockResolvedValue(undefined);
    const user = userEvent.setup();
    renderCard();
    await expand(user);
    const add = await screen.findByRole("button", { name: "Add laptop-7 to Home server" });
    await user.click(add);
    expect(await screen.findByText("laptop-7 is not one of your runners in Home server.")).toBeInTheDocument();
    await user.click(add);
    await waitFor(() => expect(pools.addMyRunner).toHaveBeenLastCalledWith("p-home", "r-laptop"));
    await user.click(screen.getAllByRole("button", { name: "Remove laptop-7 from Laptop" })[0]);
    const confirm = await screen.findByRole("alertdialog");
    expect(within(confirm).getByText("Remove laptop-7 from Laptop?")).toBeInTheDocument();
    expect(within(confirm).getByText("It won't take new runs from this pool. Runs already on it aren't affected. You can add it again at any time.")).toBeInTheDocument();
    await user.click(within(confirm).getByRole("button", { name: "Remove from pool" }));
    await waitFor(() => expect(pools.removeMyRunner).toHaveBeenCalledWith("p-laptop", "r-laptop"));
  });

  it("Add runner offers the person's own runners and the remote-hosted ones made available, minus those already in", async () => {
    withPools();
    pools.addMyRemoteRunner.mockResolvedValue({});
    const user = userEvent.setup();
    renderCard();
    await expand(user);
    await user.click(await screen.findByRole("button", { name: "Add a runner to Home server" }));
    const dialog = await screen.findByRole("dialog", { name: "Add a runner to Home server" });
    expect(within(dialog).getByText(/Another person's self-hosted runner never appears here\./)).toBeInTheDocument();
    const options = within(dialog).getAllByRole("radio");
    expect(options).toHaveLength(2);
    expect(dialog).toHaveTextContent("Machine: alice-mbp-16.corp.example.com · Runs at once: 2");
    await user.click(options[1]);
    await user.click(within(dialog).getByRole("button", { name: "Add runner" }));
    await waitFor(() => expect(pools.addMyRemoteRunner).toHaveBeenCalledWith("p-home", "shared-docker"));
    // Every runner the person can use is already in Laptop.
    await user.click(screen.getByRole("button", { name: "Add a runner to Laptop" }));
    expect(await within(await screen.findByRole("dialog", { name: "Add a runner to Laptop" })).findByText("Nothing to add. Every runner you can use is already in this pool.")).toBeInTheDocument();
  });

  it("makes a pool the default and clears it; Create and Edit go to the pool form", async () => {
    withPools("p-laptop");
    pools.setMyDefaults.mockResolvedValue({ pool_id: "p-home" });
    pools.clearMyDefaults.mockResolvedValue(undefined);
    const user = userEvent.setup();
    renderCard();
    await expand(user);
    await user.click(await screen.findByRole("button", { name: "Make Home server your default" }));
    await waitFor(() => expect(pools.setMyDefaults).toHaveBeenCalledWith({ pool_id: "p-home" }));
    await user.click(screen.getByRole("button", { name: "Clear default" }));
    await waitFor(() => expect(pools.clearMyDefaults).toHaveBeenCalled());
    await user.click(screen.getByRole("button", { name: "Create a pool" }));
    expect(navigateMock).toHaveBeenLastCalledWith("/account/pools/new");
    await user.click(screen.getByRole("button", { name: "Edit Laptop" }));
    expect(navigateMock).toHaveBeenLastCalledWith("/account/pools/p-laptop/edit");
  });

  it("an administrator's own account lists only their own pools, though their pool list carries everyone's", async () => {
    withPools();
    const bobs = { ...ENTRY(POOL("p-bob", "Build box")), owner: "bob" };
    pools.list.mockResolvedValue({ pools: [ENTRY(LAPTOP_POOL), bobs] });
    const user = userEvent.setup();
    renderCard("", true);
    await expand(user);
    const mine = (await screen.findByText("My pools")).closest("section") as HTMLElement;
    expect(await within(mine).findByRole("button", { name: "Laptop" })).toBeInTheDocument();
    expect(within(mine).queryByRole("button", { name: "Build box" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Edit Build box" })).toBeNull();
    // Nothing of another person's pool is read for this card.
    expect(pools.get).not.toHaveBeenCalledWith("p-bob");
    expect(pools.members).not.toHaveBeenCalledWith("p-bob");
    expect(screen.getByText("In 1 of 1")).toBeInTheDocument();
  });

  it("says a default that no longer applies is gone, that a runner in no pool runs nothing, and that an unclaimed one is claimed first", async () => {
    withPools();
    pools.list.mockResolvedValue({ pools: [ENTRY(HOME_POOL)], default: { selection: "personal_default", reason: "runner_pool_default_unavailable" } });
    listMineMock.mockResolvedValue([CLAIMED, WAITING]);
    const user = userEvent.setup();
    renderCard();
    await expand(user);
    expect(await screen.findByText("Your default pool is no longer available. Choose a pool to continue.")).toBeInTheDocument();
    expect(screen.getByText("It's in no pool, so it runs nothing.")).toBeInTheDocument();
    expect(screen.getByText("In 0 of 1")).toBeInTheDocument();
    expect(screen.getByText("Claim it before you add it to a pool.")).toBeInTheDocument();
  });

  it("says so when the server manages no pools, and offers Retry when the pools can't be read", async () => {
    pools.list.mockRejectedValue(new HttpError(501, "This server does not manage runner pools yet.", "runner_pools_unavailable"));
    const user = userEvent.setup();
    const { unmount } = renderCard();
    await expand(user);
    expect(await screen.findByText("This server does not manage runner pools yet.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Create a pool" })).toBeNull();
    unmount();
    pools.list.mockRejectedValueOnce(new HttpError(500, "boom")).mockResolvedValue({ pools: [] });
    renderCard();
    await expand(user);
    expect(await screen.findByText("Couldn't load pools")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("You haven't created a pool yet.")).toBeInTheDocument();
  });
});

describe("My runner card: the start command and the claim into a pool", () => {
  const registerShown = async (user: ReturnType<typeof userEvent.setup>) => {
    await expand(user);
    await user.click(await screen.findByRole("button", { name: "Register a runner" }));
    await screen.findByText("wdr_selfminted");
  };
  const VM_HINT = /^On Docker Desktop, .* --total-disk <GB> to register, since the VM hides the disk\.$/;

  it("names the proxy image the server answered with in the serve command, and always says what VM-backed Docker needs", async () => {
    listMineMock.mockResolvedValue([]);
    mintMineMock.mockResolvedValue({ ...TOKEN, proxy_image: "ghcr.io/example/wardyn-proxy:0.9.1" });
    const user = userEvent.setup();
    renderCard();
    await registerShown(user);
    expect(screen.getByText("wardyn-runnerd serve --org https://org.example.com --proxy-image ghcr.io/example/wardyn-proxy:0.9.1")).toBeInTheDocument();
    expect(screen.getByText(VM_HINT)).toBeInTheDocument();
    cleanup();
    mintMineMock.mockResolvedValue(TOKEN);
    renderCard();
    await registerShown(user);
    expect(screen.getByText("wardyn-runnerd serve --org https://org.example.com")).toBeInTheDocument();
    expect(screen.getByText(VM_HINT)).toBeInTheDocument();
  });

  const claimWithPool = async (user: ReturnType<typeof userEvent.setup>, pool: string, makeDefault: boolean, beforeClaim?: () => void) => {
    listMineMock.mockResolvedValue([WAITING]);
    claimMock.mockResolvedValue({ runner_id: "r-mbp", fingerprint: FULL, state: "claimed", org_url_sha256: "" });
    pools.addMyRunner.mockResolvedValue({});
    pools.myDefaults.mockResolvedValue({});
    pools.setMyDefaults.mockResolvedValue({});
    renderCard();
    await expand(user);
    await user.click(await screen.findByRole("button", { name: "Claim" }));
    const dialog = await screen.findByRole("dialog");
    const box = within(dialog).getByRole("checkbox", { name: "Make it my default pool" });
    expect(box).toBeDisabled();
    setField(within(dialog).getByRole("textbox", { name: /Fingerprint from your computer/ }), FULL);
    setField(within(dialog).getByRole("combobox", { name: /Put it in a pool/ }), pool);
    if (makeDefault) await user.click(box);
    beforeClaim?.();
    await user.click(within(dialog).getByRole("button", { name: "Claim runner" }));
  };

  it("a new pool name makes the pool, puts the claimed runner in it and makes it the default", async () => {
    pools.createMine.mockResolvedValue(POOL("p-new", "Desk"));
    const user = userEvent.setup();
    await claimWithPool(user, "Desk", true);
    await waitFor(() => expect(pools.setMyDefaults).toHaveBeenCalledWith({ pool_id: "p-new" }));
    expect(pools.createMine).toHaveBeenCalledWith({ name: "Desk" });
    expect(pools.addMyRunner).toHaveBeenCalledWith("p-new", "r-mbp");
    expect(claimMock).toHaveBeenCalledWith("r-mbp", FULL);
  });

  it("one of the person's own pools is used as it is, and the default stays alone unless asked", async () => {
    pools.listMine.mockResolvedValue({ pools: [ENTRY(LAPTOP_POOL), { ...ENTRY(POOL("o1", "Desk", { scope: "organization" })), scope: "organization" }] });
    pools.get.mockResolvedValue(LAPTOP_POOL);
    pools.members.mockResolvedValue([]);
    const user = userEvent.setup();
    await claimWithPool(user, "Laptop", false);
    await waitFor(() => expect(pools.addMyRunner).toHaveBeenCalledWith("p-laptop", "r-mbp"));
    expect(pools.createMine).not.toHaveBeenCalled();
    expect(pools.setMyDefaults).not.toHaveBeenCalled();
  });

  it("passes over another person's same-named pool (an administrator's oversight row, only in the shared list) and makes the person's own", async () => {
    const theirs: RunnerPoolCatalogueEntry = { ...ENTRY(POOL("p-bob", "Desk", { owner: "bob" })), owner: "bob", availability: "unavailable", reason: "runner_pool_not_found" };
    pools.list.mockResolvedValue({ pools: [theirs] });
    pools.listMine.mockResolvedValue({ pools: [] });
    pools.get.mockResolvedValue(POOL("p-bob", "Desk", { owner: "bob" }));
    pools.members.mockResolvedValue([]);
    pools.createMine.mockResolvedValue(POOL("p-new", "Desk"));
    const user = userEvent.setup();
    await claimWithPool(user, "Desk", false);
    await waitFor(() => expect(pools.addMyRunner).toHaveBeenCalledWith("p-new", "r-mbp"));
    expect(pools.createMine).toHaveBeenCalledWith({ name: "Desk" });
    expect(pools.addMyRunner).not.toHaveBeenCalledWith("p-bob", expect.anything());
  });

  it("never makes a pool when it cannot read the person's pools", async () => {
    pools.listMine.mockRejectedValue(new HttpError(500, "boom", "internal"));
    const user = userEvent.setup();
    await claimWithPool(user, "Desk", false);
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith(expect.stringContaining("mbp-ben is claimed but isn't in Desk.")));
    expect(pools.createMine).not.toHaveBeenCalled();
  });

  it("a pool step that fails after the claim says the runner is claimed and where to finish", async () => {
    pools.createMine.mockRejectedValue(new HttpError(422, "That name is taken.", "runner_pool_name_taken"));
    const user = userEvent.setup();
    await claimWithPool(user, "Desk", false);
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith(expect.stringContaining("mbp-ben is claimed but isn't in Desk. Add it from My pools.")));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("a default that fails after the runner joined the pool does not say the runner is not in the pool (DR-0210)", async () => {
    vi.mocked(toast.error).mockClear();
    pools.createMine.mockResolvedValue(POOL("p-new", "Desk"));
    const user = userEvent.setup();
    await claimWithPool(user, "Desk", true, () => pools.setMyDefaults.mockRejectedValueOnce(new HttpError(500, "Defaults could not be saved.", "internal")));
    await waitFor(() => expect(pools.addMyRunner).toHaveBeenCalledWith("p-new", "r-mbp"));
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("mbp-ben is in Desk but isn't your default pool yet. Set it from My pools. Defaults could not be saved."));
    expect(toast.error).not.toHaveBeenCalledWith(expect.stringContaining("isn't in Desk"));
  });

  it("a defaults read that fails after the runner joined the pool says the same as a failed write", async () => {
    vi.mocked(toast.error).mockClear();
    pools.createMine.mockResolvedValue(POOL("p-new", "Desk"));
    const user = userEvent.setup();
    await claimWithPool(user, "Desk", true, () => pools.myDefaults.mockRejectedValueOnce(new HttpError(500, "Defaults could not be read.", "internal")));
    await waitFor(() => expect(pools.addMyRunner).toHaveBeenCalledWith("p-new", "r-mbp"));
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("mbp-ben is in Desk but isn't your default pool yet. Set it from My pools. Defaults could not be read."));
    expect(pools.setMyDefaults).not.toHaveBeenCalled();
    expect(toast.error).not.toHaveBeenCalledWith(expect.stringContaining("isn't in Desk"));
  });
});
