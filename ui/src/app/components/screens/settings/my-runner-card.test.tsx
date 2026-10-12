/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// 0.9 Your account -> My runner (packet M-RN-2). The strings are written out, not read from the copy module,
// so a drift in copy/runners.ts fails this suite.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { RunnerRegistrationToken, RunnerView } from "../../../lib/types/runners";

const listMineMock = vi.fn();
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
  },
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { HttpError } from "../../../lib/api/core";
import { aheadByHours } from "../../../lib/test-clock";
import { OperatorProvider } from "../../wardyn/operator-context";
import { MyRunnerCard } from "./my-runner-card";
import { setField } from "../../../../test/set-field";

const FULL = "SHA256:3Jd1d8XqTHEWHOLEFINGERPRINTVALUEk2Q9";
const ABBREV = "SHA256:3Jd1d8…k2Q9";

function runner(over: Partial<RunnerView> = {}): RunnerView {
  return {
    id: "r-laptop",
    name: "laptop-7",
    state: "claimed",
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
    runs_active: 2,
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
  runs_active: 0,
});
const TOKEN: RunnerRegistrationToken = {
  id: "t1",
  owner: "alice",
  minted_by: "alice",
  token: "wdr_selfminted",
  created_at: aheadByHours(0),
  expires_at: aheadByHours(1),
  org_url: "https://org.example.com",
};

function renderCard(hash = "") {
  return render(
    <MemoryRouter initialEntries={[`/account${hash}`]}>
      <OperatorProvider operator={false} securityOperator={false} operatorResolved principal="alice">
        <MyRunnerCard />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

const expand = async (user: ReturnType<typeof userEvent.setup>) => {
  await user.click(await screen.findByRole("button", { name: /^My runner/ }));
};

beforeEach(() => {
  for (const m of [listMineMock, mintMineMock, claimMock, revokeMineMock]) m.mockReset();
  listMineMock.mockResolvedValue([CLAIMED]);
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
    expect(screen.getByText(/A computer you registered runs your own runs, under your organisation's policy and audit\./)).toBeInTheDocument();
    expect(screen.getByText("You haven't registered a runner.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Register a runner" })).toBeInTheDocument();
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
  it("shows version, runs, folders, posture, capacity and the whole fingerprint, each fact labelled as reported, with no copy control", async () => {
    const user = userEvent.setup();
    renderCard();
    await expand(user);
    expect(await screen.findByText("Version 0.9.0 · Runs on it now: 2 running")).toBeInTheDocument();
    expect(screen.getByText("/Users/alice/src")).toBeInTheDocument();
    expect(screen.getByText("Runs on it can use folders inside these only. They're set on the computer itself.")).toBeInTheDocument();
    expect(screen.getByText("macOS 15.2 · Managed by MDM · Disk encrypted")).toBeInTheDocument();
    expect(screen.getByText("Up to 4 CPUs and 8192 MiB per run")).toBeInTheDocument();
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
    expect(screen.getAllByText("Not reported yet").length).toBeGreaterThanOrEqual(3);
    expect(screen.queryByText("Disk not encrypted")).toBeNull();
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
