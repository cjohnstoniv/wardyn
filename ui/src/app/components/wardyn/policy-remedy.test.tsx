/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { PolicyRemedy, safeRequestHref } from "./policy-remedy";

const ref = (over: Record<string, string>) => ({ source: "profile", name: "Contractors", ...over });

describe("PolicyRemedy", () => {
  it("renders a link for an https: request_url, beside the owner", () => {
    render(<PolicyRemedy policy={ref({ owner: "Platform Security", request_url: "https://example.com/access" })} />);
    const a = screen.getByRole("link", { name: /Request access/ });
    expect(a).toHaveAttribute("href", "https://example.com/access");
    expect(a).toHaveAttribute("rel", "noopener noreferrer");
    expect(screen.getByTestId("policy-remedy")).toHaveTextContent("Platform Security");
  });

  it("renders a link for a mailto: request_url", () => {
    render(<PolicyRemedy policy={ref({ request_url: "mailto:access@example.com" })} />);
    expect(screen.getByRole("link", { name: /Request access/ })).toHaveAttribute("href", "mailto:access@example.com");
  });

  it("falls back to an Email link when only an email is set", () => {
    render(<PolicyRemedy policy={ref({ owner: "Platform Security", email: "access@example.com" })} />);
    expect(screen.getByRole("link", { name: "Email access@example.com" })).toHaveAttribute(
      "href",
      "mailto:access@example.com",
    );
  });

  it("falls back to the request text, verbatim and unlinked", () => {
    render(<PolicyRemedy policy={ref({ request_text: "Ask in the queue; include the run id." })} />);
    expect(screen.getByText("Ask in the queue; include the run id.")).toBeInTheDocument();
    expect(screen.queryByRole("link")).toBeNull();
  });

  it.each(["javascript:alert(1)", "data:text/html,<script>1</script>", "http://example.com", "https://u:p@example.com/"])(
    "renders plain text and no link for a planted %s request_url",
    (bad) => {
      render(<PolicyRemedy policy={ref({ owner: "Platform Security", request_url: bad })} />);
      expect(screen.queryByRole("link")).toBeNull();
      expect(screen.getByTestId("policy-remedy")).toHaveTextContent("Platform Security");
      expect(screen.getByTestId("policy-remedy")).not.toHaveTextContent(bad);
    },
  );

  it("shows an email that fails the scheme check as plain text, never a link", () => {
    render(<PolicyRemedy policy={ref({ email: "a@example.com?bcc=x@example.com" })} />);
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("renders nothing without a policy", () => {
    const { container, rerender } = render(<PolicyRemedy policy={undefined} />);
    expect(container).toBeEmptyDOMElement();
    rerender(<PolicyRemedy policy={null} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("names the wall that set the limit from the refusal's layer", () => {
    const { rerender } = render(<PolicyRemedy policy={{ source: "profile", layer: "deployment" }} />);
    expect(screen.getByTestId("policy-remedy")).toHaveTextContent("Set byWardyn boundary");
    rerender(<PolicyRemedy policy={{ source: "profile", layer: "profile", name: "Platform team" }} />);
    expect(screen.getByTestId("policy-remedy")).toHaveTextContent("Set byPlatform team walls");
    rerender(<PolicyRemedy policy={{ source: "profile", name: "Platform team", overridden: true }} />);
    expect(screen.getByTestId("policy-remedy")).toHaveTextContent("Set byThis run's policy");
  });

  it("says who to ask when the limit lists an owner but no way to ask, or neither", () => {
    const { rerender } = render(<PolicyRemedy policy={ref({ owner: "Platform Security" })} />);
    expect(screen.getByTestId("policy-remedy")).toHaveTextContent("No way to ask is listed. Contact Platform Security directly.");
    rerender(<PolicyRemedy policy={{ source: "deployment" }} />);
    const card = screen.getByTestId("policy-remedy");
    expect(card).toHaveTextContent("OwnerNot listed");
    expect(card).toHaveTextContent("This limit names no owner. Ask your Wardyn admin.");
  });

  it("shows Enforced, Can change and Copy details only when the surface asks", async () => {
    const { rerender } = render(<PolicyRemedy policy={ref({ owner: "Platform Security" })} />);
    expect(screen.queryByText("Enforced")).toBeNull();
    expect(screen.queryByText("Can change")).toBeNull();
    expect(screen.queryByRole("button", { name: "Copy details" })).toBeNull();
    rerender(
      <PolicyRemedy
        policy={ref({ owner: "Platform Security" })}
        enforced="By Wardyn, before the terminal opens"
        canChange="Not from here. The owner can."
        copyable
      />,
    );
    const card = screen.getByTestId("policy-remedy");
    expect(card).toHaveTextContent("EnforcedBy Wardyn, before the terminal opens");
    expect(card).toHaveTextContent("Can changeNot from here. The owner can.");
    expect(screen.getByRole("button", { name: "Copy details" })).toBeInTheDocument();
  });
});

describe("safeRequestHref", () => {
  it("accepts https: and one mailto: address only", () => {
    expect(safeRequestHref("https://example.com/a?b=1")).toBe("https://example.com/a?b=1");
    expect(safeRequestHref("mailto:a@example.com")).toBe("mailto:a@example.com");
    for (const bad of [
      "",
      "mailto:a@example.com?subject=x",
      "mailto:a@example.com#f",
      "mailto:a@x.com,b@x.com",
      "ftp://x",
      "//x",
      "https://",
      "https://exa mple.com",
    ]) {
      expect(safeRequestHref(bad)).toBe("");
    }
  });
});
