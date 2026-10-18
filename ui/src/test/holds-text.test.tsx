/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// holdsText finds a secret a component holds even when its props are first met deep inside a parent's JSX (FR-0095).
import * as React from "react";
import { describe, expect, it } from "vitest";
import { render } from "@testing-library/react";

import { holdsText } from "./holds-text";

const Wrap = ({ children }: { children?: React.ReactNode }) => <div>{children}</div>;
const Row = (_: { result: unknown }) => null;
const SECRET = "tok-secret-9f2";

describe("holdsText", () => {
  it("finds a secret nested past the depth cap when the holder is reached first through a parent's children", () => {
    const tree = () => (
      <Wrap>
        <Wrap>
          <Wrap>
            <Row result={{ minted: { token: { value: SECRET } } }} />
          </Wrap>
        </Wrap>
      </Wrap>
    );
    const { container, rerender } = render(tree());
    // The container's root fiber is the first one React made; it has children once a second render has used it.
    rerender(tree());
    expect(holdsText(container, SECRET)).toBe(true);
    expect(holdsText(container, "not-there")).toBe(false);
  });
});
