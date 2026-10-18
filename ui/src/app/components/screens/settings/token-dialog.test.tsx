/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The shared claim/mint dialog (#1508, mock M-SVC v2): the form, the refusals, and the show-once rules.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpError } from "../../../lib/api/core";
import type { ApiToken } from "../../../lib/types/service-identities";
import { holdsText } from "../../../../test/holds-text";
import { setField } from "../../../../test/set-field";
import { TOKEN_DIALOG as T } from "../../wardyn/copy/service-tokens";
import { TokenDialog, type TokenDialogProps } from "./token-dialog";

const SECRET = "wdn_mockonly0001_Qm3xV7pLzK9aRt2WbN8yHc5dJf4sUe6gEXAMPLE";
const NOW = Date.parse("2026-10-12T09:00:00Z");

const token = (over: Partial<ApiToken> = {}): ApiToken => ({
  id: "t1",
  principal: "dana-sub",
  role: "user",
  user_type: "standard",
  name: "ci-laptop",
  created_at: new Date(NOW).toISOString(),
  expires_at: new Date(NOW + 30 * 86_400_000).toISOString(),
  token: SECRET,
  ...over,
});

let user: ReturnType<typeof userEvent.setup>;
beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"], now: NOW });
  user = userEvent.setup({ pointerEventsCheck: 0, advanceTimers: vi.advanceTimersByTime });
});
afterEach(() => vi.useRealTimers());

function mount(over: Partial<TokenDialogProps> = {}) {
  const props: TokenDialogProps = {
    kind: "claim",
    initialName: "ci-laptop",
    submit: vi.fn(async () => token()),
    onCreated: vi.fn(),
    onRefused: vi.fn(() => false),
    onClose: vi.fn(),
    restoreFocus: vi.fn(),
    ...over,
  };
  const view = render(<TokenDialog {...props} />);
  return { props, ...view };
}

const claim = () => user.click(screen.getByRole("button", { name: T.CLAIM_SUBMIT }));

