/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The composer screen end to end against fakes: nothing leaves before Start, a stale preview is shown again before
// anything starts, the chat is plain text, letters come back to names on screen only, and every way the assistant
// stops (provider error, turned off, no tools, ended) says so while the draft stays.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { assistant, type AssistantEvent, type AssistantPreview, type AssistantSessionView } from "../../../lib/api/assistant";
import { HttpError } from "../../../lib/api/core";
import { composerHref } from "../../../lib/composer-url";
import { CHANGES } from "../../../lib/governance-copy";
import { ASSISTANT as T } from "../../wardyn/copy/assistant";
import { NR_SAVE } from "../../wardyn/copy/new-run/save";
import { OperatorProvider } from "../../wardyn/operator-context";
import { ComposerScreen } from "./composer-screen";

class FakeSource {
  static all: FakeSource[] = [];
  onerror: (() => void) | null = null;
  closed = false;
  private handlers = new Map<string, ((m: { data: string }) => void)[]>();
  constructor(public url: string) {
    FakeSource.all.push(this);
  }
  addEventListener(kind: string, fn: (m: { data: string }) => void) {
    this.handlers.set(kind, [...(this.handlers.get(kind) ?? []), fn]);
  }
  close() {
    this.closed = true;
  }
  emit(ev: AssistantEvent) {
    for (const fn of this.handlers.get(ev.kind) ?? []) fn({ data: JSON.stringify(ev) });
  }
}

let seq = 0;
const ev = (kind: string, payload: Record<string, unknown> = {}): AssistantEvent => ({ seq: ++seq, kind, payload, created_at: new Date(0).toISOString() });
const push = (...events: AssistantEvent[]) =>
  act(() => {
    for (const e of events) FakeSource.all[FakeSource.all.length - 1].emit(e);
  });

const PREVIEW: AssistantPreview = {
  provider: "Org provider",
  model: "model-name",
  sections: [
    { label: "Your unit", text: "ceiling: github.com\nasked by Person A", untrusted: false },
    { label: "Template summary", text: "from a template", untrusted: true },
  ],
  digest: "digest-1",
  letters: { "Person A": "Alice" },
  never_sent: ["governance_membership"],
  tools: ["read"],
};
const SESSION: AssistantSessionView = { id: "5b1d2f3a-0000-4000-8000-000000000001", state: "starting", run_id: "r-77", provider: "Org provider", model: "model-name", letters: { "Person A": "Alice" } };

const availability = vi.spyOn(assistant, "getMyAssistant");
const preview = vi.spyOn(assistant, "postPreview");
const create = vi.spyOn(assistant, "createSession");
const message = vi.spyOn(assistant, "postMessage");
const end = vi.spyOn(assistant, "endSession");
const stream = vi.spyOn(assistant, "streamEvents");
const edits = vi.spyOn(assistant, "postEdits");
const checks = vi.spyOn(assistant, "postChecks");

function show(path = "/compose?kind=run") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <OperatorProvider operator={false} principal="alice">
        <ComposerScreen />
      </OperatorProvider>
    </MemoryRouter>,
  );
}

async function start() {
  show();
  await userEvent.click(await screen.findByRole("button", { name: T.START }));
  await screen.findByTestId("composer-starting");
}
async function ready() {
  await start();
  await push(ev("state", { state: "ready" }));
  await screen.findByTestId("composer-chat");
}

function phone(on: boolean) {
  Object.defineProperty(window, "innerWidth", { configurable: true, writable: true, value: on ? 390 : 1280 });
  vi.stubGlobal("matchMedia", (q: string) => ({ matches: on && q.includes("max-width"), addEventListener() {}, removeEventListener() {} }));
}

