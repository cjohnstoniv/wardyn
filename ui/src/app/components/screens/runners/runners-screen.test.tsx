/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// 0.9 Admin -> Runners (packet M-RN-1, mock M-POOLS v6). The strings are written out, not read from the copy
// modules, so a drift in them fails this suite. The Pools tab has its own suite; here it is a stand-in.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { RemoteRunnerView } from "../../../lib/types/runner-pools";
import type { RunnerRegistrationToken, RunnerView } from "../../../lib/types/runners";

const listMock = vi.fn();
const getMock = vi.fn();
const settingsMock = vi.fn();
const setEnabledMock = vi.fn();
const tokensMock = vi.fn();
const mintMock = vi.fn();
const revokeMock = vi.fn();
const revokeTokenMock = vi.fn();
vi.mock("../../../lib/api/runners", () => ({
  RUNNERS_DISABLED: "runners_disabled",
  runners: {
    list: (...a: unknown[]) => listMock(...a),
    get: (...a: unknown[]) => getMock(...a),
    settings: (...a: unknown[]) => settingsMock(...a),
    setEnabled: (...a: unknown[]) => setEnabledMock(...a),
    tokens: (...a: unknown[]) => tokensMock(...a),
    mint: (...a: unknown[]) => mintMock(...a),
    revoke: (...a: unknown[]) => revokeMock(...a),
    revokeToken: (...a: unknown[]) => revokeTokenMock(...a),
  },
}));
const remoteListMock = vi.fn();
const remoteUpdateMock = vi.fn();
const setPeopleMock = vi.fn();
const clearPeopleMock = vi.fn();
vi.mock("../../../lib/api/runner-pools", () => ({
  remoteRunners: {
    list: (...a: unknown[]) => remoteListMock(...a),
    update: (...a: unknown[]) => remoteUpdateMock(...a),
    setPeoplePolicy: (...a: unknown[]) => setPeopleMock(...a),
    clearPeoplePolicy: (...a: unknown[]) => clearPeopleMock(...a),
  },
}));
vi.mock("./pools/pools-tab", () => ({
  PoolsTab: ({ openPoolId, onOpenRunner }: { openPoolId: string | null; onOpenRunner: (kind: string, id: string) => void }) => (
    <div data-testid="pools-tab">
      open:{openPoolId ?? "none"}
      <button onClick={() => onOpenRunner("remote_hosted", "shared-docker")}>member shared-docker</button>
    </div>
  ),
}));
// No directory configured: the person field is a plain text box.
vi.mock("../../../lib/api/directory", () => ({ MIN_QUERY_LEN: 2, directory: { search: () => Promise.resolve(null) } }));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { HttpError, PendingChangeError } from "../../../lib/api/core";
import { aheadByHours } from "../../../lib/test-clock";
import { OperatorProvider } from "../../wardyn/operator-context";
import { RunnersScreen } from "./runners-screen";
import { setField } from "../../../../test/set-field";
import { holdExitAnimation } from "../../../../test/exit-animation";

const ORG = "https://org.example.com";
const NOW_ISO = () => aheadByHours(0);

function usageWith(runs: number): RunnerView["usage"] {
  const none = { runs: 0, cpu_millis: 0, memory_mib: 0, disk_mib: 0 };
  return {
    online: runs > 0,
    in_use: { runs, cpu_millis: runs * 2000, memory_mib: runs * 4096, disk_mib: runs * 51200 },
    available: none,
    disk_capped: true,
    reported: { cpu_millis: 8000, memory_mib: 16384, disk_mib: 512000 },
  };
}

const SHARED: RemoteRunnerView = {
  id: "shared-docker",
  kind: "remote_hosted",
  name: "shared-docker",
  people_pools: "off",
  limits: { runs_at_once: 6, total: { cpu_millis: 24000, memory_mib: 98304, disk_mib: 1024000 }, per_run: { cpu_millis: 4000, memory_mib: 16384, disk_mib: 102400 } },
  revision: 3,
  created_at: aheadByHours(-240),
  updated_at: aheadByHours(-24),
  configured: true,
  online: true,
  pools: [{ id: "p-general", name: "General", scope: "organization" }, { id: "p-lab", name: "Lab VM", scope: "user", owner: "cara@example.com" }],
  usage: {
    online: true,
    in_use: { runs: 1, cpu_millis: 4000, memory_mib: 16384, disk_mib: 102400 },
    available: { runs: 5, cpu_millis: 20000, memory_mib: 81920, disk_mib: 921600 },
    disk_capped: true,
  },
};

function runner(over: Partial<RunnerView> = {}): RunnerView {
  return {
    id: "r-laptop",
    name: "laptop-7",
    state: "claimed",
    owner: "alice@example.com",
    online: true,
    key_fingerprint: "SHA256:3Jd1d8XqFULLFINGERPRINTVALUEk2Q9",
    key_fingerprint_abbreviated: false,
    version: "0.9.0",
    created_at: aheadByHours(-72),
    claimed_at: aheadByHours(-71),
    last_seen_at: NOW_ISO(),
    posture: { mdm_managed: true, disk_encrypted: true, os: "darwin", os_version: "15.2", evidence: "mdm_file" },
    posture_reported_at: aheadByHours(-0.05),
    posture_source: "runner_asserted",
    capacity: { cpu_millis_max: 4000, memory_mib_max: 8192 },
    classes: ["CC1", "CC2"],
    allowed_roots: ["/Users/alice/src", "/Users/alice/scratch"],
    kind: "self_hosted",
    limits: { runs_at_once: 2, total: { cpu_millis: 4000, memory_mib: 8192, disk_mib: 102400 }, per_run: { cpu_millis: 2000, memory_mib: 4096, disk_mib: 51200 } },
    usage: usageWith(2),
    machine_name: "alice-mbp-16.corp.example.com",
    pools: [{ id: "p-laptop", name: "Laptop" }],
    ...over,
  };
}