describe("TokenDialog form", () => {
  it("claims with the invitation's name and limit, and focuses the name", async () => {
    const { props } = mount({ limitSeconds: 30 * 86400, replaces: "ci-old-key" });
    expect(screen.getByRole("heading", { name: T.CLAIM_TITLE })).toBeInTheDocument();
    expect(screen.getByText(T.CLAIM_LEDE)).toBeInTheDocument();
    expect(screen.getByText(/Claiming also revokes/)).toHaveTextContent("Claiming also revokes ci-old-key. Anything still using it stops working now.");
    expect(screen.getByText(T.CLAIM_HINT_LIMITED("30 days"))).toBeInTheDocument();
    await waitFor(() => expect(screen.getByLabelText(new RegExp(T.NAME_LABEL))).toHaveFocus());
    const ttl = screen.getByLabelText(T.EXPIRES_LABEL) as HTMLSelectElement;
    expect([...ttl.options].map((o) => o.text)).toEqual([T.OPT_7_DAYS, T.OPT_30_DAYS]);
    expect(ttl.value).toBe(String(30 * 86400));
    await claim();
    expect(props.submit).toHaveBeenCalledWith({ name: "ci-laptop", ttl_seconds: 30 * 86400 });
  });

  it("offers the longest the server allows when an invitation sets no limit, and omits the lifetime for it", async () => {
    const { props } = mount();
    expect(screen.getByText(T.CLAIM_HINT_OPEN)).toBeInTheDocument();
    const ttl = screen.getByLabelText(T.EXPIRES_LABEL) as HTMLSelectElement;
    expect([...ttl.options].map((o) => o.text)).toEqual([T.OPT_7_DAYS, T.OPT_30_DAYS, T.OPT_90_DAYS, T.OPT_1_YEAR, T.OPT_LONGEST]);
    expect(ttl.value).toBe(String(90 * 86400));
    await user.selectOptions(ttl, T.OPT_LONGEST);
    await claim();
    expect(props.submit).toHaveBeenCalledWith({ name: "ci-laptop" });
  });

  it("mints for a service identity: its own wording, three lifetimes, 90 days by default", async () => {
    const { props } = mount({ kind: "mint", service: "ci-nightly", initialName: "" });
    expect(screen.getByRole("heading", { name: T.MINT_TITLE("ci-nightly") })).toBeInTheDocument();
    expect(screen.getByText(T.MINT_LEDE("ci-nightly"))).toBeInTheDocument();
    expect(screen.getByText(T.MINT_WARNING("ci-nightly"))).toBeInTheDocument();
    expect(screen.getByText(T.MINT_HINT)).toBeInTheDocument();
    const ttl = screen.getByLabelText(T.EXPIRES_LABEL) as HTMLSelectElement;
    expect([...ttl.options].map((o) => o.text)).toEqual([T.OPT_7_DAYS, T.OPT_30_DAYS, T.OPT_90_DAYS]);
    setField(screen.getByLabelText(new RegExp(T.NAME_LABEL)), "deploy");
    await user.keyboard("{Enter}");
    expect(props.submit).toHaveBeenCalledWith({ name: "deploy", ttl_seconds: 90 * 86400 });
  });

  it("refuses an empty name before any request, in the field", async () => {
    const { props } = mount({ initialName: "  " });
    await claim();
    expect(screen.getByText(T.NAME_REQUIRED)).toBeInTheDocument();
    expect(props.submit).not.toHaveBeenCalled();
    expect(screen.getByLabelText(new RegExp(T.NAME_LABEL))).toHaveAttribute("aria-invalid", "true");
  });

  it("shows the server's sentence verbatim, keeps the dialog open and the name as typed", async () => {
    const submit = vi.fn().mockRejectedValue(new HttpError(422, "name: invalid", "api_token_name_invalid"));
    mount({ submit });
    await claim();
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("name: invalid");
    expect(screen.getByLabelText(new RegExp(T.NAME_LABEL))).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByRole("button", { name: T.CLAIM_SUBMIT })).toBeEnabled();
  });

  it("says Wardyn isn't answering for a server error or a dropped connection", async () => {
    const submit = vi.fn().mockRejectedValueOnce(new HttpError(500, "internal")).mockRejectedValueOnce(new TypeError("fetch failed"));
    mount({ submit });
    await claim();
    expect(await screen.findByRole("alert")).toHaveTextContent("Wardyn isn't answering. Try again.");
    await claim();
    expect(await screen.findByRole("alert")).toHaveTextContent("Wardyn isn't answering. Try again.");
  });

  it("a stale answer leaves only Close and no warning", async () => {
    const onRefused = vi.fn(() => true);
    const { props } = mount({
      onRefused,
      replaces: "ci-old-key",
      submit: vi.fn().mockRejectedValue(new HttpError(409, "this invitation is claimed", "invitation_not_pending")),
    });
    await claim();
    expect(await screen.findByRole("alert")).toHaveTextContent("this invitation is claimed");
    expect(onRefused).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("button", { name: T.CLAIM_SUBMIT })).not.toBeInTheDocument();
    expect(screen.queryByText(/Claiming also revokes/)).not.toBeInTheDocument();
    const close = screen.getAllByRole("button", { name: T.CLOSE }).find((b) => b.closest("[data-slot=dialog-footer]"))!;
    await waitFor(() => expect(close).toHaveFocus());
    await user.click(close);
    expect(props.onClose).toHaveBeenCalledWith(false);
  });

  it("disables everything while the request is out and closes on neither Esc nor Cancel", async () => {
    let finish!: (t: ApiToken) => void;
    const { props } = mount({ submit: () => new Promise<ApiToken>((r) => (finish = r)) });
    await claim();
    expect(screen.getByRole("button", { name: T.CLAIM_BUSY })).toBeDisabled();
    expect(screen.getByRole("button", { name: T.CANCEL })).toBeDisabled();
    await user.keyboard("{Escape}");
    expect(props.onClose).not.toHaveBeenCalled();
    finish(token());
    expect(await screen.findByRole("heading", { name: T.REVEAL_TITLE })).toBeInTheDocument();
  });

  it("backs out from the form with Cancel, Esc and a click outside", async () => {
    const { props } = mount();
    await user.click(screen.getByRole("button", { name: T.CANCEL }));
    await user.keyboard("{Escape}");
    await user.click(document.body.querySelector("[data-slot=dialog-overlay]")!);
    expect(props.onClose).toHaveBeenCalledTimes(3);
    expect(props.onClose).toHaveBeenCalledWith(false);
  });
});