beforeEach(() => {
  seq = 0;
  FakeSource.all = [];
  vi.stubGlobal("EventSource", FakeSource);
  phone(false);
  availability.mockReset().mockResolvedValue({ state: "ready", provider_name: "Org provider", model: "model-name" });
  preview.mockReset().mockResolvedValue(PREVIEW);
  create.mockReset().mockResolvedValue(SESSION);
  message.mockReset().mockResolvedValue({ seq: 1 });
  end.mockReset().mockResolvedValue(undefined);
  edits.mockReset().mockResolvedValue({ seq: 1 });
  checks.mockReset().mockResolvedValue({ valid: true, errors: [], review: { verdict: "not_checked" }, risk: null, approvals_needed: 0, dropped: [] });
  stream.mockReset().mockImplementation((_id, _after) => new FakeSource("stub") as unknown as EventSource);
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("before Start", () => {
  it("reads the target from the URL and asks only whether the assistant is ready, then for the preview", async () => {
    show(composerHref({ kind: "unit", id: "u-1" }));
    await screen.findByTestId("composer-preview");
    expect(availability).toHaveBeenCalledWith("compose");
    expect(preview).toHaveBeenCalledWith("compose", { kind: "unit", id: "u-1" });
  });

  it("posts nothing, makes no session and opens no stream until Start is pressed", async () => {
    show();
    await screen.findByTestId("composer-preview");
    expect(preview).toHaveBeenCalledTimes(1);
    expect(create).not.toHaveBeenCalled();
    expect(message).not.toHaveBeenCalled();
    expect(stream).not.toHaveBeenCalled();
    expect(end).not.toHaveBeenCalled();
  });

  it("shows only the reason when the assistant is not ready, and asks for no preview", async () => {
    availability.mockResolvedValue({ state: "off" });
    show();
    expect(await screen.findByText(T.OFF)).toBeInTheDocument();
    expect(preview).not.toHaveBeenCalled();
    expect(screen.queryByTestId("composer-preview")).toBeNull();
  });

  it("shows the never-sent line when the organisation keeps unit records from the assistant", async () => {
    availability.mockResolvedValue({ state: "ready", never_sent_kinds: ["governance_unit"] });
    show("/compose?kind=unit");
    expect(await screen.findByText(T.KIND_NEVER_SENT(CHANGES.KIND.governance_unit))).toBeInTheDocument();
    expect(preview).not.toHaveBeenCalled();
  });

  it("shows the lead and, directly under it, the harness line; the records, their untrusted mark and the never-sent list", async () => {
    show();
    const box = await screen.findByTestId("composer-preview");
    const lead = within(box).getByText(T.PREVIEW_LEAD("Org provider · model-name"));
    expect(lead.nextElementSibling).toBe(within(box).getByTestId("composer-preview-addition"));
    expect(within(box).getByText(T.PREVIEW_ADDITION)).toBeInTheDocument();
    expect(within(box).getByText("Your unit")).toBeInTheDocument();
    expect(within(box).getAllByText(T.UNTRUSTED)).toHaveLength(1);
    // The records are shown as they will be sent: the letter is not a name here.
    expect(box.textContent).toContain("asked by Person A");
    expect(box.textContent).not.toContain("Alice");
    for (const line of [T.NEVER_SECRETS, T.NEVER_CREDENTIALS, T.NEVER_SOURCE_FILES, T.NEVER_AUDIT, T.NEVER_MOUNTS, T.NEVER_ROWS]) expect(within(box).getByText(line)).toBeInTheDocument();
    expect(within(box).getByText(T.KINDS_KEPT_AWAY(CHANGES.KIND.governance_membership))).toBeInTheDocument();
    expect(within(box).getByRole("button", { name: NR_SAVE.CANCEL_SAVE })).toBeInTheDocument();
  });
});

describe("Start", () => {
  it("creates the session with the preview's digest, shows the starting steps and the link to the run, and moves on at ready", async () => {
    await start();
    expect(create).toHaveBeenCalledTimes(1);
    expect(create).toHaveBeenCalledWith({ purpose: "compose", target: { kind: "run" }, preview_digest: "digest-1" });
    const starting = screen.getByTestId("composer-starting");
    expect(within(starting).getByText(T.STARTING)).toBeInTheDocument();
    expect(within(starting).getByText(T.RUN_POLICY("Org provider"))).toBeInTheDocument();
    expect(within(starting).getByRole("link", { name: T.OPEN_RUN })).toHaveAttribute("href", "/runs/r-77");
    expect(FakeSource.all[0].url).toBe("stub");
    await push(ev("state", { state: "ready" }));
    expect(await screen.findByTestId("composer-chat")).toBeInTheDocument();
    expect(screen.queryByTestId("composer-starting")).toBeNull();
    expect(screen.getByText(T.PROVIDER_CHIP("Org provider · model-name"))).toBeInTheDocument();
  });

  it("shows the fetch step first for a session still fetching build files", async () => {
    create.mockResolvedValue({ ...SESSION, state: "fetching", run_id: undefined });
    await start();
    expect(screen.getByText(T.FETCHING)).toBeInTheDocument();
    expect(screen.getByText(T.FETCH_LINE)).toBeInTheDocument();
  });

  it("answers a stale preview by showing the new preview with the changed line, and starts nothing until Start is pressed again", async () => {
    create.mockRejectedValueOnce(new HttpError(409, "stale", "assistant_preview_stale"));
    preview.mockResolvedValueOnce(PREVIEW).mockResolvedValueOnce({ ...PREVIEW, digest: "digest-2" });
    show();
    await userEvent.click(await screen.findByRole("button", { name: T.START }));
    expect(await screen.findByText(T.PREVIEW_CHANGED)).toBeInTheDocument();
    expect(preview).toHaveBeenCalledTimes(2);
    expect(create).toHaveBeenCalledTimes(1);
    expect(stream).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: T.START }));
    await screen.findByTestId("composer-starting");
    expect(create).toHaveBeenCalledTimes(2);
    expect(create).toHaveBeenLastCalledWith({ purpose: "compose", target: { kind: "run" }, preview_digest: "digest-2" });
  });

  it("shows a refusal of Start that is one of the assistant's own reasons as that reason", async () => {
    create.mockRejectedValueOnce(new HttpError(409, "off", "assistant_off"));
    show();
    await userEvent.click(await screen.findByRole("button", { name: T.START }));
    expect(await screen.findByText(T.OFF)).toBeInTheDocument();
    expect(stream).not.toHaveBeenCalled();
  });
});

