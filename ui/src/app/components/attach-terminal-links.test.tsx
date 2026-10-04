/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The terminal link policy (term-t13): what opens in one click, what asks, and
// what never opens. Every target here is text the sandbox controls.
import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { decideLink, terminalLinkHandlers } from "./attach-terminal-links";
import { LinkConfirmDialog } from "./attach-link-dialog";

const CONSOLE = "https://console.example";
const action = (raw: string) => decideLink(raw, CONSOLE).action;

afterEach(() => vi.restoreAllMocks());

describe("decideLink", () => {
  it("opens an allowlisted device-login link in one click", () => {
    expect(action("https://github.com/login/device")).toBe("open");
    expect(action("https://github.com/login/device?user_code=ABCD-1234")).toBe("open");
    expect(action("https://microsoft.com/devicelogin")).toBe("open");
  });

  it("asks for everything else, including a host-only match and the console origin", () => {
    expect(action("https://github.com/")).toBe("confirm");
    expect(action("https://github.com/login/device-evil")).toBe("confirm");
    expect(action("https://github.com/login/device/../../evil")).toBe("confirm");
    expect(action("https://github.com:8443/login/device")).toBe("confirm");
    expect(action("http://github.com/login/device")).toBe("confirm");
    expect(action("https://microsoft.com.evil.example/devicelogin")).toBe("confirm");
    expect(action(`${CONSOLE}/runs`)).toBe("confirm");
  });

  it("never matches the allowlist through userinfo", () => {
    expect(action("https://github.com@evil.example/login/device")).toBe("confirm");
    expect(decideLink("https://github.com@evil.example/login/device", CONSOLE)).toMatchObject({
      url: { host: "evil.example" },
    });
    // Even on an allowlisted origin, a name before the host asks first.
    expect(action("https://user@github.com/login/device")).toBe("confirm");
  });

  it("refuses anything that is not http(s), and anything that does not parse", () => {
    expect(action("javascript:alert(1)")).toBe("refuse");
    expect(action("data:text/html,x")).toBe("refuse");
    expect(action("file:///etc/passwd")).toBe("refuse");
    expect(action("vbscript:x")).toBe("refuse");
    expect(action("not a url")).toBe("refuse");
  });
});

describe("terminalLinkHandlers", () => {
  const click = new MouseEvent("click");

  it("detected and OSC 8 links share one policy", () => {
    const open = vi.spyOn(window, "open").mockReturnValue(null);
    const confirm = vi.fn();
    const h = terminalLinkHandlers(confirm);
    for (const activate of [h.detected, h.osc8.activate]) {
      activate(click, "https://github.com/login/device");
      expect(open).toHaveBeenLastCalledWith("https://github.com/login/device", "_blank", "noopener,noreferrer");
      expect(confirm).not.toHaveBeenCalled();
    }
    open.mockClear();
    for (const activate of [h.detected, h.osc8.activate]) {
      activate(click, "https://example.com/x");
      expect(confirm).toHaveBeenLastCalledWith(expect.objectContaining({ href: "https://example.com/x" }));
    }
    expect(open).not.toHaveBeenCalled();
  });

  it("neither entry point opens or asks for a javascript: link", () => {
    const open = vi.spyOn(window, "open").mockReturnValue(null);
    const confirm = vi.fn();
    const h = terminalLinkHandlers(confirm);
    h.detected(click, "javascript:alert(1)");
    h.osc8.activate(click, "javascript:alert(1)");
    expect(open).not.toHaveBeenCalled();
    expect(confirm).not.toHaveBeenCalled();
    expect(h.osc8.allowNonHttpProtocols).toBe(false);
  });
});

describe("LinkConfirmDialog", () => {
  it("shows the normalised href with the host highlighted and a userinfo warning, and opens noopener on Open link", () => {
    const open = vi.spyOn(window, "open").mockReturnValue(null);
    const onClose = vi.fn();
    render(<LinkConfirmDialog url={new URL("https://github.com@evil.example/login/device")} onClose={onClose} onCloseFocus={() => {}} />);
    expect(screen.getByText("Open this link?")).toBeTruthy();
    expect(screen.getByTestId("terminal-link-url").textContent).toBe("https://github.com@evil.example/login/device");
    expect(screen.getByTestId("terminal-link-host").textContent).toBe("evil.example");
    expect(screen.getByTestId("terminal-link-userinfo").textContent).toBe(
      "This address has a name before the host. The site it opens is evil.example.",
    );
    fireEvent.click(screen.getByRole("button", { name: "Open link" }));
    expect(open).toHaveBeenCalledWith("https://github.com@evil.example/login/device", "_blank", "noopener,noreferrer");
  });

  it("has no warning line without userinfo, and Cancel opens nothing", () => {
    const open = vi.spyOn(window, "open").mockReturnValue(null);
    const onClose = vi.fn();
    render(<LinkConfirmDialog url={new URL("https://example.com/a?b=1")} onClose={onClose} onCloseFocus={() => {}} />);
    expect(screen.queryByTestId("terminal-link-userinfo")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onClose).toHaveBeenCalled();
    expect(open).not.toHaveBeenCalled();
  });
});