describe("TokenDialog show-once", () => {
  async function revealed(over: Partial<TokenDialogProps> = {}) {
    const m = mount(over);
    await claim();
    await screen.findByRole("heading", { name: T.REVEAL_TITLE });
    return m;
  }

  it("shows the token once, who it acts as and when it expires, and tells the card nothing of it", async () => {
    const { props } = await revealed({ limitSeconds: 30 * 86400 });
    expect(screen.getByTestId("show-once-token")).toHaveTextContent(SECRET);
    expect(screen.getByText(T.REVEAL_WARNING)).toBeInTheDocument();
    expect(screen.getByText(T.ACTS_AS_YOU("User"))).toBeInTheDocument();
    expect(screen.getByText("11 Nov 2026 (in 30 days)")).toBeInTheDocument();
    expect(screen.queryByText(T.CLAMPED)).not.toBeInTheDocument();
    expect(props.onCreated).toHaveBeenCalledWith();
    await waitFor(() => expect(screen.getByRole("button", { name: T.COPY })).toHaveFocus());
  });

  it("names a service token's principal, not the person", async () => {
    mount({ kind: "mint", service: "ci-nightly", initialName: "deploy", submit: vi.fn(async () => token({ principal: "service:ci-nightly" })) });
    await user.click(screen.getByRole("button", { name: T.MINT_SUBMIT }));
    await screen.findByRole("heading", { name: T.REVEAL_TITLE });
    expect(screen.getByText("service:ci-nightly")).toBeInTheDocument();
    expect(screen.queryByText(/you \(/)).not.toBeInTheDocument();
  });

  it("says the server shortened the lifetime when the token came back sooner than asked", async () => {
    await revealed({ submit: vi.fn(async () => token({ expires_at: new Date(NOW + 10 * 86_400_000).toISOString() })) });
    expect(screen.getByText(T.CLAMPED)).toBeInTheDocument();
  });

  it("a token that never expires says Never", async () => {
    await revealed({ submit: vi.fn(async () => token({ expires_at: undefined })) });
    const dl = screen.getByText(T.DETAIL_EXPIRES).parentElement!;
    expect(within(dl).getByText(T.NEVER)).toBeInTheDocument();
  });

  it("copies the token, says so, and then Done closes at once", async () => {
    const { props } = await revealed();
    await user.click(screen.getByRole("button", { name: T.COPY }));
    expect(await navigator.clipboard.readText()).toBe(SECRET);
    expect(await screen.findByRole("button", { name: T.COPIED })).toBeInTheDocument();
    expect(screen.getByText(T.COPIED_STATUS)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: T.DONE }));
    expect(props.onClose).toHaveBeenCalledWith(true);
  });

  it("with nothing copied, the first Done only warns and the second closes", async () => {
    const { props } = await revealed();
    await user.click(screen.getByRole("button", { name: T.DONE }));
    expect(props.onClose).not.toHaveBeenCalled();
    expect(screen.getByRole("alert")).toHaveTextContent(T.REVEAL_UNCOPIED);
    expect(screen.getByTestId("show-once-token")).toHaveTextContent(SECRET);
    await user.click(screen.getByRole("button", { name: T.CLOSE_UNCOPIED }));
    expect(props.onClose).toHaveBeenCalledWith(true);
  });

  it("Esc and the X follow the same rule as Done", async () => {
    const { props } = await revealed();
    await user.keyboard("{Escape}");
    expect(props.onClose).not.toHaveBeenCalled();
    expect(screen.getByText(T.REVEAL_UNCOPIED)).toBeInTheDocument();
    await user.keyboard("{Escape}");
    expect(props.onClose).toHaveBeenCalledTimes(1);
  });

  it("the built-in X follows the same rule as Done (FUG-L357)", async () => {
    const { props } = await revealed();
    await user.click(screen.getByRole("button", { name: "Close" }));
    expect(props.onClose).not.toHaveBeenCalled();
    expect(screen.getByText(T.REVEAL_UNCOPIED)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Close" }));
    expect(props.onClose).toHaveBeenCalledTimes(1);
  });

  it("a click outside never closes a token, copied or not", async () => {
    const { props } = await revealed();
    await user.click(document.body.querySelector("[data-slot=dialog-overlay]")!);
    expect(props.onClose).not.toHaveBeenCalled();
    expect(screen.queryByText(T.REVEAL_UNCOPIED)).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: T.COPY }));
    await user.click(document.body.querySelector("[data-slot=dialog-overlay]")!);
    expect(props.onClose).not.toHaveBeenCalled();
    expect(screen.getByTestId("show-once-token")).toBeInTheDocument();
  });

  it("hands focus back to the card when the card drops it", async () => {
    const { props, unmount } = await revealed();
    unmount();
    await waitFor(() => expect(props.restoreFocus).toHaveBeenCalled());
  });

  it("holds the token in its own state and nowhere else while open", async () => {
    const { container } = await revealed();
    expect(holdsText(container, SECRET)).toBe(true);
  });
});