describe("the chat", () => {
  it("renders a message as plain text: no markdown, no image, no link, no element built from it", async () => {
    await ready();
    const text = "![x](https://example.invalid/x.png) [go](https://example.invalid) <img src=x onerror=alert(1)> https://example.invalid/plain";
    await push(ev("user_message", { text }), ev("text", { text }));
    const nodes = await screen.findAllByTestId(/^composer-message-/);
    expect(nodes).toHaveLength(2);
    for (const n of nodes) {
      expect(n.textContent).toBe(text);
      expect(n.querySelector("*")).toBeNull();
      expect(n.querySelector("img, a")).toBeNull();
    }
    expect(within(screen.getByTestId("composer-chat")).queryByRole("link")).toBeNull();
    expect(document.querySelector("img")).toBeNull();
  });

  it("puts names back on screen for letters, and posts exactly what was typed", async () => {
    await ready();
    await push(ev("text", { text: "Person A can approve this." }));
    expect(await screen.findByText("Alice can approve this.")).toBeInTheDocument();
    await userEvent.type(screen.getByRole("textbox", { name: T.CHAT_PLACEHOLDER }), "ask Person A about it");
    await userEvent.click(screen.getByRole("button", { name: T.SEND }));
    await waitFor(() => expect(message).toHaveBeenCalledWith(SESSION.id, "ask Person A about it"));
    expect(message.mock.calls[0][1]).not.toContain("Alice");
  });

  it("shows a tool call as one collapsed line that opens to its arguments, its dry-run mark and the turn's token use", async () => {
    await ready();
    await push(
      ev("tool_call", { name: "read", input: "unit Payments" }),
      ev("tool_record", { name: "read", ok: true, dry_run: true }),
      ev("tool_result", { name: "read", ok: true, summary: "1 ceiling" }),
      ev("turn_end", { usage: { input_tokens: 12, output_tokens: 5 } }),
    );
    const line = await screen.findByRole("button", { name: /read/ });
    expect(line).toHaveAttribute("aria-expanded", "false");
    expect(within(line).getByText("1 ceiling")).toBeInTheDocument();
    const chat = screen.getByTestId("composer-chat");
    expect(within(chat).getByText("unit Payments")).not.toBeVisible();
    await userEvent.click(line);
    expect(line).toHaveAttribute("aria-expanded", "true");
    expect(within(chat).getByText("unit Payments")).toBeVisible();
    expect(within(chat).getByText(T.DRY_RUN)).toBeVisible();
    expect(within(chat).getByText("input_tokens 12 · output_tokens 5")).toBeVisible();
  });

  it("shows each proposal in the draft panel under the Summary's section names, with a left-out host under the parts", async () => {
    await ready();
    await push(
      ev("proposal", {
        parts: [
          { section: "allowed_hosts", value: ["github.com", "api.github.com"] },
          { section: "image", value: "node-20-dev" },
        ],
        dropped: [{ host: "artifactory.example.internal", unit: "Payments" }],
      }),
    );
    const panel = await screen.findByTestId("composer-draft-panel");
    expect(within(panel).getByText("Allowed hosts")).toBeInTheDocument();
    expect(within(panel).getByText("github.com")).toBeInTheDocument();
    expect(within(panel).getByText("Image")).toBeInTheDocument();
    expect(within(panel).getAllByText(T.PART_PROPOSED)).toHaveLength(2);
    expect(within(panel).getByText(T.LEFT_OUT("artifactory.example.internal", "Payments"))).toBeInTheDocument();
    await push(ev("proposal", { parts: [{ section: "barrier", value: "Wall" }], dropped: [] }));
    await waitFor(() => expect(within(panel).queryByText("github.com")).toBeNull());
    expect(within(panel).getByText("Barrier")).toBeInTheDocument();
  });

  it("posts an accepted part to this session, so the draft is the session's own", async () => {
    await ready();
    await push(ev("proposal", { parts: [{ section: "allowed_domains", value: ["github.com"] }], dropped: [] }));
    const panel = await screen.findByTestId("composer-draft-panel");
    await userEvent.click(within(panel).getByRole("button", { name: T.PART_ACCEPT }));
    expect(edits).toHaveBeenCalledWith(SESSION.id, { draft: { allowed_domains: ["github.com"] }, changed: [{ field: "allowed_domains", value: '["github.com"]' }] });
  });

  it("shows the audit fields from the server's own events beside the trail", async () => {
    await ready();
    await push(
      ev("tool_call", { name: "validate", input: "run draft" }),
      ev("tool_result", { name: "validate", ok: true, summary: "valid" }),
      ev("suggestion", { prompt_digest: "sha256:9f2c", output_digest: "sha256:07de", tools: ["validate"] }),
    );
    const trail = await screen.findByTestId("composer-trail");
    expect(within(trail).getByText("sha256:9f2c")).toBeInTheDocument();
    expect(within(trail).getByText("sha256:07de")).toBeInTheDocument();
    expect(within(trail).getByText(T.TRANSCRIPT_KEPT)).toBeInTheDocument();
    expect(within(trail).getByText("alice")).toBeInTheDocument();
  });
});

