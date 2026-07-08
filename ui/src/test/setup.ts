/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Vitest global setup: registers @testing-library/jest-dom matchers and clears
// the DOM between tests.
import "@testing-library/jest-dom/vitest";
import { afterEach, vi } from "vitest";
import { cleanup } from "@testing-library/react";

afterEach(() => {
  cleanup();
});

// jsdom doesn't implement a handful of DOM APIs that Radix UI primitives
// (DropdownMenu, Select, Dialog) call during open/close. Without these, opening
// a dropdown/select in a test throws "scrollIntoView is not a function" /
// "hasPointerCapture is not a function". Polyfill them as no-ops so component
// tests can drive these primitives.
if (typeof Element !== "undefined") {
  if (!Element.prototype.scrollIntoView) {
    Element.prototype.scrollIntoView = vi.fn();
  }
  if (!Element.prototype.hasPointerCapture) {
    Element.prototype.hasPointerCapture = vi.fn(() => false);
  }
  if (!Element.prototype.setPointerCapture) {
    Element.prototype.setPointerCapture = vi.fn();
  }
  if (!Element.prototype.releasePointerCapture) {
    Element.prototype.releasePointerCapture = vi.fn();
  }
}

// jsdom's Blob/File does not implement async text()/arrayBuffer(). The compose
// form reads attachment files via file.text(); polyfill it from the underlying
// parts so component tests can exercise the real read path. (Real browsers ship
// Blob.text(); this only fills the jsdom gap.)
if (typeof Blob !== "undefined" && typeof Blob.prototype.text !== "function") {
  // jsdom stores the constructed parts internally; reconstruct the text from a
  // FileReader, falling back to String() of the parts.
  Blob.prototype.text = function (this: Blob): Promise<string> {
    return new Promise((resolve, reject) => {
      try {
        const reader = new FileReader();
        reader.onload = () => resolve(String(reader.result ?? ""));
        reader.onerror = () => reject(reader.error);
        reader.readAsText(this);
      } catch (e) {
        reject(e);
      }
    });
  };
}
