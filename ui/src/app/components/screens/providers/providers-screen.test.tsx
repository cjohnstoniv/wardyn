/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The /providers screen — the drives-screen.tsx precedent: forbidden (a 403 is
// the TIER, not the network), fetch-failed (distinct from empty), the legacy
// banner (zero rows), populated, and exactly ONE teal button at a time.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpError } from "../../../lib/api/core";
import { AGENTS, PROVIDERS, PROVIDERS_DRAFT } from "../../../lib/workspace-providers-copy";
import { ACCESS_STATE } from "../../../lib/people-access-copy";
import { OperatorProvider } from "../../wardyn/operator-context";
import { baseStatus } from "../../../lib/test-fixtures";
import { ProvidersScreen } from "./providers-screen";

const getWorkspaceProvidersMock = vi.fn();
const putWorkspaceProvidersMock = vi.fn();
vi.mock("../../../lib/api/providers", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/providers")>("../../../lib/api/providers");
  return {
    ...actual,
    providers: {
      ...actual.providers,
      getWorkspaceProviders: (...a: unknown[]) => getWorkspaceProvidersMock(...a),
      putWorkspaceProviders: (...a: unknown[]) => putWorkspaceProvidersMock(...a),
    },
  };
});

const getAgentProvidersMock = vi.fn();
const putAgentProvidersMock = vi.fn();
vi.mock("../../../lib/api/agent-providers", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/agent-providers")>("../../../lib/api/agent-providers");
  return {
    ...actual,
    agentProviders: {
      ...actual.agentProviders,
      getAgentProviders: (...a: unknown[]) => getAgentProvidersMock(...a),
      putAgentProviders: (...a: unknown[]) => putAgentProvidersMock(...a),
    },
  };
});

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));

vi.mock("../../../lib/api/drives", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api/drives")>("../../../lib/api/drives");
  return { ...actual, drives: { ...actual.drives, getDrives: () => Promise.resolve({ drives: [], grants: [], host_roots_configured: false, runner_target: "" }) } };
});

vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual<typeof import("react-router-dom")>("react-router-dom");
  return { ...actual, useNavigate: () => vi.fn() };
});

function renderScreen(operator = true) {
  return render(
    <OperatorProvider operator={operator}>
      <ProvidersScreen />
    </OperatorProvider>,
  );
}

beforeEach(() => {
  getWorkspaceProvidersMock.mockReset();
  putWorkspaceProvidersMock.mockReset();
  getSetupStatusMock.mockReset();
  getSetupStatusMock.mockResolvedValue(baseStatus());
  getAgentProvidersMock.mockReset();
  getAgentProvidersMock.mockResolvedValue({ providers: {}, etag: '"a0"' });
  putAgentProvidersMock.mockReset();
});