describe("at 390 px", () => {
  it("shows the draft as one bar with Show draft, and no side panel, until the sheet is opened", async () => {
    phone(true);
    await ready();
    await push(ev("proposal", { parts: [{ section: "image", value: "node-20-dev" }], dropped: [{ host: "a.example", unit: "Payments" }] }));
    expect(await screen.findByTestId("composer-draft-bar")).toHaveTextContent(T.DRAFT_BAR(1, 1));
    expect(screen.queryByTestId("composer-draft-panel")).toBeNull();
    expect(screen.queryByTestId("composer-trail")).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: T.SHOW_DRAFT }));
    const sheet = screen.getByTestId("composer-draft-sheet");
    expect(within(sheet).getByTestId("composer-draft-panel")).toBeInTheDocument();
    await userEvent.click(within(sheet).getByRole("button", { name: T.HIDE_DRAFT }));
    expect(screen.queryByTestId("composer-draft-sheet")).toBeNull();
  });

  it("posts an accepted part to this session from the draft sheet too", async () => {
    phone(true);
    // What the person accepted is kept per session, so this one is its own.
    const own = { ...SESSION, id: "5b1d2f3a-0000-4000-8000-000000000002" };
    create.mockResolvedValue(own);
    await ready();
    await push(ev("proposal", { parts: [{ section: "allowed_domains", value: ["github.com"] }], dropped: [] }));
    await userEvent.click(await screen.findByRole("button", { name: T.SHOW_DRAFT }));
    const sheet = screen.getByTestId("composer-draft-sheet");
    await userEvent.click(within(sheet).getByRole("button", { name: T.PART_ACCEPT }));
    expect(edits).toHaveBeenCalledWith(own.id, expect.objectContaining({ draft: { allowed_domains: ["github.com"] } }));
    // Closing and opening the sheet keeps what was accepted.
    await userEvent.click(within(sheet).getByRole("button", { name: T.HIDE_DRAFT }));
    await userEvent.click(screen.getByRole("button", { name: T.SHOW_DRAFT }));
    expect(within(screen.getByTestId("composer-draft-sheet")).getByTestId("composer-part")).toHaveAttribute("data-status", "accepted");
  });

  it("shows the side panel and no bar on a wide screen", async () => {
    await ready();
    expect(screen.getByTestId("composer-draft-panel")).toBeInTheDocument();
    expect(screen.queryByTestId("composer-draft-bar")).toBeNull();
  });
});