const LAPTOP = runner();
const DESK = runner({ id: "r-desk", name: "desk-2", online: false, last_seen_at: aheadByHours(-3), posture: undefined, capacity: undefined, classes: undefined, allowed_roots: undefined, usage: usageWith(0), machine_name: undefined, pools: [] });
const WAITING = runner({
  id: "r-mbp",
  name: "mbp-ben",
  state: "unclaimed",
  owner: "ben@example.com",
  online: false,
  claimed_at: undefined,
  claim_expires_at: aheadByHours(23),
  posture: undefined,
  capacity: undefined,
  classes: undefined,
  allowed_roots: undefined,
  usage: usageWith(0),
});
const OLD = runner({ id: "r-old", name: "old-box", state: "revoked", online: false, revoked_at: aheadByHours(-48), revoked_by: "carol@example.com", usage: usageWith(0) });

const TOKEN: RunnerRegistrationToken = { id: "t1", owner: "dan@example.com", minted_by: "admin@corp.example", created_at: aheadByHours(-1), expires_at: aheadByHours(48) };

function renderScreen(opts: { operator?: boolean; at?: string } = {}) {
  const { operator = true, at = "/admin/runners" } = opts;
  return render(
    <MemoryRouter initialEntries={[at]}>
      <OperatorProvider operator={operator} securityOperator operatorResolved principal="admin@corp.example">
        <RunnersScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  for (const m of [listMock, getMock, settingsMock, setEnabledMock, tokensMock, mintMock, revokeMock, revokeTokenMock, remoteListMock, remoteUpdateMock, setPeopleMock, clearPeopleMock]) m.mockReset();
  remoteListMock.mockResolvedValue([]);
  settingsMock.mockResolvedValue({ enabled: true });
  listMock.mockResolvedValue([DESK, OLD, LAPTOP, WAITING]);
  tokensMock.mockResolvedValue([TOKEN]);
  getMock.mockResolvedValue(LAPTOP);
});
afterEach(() => cleanup());

const cards = () => within(screen.getByRole("list", { name: "Runners" })).getAllByRole("listitem");
const rowNames = () => cards().map((li) => li.getAttribute("aria-label"));
const cardOf = (name: string) => screen.getByRole("button", { name }).closest("li") as HTMLElement;