describe("ProvidersScreen", () => {
  it("a 403 renders the tier refusal, not a fetch-failed banner", async () => {
    getWorkspaceProvidersMock.mockRejectedValue(new HttpError(403, "Requires the admin role."));
    renderScreen();
    expect(await screen.findByText(/requires the admin role/i)).toBeInTheDocument();
    expect(screen.queryByText(PROVIDERS.FETCH_FAILED_TITLE)).not.toBeInTheDocument();
  });

  it("a non-403 failure renders FETCH_FAILED with Retry, never a confident empty", async () => {
    getWorkspaceProvidersMock.mockRejectedValue(new Error("boom"));
    renderScreen();
    expect(await screen.findByText(PROVIDERS.FETCH_FAILED_TITLE)).toBeInTheDocument();
    expect(screen.getByText(PROVIDERS.FETCH_FAILED_BODY)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /retry/i })).toBeInTheDocument();
  });

  it("zero rows renders the Git tab's legacy-open banner by default", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({ providers: {}, etag: '"e0"' });
    renderScreen();
    expect(await screen.findByText(PROVIDERS.LEGACY_OPEN_TITLE)).toBeInTheDocument();
  });

  it("renders the populated Git tab with rows from the wire", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({
      providers: { git: [{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }] },
      etag: '"e1"',
    });
    renderScreen();
    expect(await screen.findByTestId("provider-row-github")).toBeInTheDocument();
  });

  it("carries exactly ONE teal (default) button at a time — legacy-open mode", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({ providers: {}, etag: '"e2"' });
    renderScreen();
    await screen.findByText(PROVIDERS.LEGACY_OPEN_TITLE);
    // The legacy banner's own Add provider is the state's one affirmative;
    // Save providers is withheld while it shows (providers-screen.tsx).
    const teal = screen.getAllByRole("button").filter((b) => b.className.split(/\s+/).includes("bg-primary"));
    expect(teal.length).toBe(1);
    expect(teal[0]).toHaveTextContent(PROVIDERS.ADD_ROW_CTA);
  });

  // The pin that would have caught it: a populated row's OWN credential-lane
  // Save button (SecretLane, rendered by default on the row's initial
  // selected lane) must not compete with the screen's Save providers — the
  // lane's Save is `saveVariant="secondary"` for exactly this reason
  // (git-tab.tsx).
  it("carries exactly ONE teal (default) button at a time — a populated row", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({
      providers: { git: [{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }] },
      etag: '"e2b"',
    });
    renderScreen();
    await screen.findByTestId("provider-row-github");
    const teal = screen.getAllByRole("button").filter((b) => b.className.split(/\s+/).includes("bg-primary"));
    expect(teal.length).toBe(1);
    expect(teal[0]).toHaveTextContent(PROVIDERS.SAVE_CTA);
  });

  it("saves the whole document and shows the saved toast", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({
      providers: { git: [{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }] },
      etag: '"e3"',
    });
    putWorkspaceProvidersMock.mockResolvedValue({
      providers: { git: [{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }] },
      etag: '"e4"',
      sourcesNoLongerAdmitted: 0,
    });
    renderScreen();
    await screen.findByTestId("provider-row-github");
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));
    expect(putWorkspaceProvidersMock).toHaveBeenCalled();
  });

  // V1 lens E finding 2 (HIGH): Save was withheld on the DRAFT's emptiness, so
  // removing the last row hid the only button that could commit the removal —
  // the tab flipped to the legacy-open banner whose only action is Add, and the
  // change was discarded on navigation. The withhold is on the LOADED snapshot.
  it("removing the last row leaves Save available, and the PUT carries an empty git set", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({
      providers: { git: [{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }] },
      etag: '"e7"',
    });
    putWorkspaceProvidersMock.mockResolvedValue({ providers: {}, etag: '"e8"', sourcesNoLongerAdmitted: 0 });
    renderScreen();
    const row = await screen.findByTestId("provider-row-github");
    await userEvent.click(within(row).getByRole("button", { name: "Remove" }));
    await userEvent.click(within(screen.getByRole("alertdialog")).getByRole("button", { name: "Remove" }));

    expect(await screen.findByText(PROVIDERS.LEGACY_OPEN_TITLE)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));
    expect(putWorkspaceProvidersMock).toHaveBeenCalledWith({ git: [] }, '"e7"');
    // Saved: the org IS in legacy-open mode now, so the banner's Add owns the
    // one affirmative again and Save steps back off.
    await waitFor(() => expect(screen.queryByRole("button", { name: PROVIDERS.SAVE_CTA })).not.toBeInTheDocument());
  });

  it("a genuinely empty loaded state still withholds Save", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({ providers: {}, etag: '"e9"' });
    renderScreen();
    await screen.findByText(PROVIDERS.LEGACY_OPEN_TITLE);
    expect(screen.queryByRole("button", { name: PROVIDERS.SAVE_CTA })).not.toBeInTheDocument();
  });

  // The withhold is "nothing to save", not "the org started empty": on a fresh
  // install the first row an admin adds is a pending change, and Save must
  // appear with it — the providers e2e authoring walk caught a rule that hid
  // Save whenever the loaded snapshot was empty.
  it("a fresh install shows Save the moment the first row is added, and the PUT carries it", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({ providers: {}, etag: '"e10"' });
    putWorkspaceProvidersMock.mockResolvedValue({ providers: {}, etag: '"e11"', sourcesNoLongerAdmitted: 0 });
    renderScreen();
    await screen.findByText(PROVIDERS.LEGACY_OPEN_TITLE);
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.ADD_ROW_CTA }));
    await screen.findByTestId("provider-row-github");
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));
    expect(putWorkspaceProvidersMock).toHaveBeenCalledWith(
      expect.objectContaining({ git: [expect.objectContaining({ kind: "github" })] }),
      '"e10"',
    );
  });

  // V1 lens E finding 1 (HIGH), the other half: a second address typed into the
  // textarea reaches the PUT — the write path, not just the rendered value.
  it("Save sends a second base URL typed into the textarea", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({
      providers: { git: [{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }] },
      etag: '"ea"',
    });
    putWorkspaceProvidersMock.mockResolvedValue({
      providers: { git: [{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }] },
      etag: '"eb"',
      sourcesNoLongerAdmitted: 0,
    });
    renderScreen();
    const row = await screen.findByTestId("provider-row-github");
    await userEvent.type(within(row).getByLabelText(PROVIDERS.FIELD_BASE_URLS), "{Enter}https://git.corp.example/team");
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));
    expect(putWorkspaceProvidersMock.mock.calls[0][0].git[0].base_urls).toEqual([
      "https://github.com/acme",
      "https://git.corp.example/team",
    ]);
  });

  // #460 — the dirty chip: beside the screen's own title (PageHeader) and
  // beside the Git/Storage Segmented tab labels (they share this one draft),
  // the same fact the bottom-of-tab marker already carried. Gone again once
  // the draft matches what Save just wrote back.
  it("shows the dirty chip once the draft differs from what loaded, and clears it after Save", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({
      providers: { git: [{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }] },
      etag: '"h0"',
    });
    putWorkspaceProvidersMock.mockResolvedValue({
      providers: {
        git: [{ id: "github", kind: "github", base_urls: ["https://github.com/acme", "https://git.corp.example/team"] }],
      },
      etag: '"h1"',
      sourcesNoLongerAdmitted: 0,
    });
    renderScreen();
    const row = await screen.findByTestId("provider-row-github");
    expect(screen.queryAllByText(PROVIDERS_DRAFT.UNSAVED_MARKER)).toHaveLength(0);

    await userEvent.type(within(row).getByLabelText(PROVIDERS.FIELD_BASE_URLS), "{Enter}https://git.corp.example/team");
    expect(screen.getAllByText(PROVIDERS_DRAFT.UNSAVED_MARKER).length).toBeGreaterThan(0);

    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));
    await waitFor(() => expect(screen.queryAllByText(PROVIDERS_DRAFT.UNSAVED_MARKER)).toHaveLength(0));
  });

  // F4-F3: the banner must not SWAP the whole tab body — that would discard
  // an edit typed moments before the 412 and make it unreadable. The draft
  // stays MOUNTED (the edited textarea survives), and the banner's ONE
  // control is "Discard mine and reload" — no "Save over theirs" arm (a
  // security document is never last-writer-wins from this banner).
  it("a 412 renders the saved-elsewhere state, keeps the draft mounted, and never overwrites", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({
      providers: { git: [{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }] },
      etag: '"e5"',
    });
    putWorkspaceProvidersMock.mockRejectedValue(new HttpError(412, "providers changed since you loaded them — reload and retry"));
    renderScreen();
    const row = await screen.findByTestId("provider-row-github");
    await userEvent.type(within(row).getByLabelText(PROVIDERS.FIELD_BASE_URLS), "{Enter}https://git.corp.example/team");
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));

    expect(await screen.findByText(PROVIDERS.SAVED_ELSEWHERE_TITLE)).toBeInTheDocument();
    // The draft is still mounted and readable — the edited line survives (a
    // controlled textarea's value is a DOM property, not text content).
    expect(within(row).getByLabelText(PROVIDERS.FIELD_BASE_URLS)).toHaveValue(
      "https://github.com/acme\nhttps://git.corp.example/team",
    );
    // ONE control on the banner: Discard mine and reload. No "Save over
    // theirs" — a second re-PUT arm the corrected verdict refused.
    expect(screen.getByRole("button", { name: PROVIDERS_DRAFT.DISCARD_AND_RELOAD })).toBeInTheDocument();
    expect(screen.queryByText(/save over theirs/i)).not.toBeInTheDocument();
    // Save providers is STILL on screen (the draft is still there to save) —
    // an ordinary retry sends If-Match, as the sibling test below pins.
    expect(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA })).toBeInTheDocument();
  });

  it("a 400 renders the server's own refusal verbatim under SAVE_REFUSED_TITLE", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({
      providers: { git: [{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }] },
      etag: '"e6"',
    });
    putWorkspaceProvidersMock.mockRejectedValue(new HttpError(400, 'id "github" is not unique'));
    renderScreen();
    await screen.findByTestId("provider-row-github");
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));
    expect(await screen.findByText(PROVIDERS.SAVE_REFUSED_TITLE)).toBeInTheDocument();
    expect(screen.getByText('id "github" is not unique')).toBeInTheDocument();
  });

  // V1 r2 MEDIUM: an emptied base-URLs field committed `base_urls: []` with Save
  // still enabled — a guaranteed 400 (validateWorkspaceProviders refuses a row
  // with no addresses), so there was nothing honest to send. The PUT carries the
  // whole document, so the withhold is not per-tab.
  it("Save is withheld while any git row has no addresses, and returns the moment one is typed", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({
      providers: { git: [{ id: "github", kind: "github", base_urls: ["https://github.com/acme"] }] },
      etag: '"ec"',
    });
    renderScreen();
    const row = await screen.findByTestId("provider-row-github");
    const save = screen.getByRole("button", { name: PROVIDERS.SAVE_CTA });
    expect(save).toBeEnabled();
    const textarea = within(row).getByLabelText(PROVIDERS.FIELD_BASE_URLS);
    await userEvent.clear(textarea);
    expect(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA })).toBeDisabled();
    await userEvent.type(textarea, "https://github.com/acme");
    expect(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA })).toBeEnabled();
    expect(putWorkspaceProvidersMock).not.toHaveBeenCalled();
  });

  // V1 r2 HIGH: the roster comes from THIS screen's /setup/status read, but the
  // Agents tab's roster-unknown Retry called the tab's own load() — which
  // re-reads /agent-providers, the read that had NOT failed. Three clicks, three
  // getAgentProviders calls, the same dead end.
  it("the Agents tab's roster-unknown Retry re-fires the screen's setup-status read", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({ providers: {}, etag: '"ed"' });
    // baseStatus() carries no `harnesses` — an older daemon, or a partial read:
    // UNKNOWN, which the tab renders as the same dead end a failed GET does.
    renderScreen();
    await screen.findByText(PROVIDERS.LEGACY_OPEN_TITLE);
    await userEvent.click(screen.getByRole("button", { name: AGENTS.AGENTS_TITLE }));
    expect(await screen.findByText(PROVIDERS.FETCH_FAILED_TITLE)).toBeInTheDocument();

    const statusReads = getSetupStatusMock.mock.calls.length;
    const agentReads = getAgentProvidersMock.mock.calls.length;
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ harnesses: [{ id: "claude-code", display: "Claude Code", has_gateway: true, has_login: true }] }),
    );
    await userEvent.click(screen.getByRole("button", { name: ACCESS_STATE.FETCH_FAILED_RETRY }));

    expect(getSetupStatusMock.mock.calls.length).toBe(statusReads + 1);
    // The roster lands, so the rows render and the tab's own Save appears.
    expect(await screen.findByTestId("agent-row-claude-code")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA })).toBeInTheDocument();
    // The dead-end click must not spend a /agent-providers read.
    expect(getAgentProvidersMock.mock.calls.length).toBe(agentReads);
  });

  // A-01: the Agents tab's own Save must not re-fire the PARENT's WHOLE
  // `load()`, which would reset `draft` (the shared document GitTab/
  // StorageTab render) to whatever /workspace-providers last GET —
  // silently discarding an admin's un-saved Git-tab edit made moments
  // earlier on a different tab. Save on Agents must re-read /setup/status
  // ONLY (`onStatusRefresh`), never /workspace-providers.
  it("saving the Agents tab does not discard an unsaved Git-tab edit", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({ providers: {}, etag: '"f0"' });
    getSetupStatusMock.mockResolvedValue(
      baseStatus({ harnesses: [{ id: "claude-code", display: "Claude Code", has_gateway: true, has_login: true }] }),
    );
    putAgentProvidersMock.mockResolvedValue({ providers: { agents: [] }, etag: '"f2"' });
    renderScreen();
    await screen.findByText(PROVIDERS.LEGACY_OPEN_TITLE);

    // An unsaved git row, typed but never Saved on this (Git) tab.
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.ADD_ROW_CTA }));
    expect(await screen.findByTestId("provider-row-github")).toBeInTheDocument();

    // Switch to Agents (its own GET/PUT resource) and Save there.
    await userEvent.click(screen.getByRole("button", { name: AGENTS.AGENTS_TITLE }));
    await screen.findByTestId("agent-row-claude-code");
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));
    await waitFor(() => expect(putAgentProvidersMock).toHaveBeenCalled());
    // /workspace-providers must NOT be re-read by this save — that would be
    // the whole-screen `load()` this fix removes from the call path.
    expect(getWorkspaceProvidersMock.mock.calls.length).toBe(1);

    // Back on Git: the unsaved row must still be there, never reset to the
    // server's last-loaded (empty) snapshot.
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.GIT_TITLE }));
    expect(await screen.findByTestId("provider-row-github")).toBeInTheDocument();
    expect(screen.queryByText(PROVIDERS.LEGACY_OPEN_TITLE)).not.toBeInTheDocument();
  });

  // R-02 (review): a rejected /setup/status refresh (Appendix A finding 2's
  // shape — a stale `harnesses` read strands the per_user banner and its
  // start-URL prompt) re-fires once instead of giving up on the first
  // failure — the ordinary case is one transient request, not an outage.
  it("refreshSetupStatus re-fires once after a rejection, then applies the retry's result", async () => {
    getWorkspaceProvidersMock.mockResolvedValue({ providers: {}, etag: '"g0"' });
    getSetupStatusMock
      .mockResolvedValueOnce(
        baseStatus({ harnesses: [{ id: "claude-code", display: "Claude Code", has_gateway: true, has_login: true }] }),
      )
      .mockRejectedValueOnce(new Error("network blip"))
      .mockResolvedValueOnce(
        baseStatus({
          harnesses: [
            {
              id: "claude-code",
              display: "Claude Code",
              has_gateway: true,
              has_login: true,
              mechanism: "bedrock_sso",
              credential_source: "per_user",
            },
          ],
          model_access: { state: "not_configured", action: "Sign in to AWS" },
        }),
      );
    putAgentProvidersMock.mockResolvedValue({
      providers: { agents: [{ id: "claude-code", mechanism: "bedrock_sso", credential_source: "per_user" }] },
      etag: '"g2"',
    });
    renderScreen();
    await screen.findByText(PROVIDERS.LEGACY_OPEN_TITLE);

    await userEvent.click(screen.getByRole("button", { name: AGENTS.AGENTS_TITLE }));
    await screen.findByTestId("agent-row-claude-code");
    await userEvent.click(screen.getByRole("button", { name: PROVIDERS.SAVE_CTA }));
    await waitFor(() => expect(putAgentProvidersMock).toHaveBeenCalled());

    // The retry's result (the saved per_user row) lands, proving the second
    // /setup/status call was made and applied despite the first rejecting.
    expect(await screen.findByTestId("per-user-sign-in-banner")).toBeInTheDocument();
    expect(getSetupStatusMock.mock.calls.length).toBe(3);
  });
});