describe("when the assistant stops", () => {
  async function withDraft() {
    await ready();
    await push(ev("user_message", { text: "need a sandbox" }), ev("proposal", { parts: [{ section: "image", value: "node-20-dev" }], dropped: [] }));
    await screen.findByText("node-20-dev");
  }

  it("keeps the chat and the draft on a provider error, says so with Try again, and starts again from the first step", async () => {
    await withDraft();
    await push(ev("notice", { notice: "provider_error" }));
    expect(await screen.findByText(T.PROVIDER_ERROR("Org provider"))).toBeInTheDocument();
    expect(screen.getByText("need a sandbox")).toBeInTheDocument();
    expect(screen.getByText("node-20-dev")).toBeInTheDocument();
    availability.mockClear();
    await userEvent.click(screen.getByRole("button", { name: T.TRY_AGAIN }));
    await waitFor(() => expect(availability).toHaveBeenCalledTimes(1));
    await screen.findByTestId("composer-preview");
    expect(create).toHaveBeenCalledTimes(1);
  });

  it("keeps what the person accepted and edited when Try again starts a new session", async () => {
    const first = { ...SESSION, id: "5b1d2f3a-0000-4000-8000-0000000000a1" };
    const second = { ...SESSION, id: "5b1d2f3a-0000-4000-8000-0000000000a2" };
    create.mockReset().mockResolvedValueOnce(first).mockResolvedValueOnce(second);
    await ready();
    await push(ev("proposal", { parts: [{ section: "allowed_domains", value: ["github.com", "api.github.com"] }], dropped: [] }));
    const panel = await screen.findByTestId("composer-draft-panel");
    await userEvent.click(within(panel).getByRole("button", { name: T.PART_ACCEPT }));
    await userEvent.click(within(panel).getByRole("button", { name: CHANGES.OP.update }));
    await userEvent.clear(within(panel).getByRole("textbox"));
    await userEvent.click(within(panel).getByRole("textbox"));
    await userEvent.paste('["github.com"]');
    await userEvent.click(within(panel).getByRole("button", { name: T.PART_EDIT_DONE }));
    await push(ev("notice", { notice: "provider_error" }));
    await userEvent.click(await screen.findByRole("button", { name: T.TRY_AGAIN }));
    await userEvent.click(await screen.findByRole("button", { name: T.START }));
    await push(ev("state", { state: "ready" }));
    const again = await screen.findByTestId("composer-part");
    // The new session is told the carried draft once, through the same queue as every edit.
    await waitFor(() => expect(edits).toHaveBeenCalledWith(second.id, { draft: { allowed_domains: ["github.com"] }, changed: [{ field: "allowed_domains", value: '["github.com"]' }] }));
    expect(edits.mock.calls.filter((c) => c[0] === second.id)).toHaveLength(1);
    expect(again).toHaveAttribute("data-status", "accepted");
    expect(again).toHaveTextContent(T.PART_EDITED);
    expect(again).toHaveTextContent("github.com");
    expect(again).not.toHaveTextContent("api.github.com");
  });

  it("sends a chat message only after the session's pending edits have settled", async () => {
    let answer: () => void = () => undefined;
    edits.mockReset().mockImplementation(() => new Promise((ok) => (answer = () => ok({ seq: 1 }))));
    // What the person accepted is kept per session, so this one is its own.
    create.mockResolvedValue({ ...SESSION, id: "5b1d2f3a-0000-4000-8000-0000000000b1" });
    await ready();
    await push(ev("proposal", { parts: [{ section: "allowed_domains", value: ["github.com"] }], dropped: [] }));
    await userEvent.click(within(await screen.findByTestId("composer-draft-panel")).getByRole("button", { name: T.PART_ACCEPT }));
    await userEvent.type(screen.getByRole("textbox", { name: T.CHAT_PLACEHOLDER }), "now allow it");
    await userEvent.click(screen.getByRole("button", { name: T.SEND }));
    expect(message).not.toHaveBeenCalled();
    await act(async () => answer());
    await waitFor(() => expect(message).toHaveBeenCalledWith(expect.any(String), "now allow it"));
  });

  it("sends no chat message after the session's last edit post failed, shows why and keeps what was typed", async () => {
    edits.mockReset().mockRejectedValue(new Error("The session could not take that edit."));
    create.mockResolvedValue({ ...SESSION, id: "5b1d2f3a-0000-4000-8000-0000000000c1" });
    await ready();
    await push(ev("proposal", { parts: [{ section: "allowed_domains", value: ["github.com"] }], dropped: [] }));
    await userEvent.click(within(await screen.findByTestId("composer-draft-panel")).getByRole("button", { name: T.PART_ACCEPT }));
    await userEvent.type(screen.getByRole("textbox", { name: T.CHAT_PLACEHOLDER }), "now allow it");
    await userEvent.click(screen.getByRole("button", { name: T.SEND }));
    await waitFor(() => expect(screen.getAllByRole("alert").map((a) => a.textContent)).toContain("The session could not take that edit."));
    expect(message).not.toHaveBeenCalled();
    expect(screen.getByRole("textbox", { name: T.CHAT_PLACEHOLDER })).toHaveValue("now allow it");
  });

  it("drops a chat message that waited for an edit while its session ended", async () => {
    let answer: () => void = () => undefined;
    edits.mockReset().mockImplementation(() => new Promise((ok) => (answer = () => ok({ seq: 1 }))));
    create.mockResolvedValue({ ...SESSION, id: "5b1d2f3a-0000-4000-8000-0000000000c2" });
    await ready();
    await push(ev("proposal", { parts: [{ section: "allowed_domains", value: ["github.com"] }], dropped: [] }));
    await userEvent.click(within(await screen.findByTestId("composer-draft-panel")).getByRole("button", { name: T.PART_ACCEPT }));
    await userEvent.type(screen.getByRole("textbox", { name: T.CHAT_PLACEHOLDER }), "now allow it");
    await userEvent.click(screen.getByRole("button", { name: T.SEND }));
    await userEvent.click(screen.getByRole("button", { name: T.END }));
    await act(async () => answer());
    await waitFor(() => expect(end).toHaveBeenCalled());
    expect(message).not.toHaveBeenCalled();
  });

  it("answers a message refused with assistant_off by ending the chat with that reason, keeping the draft", async () => {
    await withDraft();
    message.mockRejectedValueOnce(new HttpError(409, "off", "assistant_off"));
    await userEvent.type(screen.getByRole("textbox", { name: T.CHAT_PLACEHOLDER }), "more please");
    await userEvent.click(screen.getByRole("button", { name: T.SEND }));
    expect(await screen.findByText(T.OFF)).toBeInTheDocument();
    expect(screen.queryByText(T.ENDED)).toBeNull();
    expect(screen.getByText("node-20-dev")).toBeInTheDocument();
    expect(screen.getByText("need a sandbox")).toBeInTheDocument();
    expect(screen.getByRole("textbox", { name: T.CHAT_PLACEHOLDER })).toBeDisabled();
    expect(screen.getByRole("button", { name: T.START_AGAIN })).toBeInTheDocument();
  });

  it("shows the ended wording and Start again when the session ends, keeps the draft, and Start again returns to the first step", async () => {
    await withDraft();
    await push(ev("state", { state: "idle" }));
    expect(await screen.findByText(T.ENDED)).toBeInTheDocument();
    expect(screen.getByText("node-20-dev")).toBeInTheDocument();
    availability.mockClear();
    await userEvent.click(screen.getByRole("button", { name: T.START_AGAIN }));
    await screen.findByTestId("composer-preview");
    expect(availability).toHaveBeenCalledTimes(1);
  });

  it("ends a tools_unavailable stop, which arrives while the starting view shows, with its own line and no ended wording, and stops reading the session", async () => {
    await start();
    await push(ev("error", { code: "tools_unavailable", status: 404 }));
    expect(await screen.findByText(T.TOOLS_UNAVAILABLE)).toBeInTheDocument();
    expect(FakeSource.all[0].closed).toBe(true);
    expect(screen.queryByTestId("composer-starting")).toBeNull();
    expect(screen.queryByText(T.ENDED)).toBeNull();
    expect(screen.queryByRole("button", { name: T.START_AGAIN })).toBeNull();
    expect(screen.getByRole("textbox", { name: T.CHAT_PLACEHOLDER })).toBeDisabled();
  });

  it("keeps a retried chat's two sessions apart when the second one's seqs start at 1 again", async () => {
    const errors = vi.spyOn(console, "error").mockImplementation(() => undefined);
    await ready();
    await push({ seq: 2, kind: "user_message", payload: { text: "first ask" }, created_at: "t" }, { seq: 3, kind: "text", payload: { text: "first reply" }, created_at: "t" }, { seq: 4, kind: "notice", payload: { notice: "provider_error" }, created_at: "t" });
    await userEvent.click(await screen.findByRole("button", { name: T.TRY_AGAIN }));
    await userEvent.click(await screen.findByRole("button", { name: T.START }));
    await waitFor(() => expect(FakeSource.all).toHaveLength(2));
    await push({ seq: 1, kind: "state", payload: { state: "ready" }, created_at: "t" }, { seq: 2, kind: "user_message", payload: { text: "second ask" }, created_at: "t" }, { seq: 3, kind: "text", payload: { text: "second reply" }, created_at: "t" });
    expect(await screen.findByText("second reply")).toBeInTheDocument();
    expect(screen.getByText("first reply")).toBeInTheDocument();
    expect(screen.getByText("first ask")).toBeInTheDocument();
    expect(screen.getByText("second ask")).toBeInTheDocument();
    expect(errors.mock.calls.filter((c) => String(c[0]).includes("same key"))).toHaveLength(0);
    errors.mockRestore();
  });

  it("ends the chat, with the server's own sentence, when a message is refused for a reason the screen has no state for", async () => {
    await ready();
    message.mockRejectedValueOnce(new HttpError(409, "The assistant's provider or model changed. Start again to see what will be sent.", "assistant_session_provider_changed"));
    await userEvent.type(screen.getByRole("textbox", { name: T.CHAT_PLACEHOLDER }), "more");
    await userEvent.click(screen.getByRole("button", { name: T.SEND }));
    expect(await screen.findByText("The assistant's provider or model changed. Start again to see what will be sent.")).toBeInTheDocument();
    expect(screen.queryByText(T.ENDED)).toBeNull();
    expect(screen.getByRole("button", { name: T.START_AGAIN })).toBeInTheDocument();
    expect(screen.getByRole("textbox", { name: T.CHAT_PLACEHOLDER })).toBeDisabled();
  });

  it("ends the chat when a message is refused because the person's role no longer allows the assistant", async () => {
    await ready();
    message.mockRejectedValueOnce(new HttpError(403, "You can't use the assistant.", "assistant_role_denied"));
    await userEvent.type(screen.getByRole("textbox", { name: T.CHAT_PLACEHOLDER }), "more");
    await userEvent.click(screen.getByRole("button", { name: T.SEND }));
    expect(await screen.findByText("You can't use the assistant.")).toBeInTheDocument();
    expect(screen.getByRole("textbox", { name: T.CHAT_PLACEHOLDER })).toBeDisabled();
    expect(screen.queryByRole("button", { name: T.END })).toBeNull();
  });
});