describe("Admin -> Runners list", () => {
  it("reads every runner once (revoked included), and sorts waiting first, then online, then offline", async () => {
    renderScreen();
    await screen.findByRole("button", { name: "laptop-7" });
    expect(listMock).toHaveBeenCalledWith("all");
    expect(rowNames()).toEqual(["mbp-ben", "laptop-7", "desk-2"]);
    expect(screen.getByText("Runners", { selector: "h1" })).toBeInTheDocument();
  });

  // RN1-D2: Revoked outranks Not claimed yet, which outranks Online/Offline. The fixture is a revoked runner a stale
  // read still marks online; the chip must say Revoked.
  it("one chip per runner: Revoked beats Online, Not claimed yet beats Online", async () => {
    listMock.mockResolvedValue([runner({ id: "r-x", name: "ghost-1", state: "revoked", online: true }), runner({ id: "r-y", name: "wait-1", state: "unclaimed", online: true }), LAPTOP]);
    const user = userEvent.setup();
    renderScreen();
    await screen.findByRole("button", { name: "laptop-7" });
    const chipOf = (name: string) => within(cardOf(name)).getByText(/^(Online|Offline|Revoked|Not claimed yet)$/);
    expect(chipOf("laptop-7")).toHaveTextContent("Online");
    expect(chipOf("wait-1")).toHaveTextContent("Not claimed yet");
    await user.click(screen.getByRole("radio", { name: "All" }));
    expect(chipOf("ghost-1")).toHaveTextContent("Revoked");
  });

  it("an unclaimed card says who must claim it and when it expires; Last seen is real and Online reads now", async () => {
    renderScreen();
    const row = (await screen.findByRole("button", { name: "mbp-ben" })).closest("li") as HTMLElement;
    expect(within(row).getByText(/Waiting for ben@example.com to claim it\. Only they can\. · expires in 23 hours/)).toBeInTheDocument();
    // It takes no run until it is claimed, so it shows no resources.
    expect(within(row).queryByRole("table")).toBeNull();
    expect(within(cardOf("laptop-7")).getByText("Last seen now")).toBeInTheDocument();
    expect(within(cardOf("desk-2")).getByText("Last seen 3h ago")).toBeInTheDocument();
  });

  it("a card names the kind, Machine, owner and pools, and shows Total, Per run, In use and Available", async () => {
    remoteListMock.mockResolvedValue([SHARED]);
    renderScreen();
    const laptop = (await screen.findByRole("button", { name: "laptop-7" })).closest("li") as HTMLElement;
    expect(within(laptop).getByText("Self-hosted")).toBeInTheDocument();
    expect(within(laptop).getByText("alice-mbp-16.corp.example.com")).toBeInTheDocument();
    expect(within(laptop).getByText("alice@example.com")).toBeInTheDocument();
    expect(within(laptop).getByRole("button", { name: "Laptop" })).toBeInTheDocument();
    const cpu = within(laptop).getByRole("row", { name: /^CPU/ });
    expect(cpu).toHaveTextContent("4 CPUs");
    expect(cpu).toHaveTextContent("2 CPUs");
    expect(cpu).toHaveTextContent("4 of 4 CPUs");
    expect(within(laptop).getByRole("row", { name: /^Runs/ })).toHaveTextContent("2 of 2");
    expect(within(laptop).getAllByText("Full").length).toBeGreaterThan(0);
    // An offline runner's capacity is never offered, and a runner with no Machine or pool says so.
    const desk = cardOf("desk-2");
    expect(within(desk).getAllByText("Not counted")).toHaveLength(4);
    expect(within(desk).getByText("This runner is offline. Its capacity isn't counted as available until it reconnects.")).toBeInTheDocument();
    expect(within(desk).getByText("Not reported")).toBeInTheDocument();
    expect(within(desk).getByText("None — runs nothing")).toBeInTheDocument();
    // A remote-hosted runner is the organization's; its user pools name their owner.
    const shared = cardOf("shared-docker");
    expect(within(shared).getByText("Remote-hosted")).toBeInTheDocument();
    expect(within(shared).getByText("Not applicable")).toBeInTheDocument();
    expect(within(shared).getByText("Organization")).toBeInTheDocument();
    expect(shared).toHaveTextContent("General, Lab VM · cara");
    expect(within(shared).getByRole("row", { name: /^Memory/ })).toHaveTextContent("16 GB of 96 GB");
    expect(rowNames()).toEqual(["mbp-ben", "laptop-7", "desk-2", "shared-docker"]);
  });

  it("filters Active (default) / Revoked / All with arrow keys, and searches by name or owner", async () => {
    const user = userEvent.setup();
    renderScreen();
    await screen.findByRole("button", { name: "laptop-7" });
    expect(screen.getByRole("radio", { name: "Active" })).toBeChecked();
    expect(screen.queryByRole("button", { name: "old-box" })).toBeNull();
    await user.click(screen.getByRole("radio", { name: "Active" }));
    await user.keyboard("{ArrowRight}");
    expect(screen.getByRole("radio", { name: "Revoked" })).toBeChecked();
    expect(rowNames()).toEqual(["old-box"]);
    await user.keyboard("{ArrowRight}");
    expect(rowNames()).toHaveLength(4);
    setField(screen.getByRole("searchbox", { name: "Search runners" }), "ben@");
    await waitFor(() => expect(rowNames()).toEqual(["mbp-ben"]));
    setField(screen.getByRole("searchbox", { name: "Search runners" }), "alice-mbp");
    await waitFor(() => expect(rowNames()).toEqual(["mbp-ben", "laptop-7", "old-box"]));
    setField(screen.getByRole("searchbox", { name: "Search runners" }), "zzz");
    expect(await screen.findByText('No runner matches "zzz".')).toBeInTheDocument();
  });

  it("says so when a filter is empty, and when no runner exists yet", async () => {
    listMock.mockResolvedValue([LAPTOP]);
    const user = userEvent.setup();
    const { unmount } = renderScreen();
    await screen.findByRole("button", { name: "laptop-7" });
    await user.click(screen.getByRole("radio", { name: "Revoked" }));
    expect(screen.getByText("No revoked runners.")).toBeInTheDocument();
    unmount();
    listMock.mockResolvedValue([]);
    renderScreen();
    expect(await screen.findByText("No runners yet")).toBeInTheDocument();
    expect(screen.getByText(/People register a computer from Your account → My runner\./)).toBeInTheDocument();
  });

  it("a failed read shows its own title and a Retry that reads again", async () => {
    listMock.mockRejectedValueOnce(new HttpError(500, "boom"));
    const user = userEvent.setup();
    renderScreen();
    expect(await screen.findByText("Couldn't load runners")).toBeInTheDocument();
    expect(screen.getByText(/Runners already claimed keep running their owners' runs under your policy/)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("button", { name: "laptop-7" })).toBeInTheDocument();
  });

  it("re-reads silently every 30 seconds while the tab is visible", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    try {
      renderScreen();
      await screen.findByRole("button", { name: "laptop-7" });
      expect(listMock).toHaveBeenCalledTimes(1);
      await act(async () => {
        vi.advanceTimersByTime(30_000);
      });
      await waitFor(() => expect(listMock).toHaveBeenCalledTimes(2));
      expect(screen.queryByText("Couldn't load runners")).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("Admin -> Runners tiers", () => {
  it("a super admin sees Create a registration token (the page's one default) and Turn off runners (outline)", async () => {
    renderScreen();
    const create = await screen.findByRole("button", { name: "Create a registration token" });
    expect(create.className).toMatch(/bg-primary/);
    expect(screen.getByRole("button", { name: "Turn off runners" }).className).not.toMatch(/bg-primary|bg-destructive/);
  });

  it("a security admin has no Create, Turn on or Turn off, but can revoke a runner and an unused token", async () => {
    const user = userEvent.setup();
    revokeMock.mockResolvedValue({ id: "r-laptop", state: "revoked", runs_killed: 2 });
    revokeTokenMock.mockResolvedValue(undefined);
    renderScreen({ operator: false });
    await screen.findByRole("button", { name: "laptop-7" });
    expect(screen.queryByRole("button", { name: /Create a registration token|Turn on runners|Turn off runners/ })).toBeNull();

    await user.click(screen.getByRole("button", { name: "Revoke" }));
    const confirmToken = await screen.findByRole("alertdialog");
    expect(within(confirmToken).getByText("Revoke the unused token for dan@example.com?")).toBeInTheDocument();
    await user.click(within(confirmToken).getByRole("button", { name: "Revoke" }));
    await waitFor(() => expect(revokeTokenMock).toHaveBeenCalledWith("t1"));

    await user.click(screen.getByRole("button", { name: "laptop-7" }));
    await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Revoke" }));
    await user.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Revoke runner" }));
    await waitFor(() => expect(revokeMock).toHaveBeenCalledWith("r-laptop"));
  });

  it("with runners off a super admin can turn them on; a security admin is told who can", async () => {
    settingsMock.mockResolvedValue({ enabled: false });
    listMock.mockResolvedValue([]);
    const { unmount } = renderScreen();
    expect(await screen.findByText("Runners are off")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Turn on runners" })).toBeInTheDocument();
    expect(screen.queryByText("A super admin can turn runners on here.", { exact: false })).toBeNull();
    unmount();
    renderScreen({ operator: false });
    expect(await screen.findByText(/A super admin can turn runners on here\./)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Turn on runners" })).toBeNull();
  });

  it("turning on is confirmed, and a refusal shows the server's sentence inside the confirm", async () => {
    settingsMock.mockResolvedValue({ enabled: false });
    listMock.mockResolvedValue([]);
    setEnabledMock.mockRejectedValueOnce(new HttpError(409, "WARDYN_CONTROL_PLANE_URL must be https", "runners_org_url_invalid"));
    const user = userEvent.setup();
    renderScreen();
    await user.click(await screen.findByRole("button", { name: "Turn on runners" }));
    const confirm = await screen.findByRole("alertdialog");
    expect(within(confirm).getByText("Turn on runners?")).toBeInTheDocument();
    await user.click(within(confirm).getByRole("button", { name: "Turn on runners" }));
    const alert = await within(confirm).findByRole("alert");
    expect(alert).toHaveTextContent("Runners can't be turned on yet");
    expect(alert).toHaveTextContent("WARDYN_CONTROL_PLANE_URL must be https");
    expect(setEnabledMock).toHaveBeenCalledWith(true);
    settingsMock.mockResolvedValue({ enabled: true });
    setEnabledMock.mockResolvedValueOnce({ enabled: true });
    listMock.mockResolvedValue([LAPTOP]);
    await user.click(within(confirm).getByRole("button", { name: "Turn on runners" }));
    expect(await screen.findByRole("button", { name: "laptop-7" })).toBeInTheDocument();
  });

  it("turning off is a destructive confirm", async () => {
    setEnabledMock.mockResolvedValue({ enabled: false });
    const user = userEvent.setup();
    renderScreen();
    await user.click(await screen.findByRole("button", { name: "Turn off runners" }));
    const confirm = await screen.findByRole("alertdialog");
    expect(within(confirm).getByText("Turn off runners?")).toBeInTheDocument();
    expect(within(confirm).getByRole("button", { name: "Turn off runners" }).className).toMatch(/bg-destructive/);
    await user.click(within(confirm).getByRole("button", { name: "Turn off runners" }));
    await waitFor(() => expect(setEnabledMock).toHaveBeenCalledWith(false));
  });
});

describe("Admin -> Runners drawer and revoke", () => {
  it("shows the reported facts under their label, the whole fingerprint without a copy control, and Revoke", async () => {
    const user = userEvent.setup();
    renderScreen();
    await user.click(await screen.findByRole("button", { name: "laptop-7" }));
    const drawer = await screen.findByRole("dialog");
    expect(within(drawer).getAllByText(/^Reported by this runner/).length).toBeGreaterThanOrEqual(4);
    expect(within(drawer).getByText("Managed by MDM")).toBeInTheDocument();
    expect(within(drawer).getByText("Fence · Wall")).toBeInTheDocument();
    expect(within(drawer).getByText("/Users/alice/src")).toBeInTheDocument();
    // Machine, its resources (its owner's to set) and its pools.
    expect(within(drawer).getByText("The hostname the runner reports at register and on each connect. It is not proof of identity.")).toBeInTheDocument();
    expect(within(drawer).getByText("Limits set by its owner")).toBeInTheDocument();
    expect(within(drawer).getByText("Runs at once: 2")).toBeInTheDocument();
    expect(within(drawer).getByText(/Running now: 2 of 2/)).toBeInTheDocument();
    expect(within(drawer).getByText("At its limit")).toBeInTheDocument();
    expect(within(drawer).getByText("Only its owner changes these, in Your account → My runner or with wardyn runner set. You can read them here.")).toBeInTheDocument();
    expect(within(drawer).getByText(/This computer reports 8 CPUs · 16 GB memory · 500 GB disk\. Its limits can't be higher\./)).toBeInTheDocument();
    expect(within(drawer).getByRole("button", { name: "Laptop" })).toBeInTheDocument();
    expect(within(drawer).queryByRole("button", { name: "Edit limits" })).toBeNull();
    expect(within(drawer).getByText("SHA256:3Jd1d8XqFULLFINGERPRINTVALUEk2Q9")).toBeInTheDocument();
    expect(within(drawer).queryByRole("button", { name: /copy/i })).toBeNull();
    expect(within(drawer).getByRole("button", { name: "Revoke" })).toBeInTheDocument();
    expect(within(drawer).getByText("laptop-7")).toHaveFocus();
  });

  // The confirm closes after a 200 ms exit animation. The drawer puts focus on its title when Revoke is gone;
  // the confirm's own focus restore must not pull it back to the Revoke button that the re-read then removes.
  it("after a revoke the drawer's title keeps focus while the confirm leaves and the re-read lands late", async () => {
    const hold = holdExitAnimation();
    try {
      const user = userEvent.setup();
      revokeMock.mockResolvedValue({ id: "r-laptop", state: "revoked", runs_killed: 2 });
      renderScreen();
      await user.click(await screen.findByRole("button", { name: "laptop-7" }));
      const drawer = await screen.findByRole("dialog");
      await user.click(within(drawer).getByRole("button", { name: "Revoke" }));
      let landRead!: (rows: RunnerView[]) => void;
      listMock.mockImplementation(() => new Promise<RunnerView[]>((r) => { landRead = r; }));
      await user.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Revoke runner" }));
      await waitFor(() => expect(revokeMock).toHaveBeenCalledWith("r-laptop"));
      await act(async () => { await new Promise((r) => requestAnimationFrame(() => r(undefined))); });
      expect(document.activeElement).toHaveTextContent("laptop-7");
      await hold.finish();
      await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
      expect(document.activeElement).toHaveTextContent("laptop-7");
      await act(async () => landRead([DESK, OLD, { ...LAPTOP, state: "revoked" }, WAITING]));
      expect(document.activeElement).toHaveTextContent("laptop-7");
    } finally {
      hold.release();
    }
  });

  it("a claimed runner that has not reported shows Not reported yet, never a guess", async () => {
    const user = userEvent.setup();
    renderScreen();
    await user.click(await screen.findByRole("button", { name: "desk-2" }));
    const drawer = await screen.findByRole("dialog");
    expect(within(drawer).getAllByText("Not reported yet")).toHaveLength(3);
    expect(within(drawer).getByText("Not reported")).toBeInTheDocument();
    expect(within(drawer).getByText("None — runs nothing")).toBeInTheDocument();
    expect(within(drawer).queryByText("Disk not encrypted")).toBeNull();
  });

  it("an unclaimed runner's drawer has the wait, the fingerprint and Revoke, and nothing reported", async () => {
    const user = userEvent.setup();
    renderScreen();
    await user.click(await screen.findByRole("button", { name: "mbp-ben" }));
    const drawer = await screen.findByRole("dialog");
    expect(within(drawer).getByText("Waiting for ben@example.com to claim it. Only they can.")).toBeInTheDocument();
    expect(within(drawer).queryByText("Posture")).toBeNull();
    expect(within(drawer).getByRole("button", { name: "Revoke" })).toBeInTheDocument();
    // A4: the admin never claims.
    expect(screen.queryAllByRole("button", { name: /claim/i })).toHaveLength(0);
  });

  it("a revoked runner's drawer says who and has no Revoke", async () => {
    const user = userEvent.setup();
    renderScreen();
    await user.click(await screen.findByRole("radio", { name: "Revoked" }));
    await user.click(screen.getByRole("button", { name: "old-box" }));
    const drawer = await screen.findByRole("dialog");
    expect(within(drawer).getByText(/Revoked by carol@example.com/)).toBeInTheDocument();
    expect(within(drawer).queryByRole("button", { name: "Revoke" })).toBeNull();
  });

  it("the revoke confirm opens on Cancel, names the runs on it, and Cancel acts on nothing", async () => {
    const user = userEvent.setup();
    renderScreen();
    await user.click(await screen.findByRole("button", { name: "laptop-7" }));
    await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Revoke" }));
    const confirm = await screen.findByRole("alertdialog");
    expect(within(confirm).getByText("Revoke laptop-7?")).toBeInTheDocument();
    expect(within(confirm).getByText("2 runs are on it now.")).toBeInTheDocument();
    expect(within(confirm).getByRole("button", { name: "Cancel" })).toHaveFocus();
    await user.click(within(confirm).getByRole("button", { name: "Cancel" }));
    expect(revokeMock).not.toHaveBeenCalled();
  });

  it("revoking turns the row Revoked in place and sends the id", async () => {
    revokeMock.mockResolvedValue({ id: "r-laptop", state: "revoked", runs_killed: 2 });
    const user = userEvent.setup();
    renderScreen();
    await user.click(await screen.findByRole("button", { name: "laptop-7" }));
    const drawer = await screen.findByRole("dialog");
    listMock.mockResolvedValue([DESK, OLD, { ...LAPTOP, state: "revoked", online: false, revoked_at: NOW_ISO(), revoked_by: "admin@corp.example" }, WAITING]);
    await user.click(within(drawer).getByRole("button", { name: "Revoke" }));
    await user.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Revoke runner" }));
    await waitFor(() => expect(revokeMock).toHaveBeenCalledWith("r-laptop"));
    await waitFor(() => expect(within(drawer).queryByRole("button", { name: "Revoke" })).toBeNull());
    expect(within(drawer).getByText(/Revoked by admin@corp.example/)).toBeInTheDocument();
  });

  it("a refused revoke stays in the confirm with the server's sentence; a 404 re-reads and closes it", async () => {
    revokeMock.mockRejectedValueOnce(new HttpError(500, "closing its session did not complete")).mockRejectedValueOnce(new HttpError(404, "runner not found"));
    const user = userEvent.setup();
    renderScreen();
    await user.click(await screen.findByRole("button", { name: "laptop-7" }));
    await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Revoke" }));
    const confirm = await screen.findByRole("alertdialog");
    await user.click(within(confirm).getByRole("button", { name: "Revoke runner" }));
    expect(await within(confirm).findByRole("alert")).toHaveTextContent("closing its session did not complete");
    const reads = listMock.mock.calls.length;
    await user.click(within(confirm).getByRole("button", { name: "Revoke runner" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    await waitFor(() => expect(listMock.mock.calls.length).toBeGreaterThan(reads));
  });
});

describe("Admin -> Runners remote-hosted runner", () => {
  beforeEach(() => remoteListMock.mockResolvedValue([SHARED]));

  it("its sheet says where it is set up, shows its resources and pools, and has no fingerprint or Revoke", async () => {
    const user = userEvent.setup();
    renderScreen();
    await user.click(await screen.findByRole("button", { name: "shared-docker" }));
    const drawer = await screen.findByRole("dialog");
    expect(within(drawer).getByText("Set up in")).toBeInTheDocument();
    expect(within(drawer).getByText(/this server is configured with\. It is not someone's computer\./)).toBeInTheDocument();
    expect(within(drawer).getByText("Set by an admin")).toBeInTheDocument();
    expect(within(drawer).getByText("Runs at once: 6")).toBeInTheDocument();
    expect(within(drawer).getByText(/Running now: 1 of 6/)).toBeInTheDocument();
    expect(within(drawer).getByRole("button", { name: "General" })).toBeInTheDocument();
    expect(within(drawer).queryByRole("button", { name: "Revoke" })).toBeNull();
    expect(within(drawer).queryByText("Key fingerprint")).toBeNull();
  });

  it("a super admin edits its limits; per run can't be more than the total", async () => {
    remoteUpdateMock.mockResolvedValue(SHARED);
    const user = userEvent.setup();
    renderScreen();
    await user.click(await screen.findByRole("button", { name: "shared-docker" }));
    await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Edit limits" }));
    const dialog = await screen.findByRole("dialog", { name: "Limits for shared-docker" });
    expect(within(dialog).getByText("Remote-hosted runners need limits too. Every run on it stays within them.")).toBeInTheDocument();
    expect(within(dialog).getByText("Running now: 1. Lowering any limit never stops a run already going.")).toBeInTheDocument();
    expect(within(dialog).getByText("Total ÷ Runs at once: 24 ÷ 6 = 4 CPUs · 96 ÷ 6 = 16 GB memory · 1000 ÷ 6 = 166 GB disk. At least 1 CPU and 1 GB.")).toBeInTheDocument();
    const memory = within(dialog).getAllByRole("spinbutton", { name: "Memory (GB)" });
    setField(memory[1], "128");
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    expect(within(dialog).getAllByText("Per run can't be more than the total.").length).toBeGreaterThan(0);
    expect(remoteUpdateMock).not.toHaveBeenCalled();
    setField(memory[1], "8");
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(remoteUpdateMock).toHaveBeenCalledTimes(1));
    expect(remoteUpdateMock.mock.calls[0]).toEqual([
      "shared-docker",
      { revision: 3, limits: { runs_at_once: 6, total: { cpu_millis: 24000, memory_mib: 98304, disk_mib: 1024000 }, per_run: { cpu_millis: 4000, memory_mib: 8192, disk_mib: 102400 } } },
    ]);
  });

  // The dialog keeps its draft from when it opened, so it saves with that revision: another admin's save in
  // between is refused as stale, not overwritten by the 30 s re-read's revision.
  it("a limits save names the revision the dialog opened on, sends untouched amounts exactly, and shows a stale refusal", async () => {
    const ODD: RemoteRunnerView = { ...SHARED, limits: { ...SHARED.limits!, per_run: { cpu_millis: 1250, memory_mib: 1700, disk_mib: 102400 } } };
    remoteListMock.mockResolvedValue([ODD]);
    const stale = "shared-docker changed while this run was being set up. Review it and try again.";
    remoteUpdateMock.mockRejectedValue(new HttpError(409, stale, "runner_pool_stale"));
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    try {
      const user = userEvent.setup();
      renderScreen();
      await user.click(await screen.findByRole("button", { name: "shared-docker" }));
      await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Edit limits" }));
      const dialog = await screen.findByRole("dialog", { name: "Limits for shared-docker" });
      const lists = remoteListMock.mock.calls.length;
      remoteListMock.mockResolvedValue([{ ...ODD, revision: 4 }]);
      await act(async () => {
        vi.advanceTimersByTime(30_000);
      });
      await waitFor(() => expect(remoteListMock.mock.calls.length).toBe(lists + 1));
      setField(dialog.querySelector("#lm-rn") as HTMLInputElement, "4");
      await user.click(within(dialog).getByRole("button", { name: "Save" }));
      await waitFor(() => expect(remoteUpdateMock).toHaveBeenCalledTimes(1));
      expect(remoteUpdateMock.mock.calls[0]).toEqual(["shared-docker", { revision: 3, limits: { ...ODD.limits, runs_at_once: 4 } }]);
      expect(await within(dialog).findByText(stale)).toBeInTheDocument();
      expect(screen.getByRole("dialog", { name: "Limits for shared-docker" })).toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });

  it("a super admin makes it available to people's pools; a held change says so; a security admin reads only", async () => {
    setPeopleMock.mockResolvedValueOnce(SHARED).mockRejectedValueOnce(new PendingChangeError({ id: "chg-1" } as never));
    const user = userEvent.setup();
    const { unmount } = renderScreen();
    await user.click(await screen.findByRole("button", { name: "shared-docker" }));
    let drawer = await screen.findByRole("dialog");
    const box = within(drawer).getByRole("checkbox", { name: "People can add this runner to their own user pools" });
    expect(box).not.toBeChecked();
    await user.click(box);
    await waitFor(() => expect(setPeopleMock).toHaveBeenCalledWith("shared-docker", { revision: 3, people_pools: "everyone" }));
    await user.click(box);
    expect(await within(drawer).findByText("Held for a second person.")).toBeInTheDocument();
    unmount();
    renderScreen({ operator: false });
    await user.click(await screen.findByRole("button", { name: "shared-docker" }));
    drawer = await screen.findByRole("dialog");
    expect(within(drawer).getByRole("checkbox")).toBeDisabled();
    expect(within(drawer).queryByRole("button", { name: "Edit limits" })).toBeNull();
    expect(within(drawer).getByText("Only a super admin changes a remote-hosted runner's limits.")).toBeInTheDocument();
  });
});

describe("Admin -> Runners remote-hosted runner available to listed people", () => {
  it("shows it as available, says where the list changes, and offers no switch that would discard the list", async () => {
    remoteListMock.mockResolvedValue([{ ...SHARED, people_pools: "listed", people_subjects: [{ subject_type: "group", subject: "platform-eng" }] }]);
    const user = userEvent.setup();
    renderScreen();
    await user.click(await screen.findByRole("button", { name: "shared-docker" }));
    const drawer = await screen.findByRole("dialog");
    const box = within(drawer).getByRole("checkbox", { name: "People can add this runner to their own user pools" });
    expect(box).toBeChecked();
    expect(box).toBeDisabled();
    expect(within(drawer).getByText("Only the people, groups and roles an admin listed can add it. Change the list with wardyn runner remote people.")).toBeInTheDocument();
    await user.click(box);
    expect(clearPeopleMock).not.toHaveBeenCalled();
    expect(setPeopleMock).not.toHaveBeenCalled();
  });
});

describe("Admin -> Runners tabs", () => {
  it("has one tab strip; Pools keeps the page title, changes the lede and drops the token button", async () => {
    const user = userEvent.setup();
    renderScreen();
    await screen.findByRole("button", { name: "laptop-7" });
    const strip = screen.getByRole("navigation", { name: "Runners sections" });
    expect(within(strip).getByRole("button", { name: "Runners" })).toHaveAttribute("aria-current", "page");
    expect(screen.getByText(/^A runner is what runs sandboxes\./)).toBeInTheDocument();
    expect(screen.getByText(/^This tab lists every runner:/)).toBeInTheDocument();
    await user.click(within(strip).getByRole("button", { name: "Pools" }));
    expect(await screen.findByTestId("pools-tab")).toHaveTextContent("open:none");
    expect(screen.getByText("A pool is a named choice for where a run goes. Its limits come from its runners, and it can narrow them, never widen them.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Create a registration token" })).toBeNull();
    expect(screen.queryByText(/^This tab lists every runner:/)).toBeNull();
  });

  it("a runner's pool link opens that pool on the Pools tab, and a pool's member opens its runner", async () => {
    remoteListMock.mockResolvedValue([SHARED]);
    const user = userEvent.setup();
    renderScreen();
    await user.click(within(cardOf((await screen.findByRole("button", { name: "laptop-7" })).textContent ?? "")).getByRole("button", { name: "Laptop" }));
    expect(await screen.findByTestId("pools-tab")).toHaveTextContent("open:p-laptop");
    await user.click(screen.getByRole("button", { name: "member shared-docker" }));
    expect(await screen.findByRole("dialog")).toHaveTextContent("Set up in");
  });
});

describe("Admin -> Runners mint", () => {
  const open = async (user: ReturnType<typeof userEvent.setup>) => {
    await user.click(await screen.findByRole("button", { name: "Create a registration token" }));
    return screen.findByRole("dialog");
  };

  it("names the person, shows the token once with --org from the server's answer, and offers no claim", async () => {
    mintMock.mockResolvedValue({ ...TOKEN, owner: "alice@example.com", token: "wdr_secret", org_url: ORG });
    const user = userEvent.setup();
    renderScreen();
    const dialog = await open(user);
    expect(within(dialog).getByText("Registration token for a person")).toBeInTheDocument();
    const create = within(dialog).getByRole("button", { name: "Create token" });
    expect(create).toBeDisabled();
    setField(within(dialog).getByRole("textbox"), "alice@example.com");
    await user.click(create);
    await waitFor(() => expect(mintMock).toHaveBeenCalledWith("alice@example.com"));
    expect(await within(dialog).findByText("Registration token for alice@example.com")).toHaveFocus();
    expect(within(dialog).getByText("wdr_secret")).toBeInTheDocument();
    expect(within(dialog).getByText(`wardyn-runnerd register --org ${ORG} --token-file /path/to/token`)).toBeInTheDocument();
    expect(within(dialog).getByText("Their computer then waits for alice@example.com to claim it in Your account → My runner.")).toBeInTheDocument();
    expect(within(dialog).queryByRole("button", { name: /claim/i })).toBeNull();
  });

  it("falls back to this page's address only when the answer carries no org_url", async () => {
    mintMock.mockResolvedValue({ ...TOKEN, owner: "alice@example.com", token: "wdr_secret" });
    const user = userEvent.setup();
    renderScreen();
    const dialog = await open(user);
    setField(within(dialog).getByRole("textbox"), "alice@example.com");
    await user.click(within(dialog).getByRole("button", { name: "Create token" }));
    expect(await within(dialog).findByText(`wardyn-runnerd register --org ${window.location.origin} --token-file /path/to/token`)).toBeInTheDocument();
  });

  it("closing the token before copying it asks once; after a copy it closes quietly", async () => {
    mintMock.mockResolvedValue({ ...TOKEN, owner: "alice@example.com", token: "wdr_secret", org_url: ORG });
    const user = userEvent.setup();
    renderScreen();
    let dialog = await open(user);
    setField(within(dialog).getByRole("textbox"), "alice@example.com");
    await user.click(within(dialog).getByRole("button", { name: "Create token" }));
    await within(dialog).findByText("wdr_secret");

    await user.click(within(dialog).getByRole("button", { name: "Done" }));
    const ask = await screen.findByRole("alertdialog");
    expect(within(ask).getByText("Close without copying the token?")).toBeInTheDocument();
    expect(within(ask).getByText("You can't see this token again. If you need it, create another.")).toBeInTheDocument();
    await user.click(within(ask).getByRole("button", { name: "Keep it open" }));
    expect(screen.getByText("wdr_secret")).toBeInTheDocument();

    await user.keyboard("{Escape}");
    expect(await screen.findByText("Close without copying the token?")).toBeInTheDocument();
    await user.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Close" }));
    await waitFor(() => expect(screen.queryByText("wdr_secret")).toBeNull());

    // A copy control used: closing is quiet.
    mintMock.mockResolvedValue({ ...TOKEN, owner: "bob@example.com", token: "wdr_second", org_url: ORG });
    dialog = await open(user);
    setField(within(dialog).getByRole("textbox"), "bob@example.com");
    await user.click(within(dialog).getByRole("button", { name: "Create token" }));
    await within(dialog).findByText("wdr_second");
    await user.click(within(dialog).getByRole("button", { name: "Copy token" }));
    await user.click(within(dialog).getByRole("button", { name: "Done" }));
    await waitFor(() => expect(screen.queryByText("wdr_second")).toBeNull());
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("a refused mint shows the server's sentence and keeps the field", async () => {
    mintMock.mockRejectedValueOnce(new HttpError(422, "owner must name a person's principal", "runner_owner_invalid"));
    const user = userEvent.setup();
    renderScreen();
    const dialog = await open(user);
    setField(within(dialog).getByRole("textbox"), "nobody");
    await user.click(within(dialog).getByRole("button", { name: "Create token" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("owner must name a person's principal");
    expect(within(dialog).getByRole("textbox")).toHaveValue("nobody");
  });
});

describe("Admin -> Runners unused tokens", () => {
  it("lists them with who made them, and is absent when there are none", async () => {
    tokensMock.mockResolvedValue([TOKEN, { ...TOKEN, id: "t2", owner: "eve@example.com", minted_by: "someone-else" }]);
    const { unmount } = renderScreen();
    expect(await screen.findByText("Unused registration tokens")).toBeInTheDocument();
    const section = screen.getByText("Unused registration tokens").closest("section") as HTMLElement;
    expect(within(section).getByText("dan@example.com")).toBeInTheDocument();
    expect(within(section).getByText("you")).toBeInTheDocument();
    expect(within(section).getByText("someone-else")).toBeInTheDocument();
    unmount();
    tokensMock.mockResolvedValue([]);
    renderScreen();
    await screen.findByRole("button", { name: "laptop-7" });
    expect(screen.queryByText("Unused registration tokens")).toBeNull();
  });
});