describe("ending", () => {
  it("calls endSession from End and again never for a session it already ended", async () => {
    await ready();
    await userEvent.click(screen.getByRole("button", { name: T.END }));
    await waitFor(() => expect(end).toHaveBeenCalledTimes(1));
    expect(end).toHaveBeenCalledWith(SESSION.id);
    expect(await screen.findByText(T.ENDED)).toBeInTheDocument();
    cleanup();
    expect(end).toHaveBeenCalledTimes(1);
  });

  it("ends a live session when the person leaves the screen", async () => {
    await ready();
    cleanup();
    expect(end).toHaveBeenCalledWith(SESSION.id);
    expect(FakeSource.all[0].closed).toBe(true);
  });

  it("says what the server said when the stop failed, keeps End, and a second End that works closes the chat", async () => {
    await ready();
    end.mockRejectedValueOnce(new HttpError(500, "The run could not be stopped. Try again."));
    await userEvent.click(screen.getByRole("button", { name: T.END }));
    expect(await screen.findByRole("alert")).toHaveTextContent("The run could not be stopped. Try again.");
    expect(screen.queryByText(T.ENDED)).toBeNull();
    expect(screen.getByRole("textbox", { name: T.CHAT_PLACEHOLDER })).toBeEnabled();
    await userEvent.click(screen.getByRole("button", { name: T.END }));
    expect(await screen.findByText(T.ENDED)).toBeInTheDocument();
    expect(end).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("ends a session that was made after the person left, and opens no stream for it", async () => {
    let made!: (s: AssistantSessionView) => void;
    create.mockReturnValueOnce(new Promise<AssistantSessionView>((r) => (made = r)));
    show();
    await userEvent.click(await screen.findByRole("button", { name: T.START }));
    cleanup();
    await act(async () => made(SESSION));
    expect(end).toHaveBeenCalledWith(SESSION.id);
    expect(stream).not.toHaveBeenCalled();
    expect(FakeSource.all).toHaveLength(0);
  });

  it("shows nothing from a preview that arrives after the person left", async () => {
    let shown!: (p: AssistantPreview) => void;
    preview.mockReturnValueOnce(new Promise<AssistantPreview>((r) => (shown = r)));
    const { unmount } = show();
    await waitFor(() => expect(preview).toHaveBeenCalledTimes(1));
    unmount();
    const errors = vi.spyOn(console, "error").mockImplementation(() => undefined);
    await act(async () => shown(PREVIEW));
    expect(screen.queryByTestId("composer-preview")).toBeNull();
    expect(errors).not.toHaveBeenCalled();
    errors.mockRestore();
  });

  it("makes no end call on leaving before Start", async () => {
    show();
    await screen.findByTestId("composer-preview");
    cleanup();
    expect(end).not.toHaveBeenCalled();
  });
});
