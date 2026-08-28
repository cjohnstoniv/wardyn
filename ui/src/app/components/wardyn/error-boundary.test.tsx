/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The case this file exists for: a console deploy replaces index.html and its
// hashed chunks, and every already-open tab keeps the OLD index.html in memory.
// The next navigation to a not-yet-loaded lazy route then throws "Failed to
// fetch dynamically imported module: .../assets/demos-step-<hash>.js", and the
// generic "Try again" was a button that could never work — it re-runs the same
// import against the same missing file.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ErrorBoundary } from "./error-boundary";

function Boom({ message }: { message: string }): React.ReactElement {
  throw new Error(message);
}

const CHUNK_MSG =
  "Failed to fetch dynamically imported module: http://localhost:8080/assets/demos-step-CDS7MILr.js";

let reload: ReturnType<typeof vi.fn>;
let consoleErr: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
  sessionStorage.clear();
  reload = vi.fn();
  Object.defineProperty(window, "location", {
    value: { ...window.location, reload },
    writable: true,
  });
  // The boundary logs every caught error on purpose; keep the suite output clean.
  consoleErr = vi.spyOn(console, "error").mockImplementation(() => {});
});

afterEach(() => consoleErr.mockRestore());

describe("ErrorBoundary — stale-chunk recovery", () => {
  it("auto-reloads once on a dynamic-import failure", () => {
    render(
      <ErrorBoundary region="/setup">
        <Boom message={CHUNK_MSG} />
      </ErrorBoundary>,
    );
    expect(reload).toHaveBeenCalledTimes(1);
  });

  // The guard that keeps a genuinely broken deploy from pinning the browser in
  // a refresh loop: the second failure inside the cooldown must NOT reload.
  it("does not reload twice inside the cooldown — a broken deploy must not loop", () => {
    render(
      <ErrorBoundary>
        <Boom message={CHUNK_MSG} />
      </ErrorBoundary>,
    );
    render(
      <ErrorBoundary>
        <Boom message={CHUNK_MSG} />
      </ErrorBoundary>,
    );
    expect(reload).toHaveBeenCalledTimes(1);
  });

  it("names the real cause and offers Reload, never the useless Try again", async () => {
    sessionStorage.setItem("wardyn-chunk-reload-at", String(Date.now())); // already reloaded
    render(
      <ErrorBoundary region="/setup">
        <Boom message={CHUNK_MSG} />
      </ErrorBoundary>,
    );
    expect(reload).not.toHaveBeenCalled();
    expect(screen.getByText(/Wardyn was updated while this tab was open/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /try again/i })).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: /^reload$/i }));
    expect(reload).toHaveBeenCalledTimes(1);
  });

  it("reassures that configuration is untouched — this is a stale tab, not data loss", () => {
    sessionStorage.setItem("wardyn-chunk-reload-at", String(Date.now()));
    render(
      <ErrorBoundary>
        <Boom message={CHUNK_MSG} />
      </ErrorBoundary>,
    );
    expect(screen.getByText(/nothing you have configured is affected/i)).toBeInTheDocument();
  });
});

describe("ErrorBoundary — ordinary render errors are untouched", () => {
  it("never reloads, and keeps Try again with the real message", async () => {
    const reset = vi.fn();
    render(
      <ErrorBoundary region="Runs">
        <Boom message="Cannot read properties of undefined (reading 'backends')" />
      </ErrorBoundary>,
    );
    expect(reload).not.toHaveBeenCalled();
    expect(screen.getByText(/Something went wrong rendering Runs/)).toBeInTheDocument();
    expect(screen.getByText(/reading 'backends'/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /try again/i })).toBeInTheDocument();
    expect(reset).not.toHaveBeenCalled();
  });

  it("a custom fallback still wins for both kinds", () => {
    render(
      <ErrorBoundary fallback={(e) => <p>custom: {e.message}</p>}>
        <Boom message={CHUNK_MSG} />
      </ErrorBoundary>,
    );
    expect(screen.getByText(/^custom:/)).toBeInTheDocument();
  });
});

describe("ErrorBoundary — resetKey", () => {
  it("clears a caught error and mounts the child fresh once resetKey changes", () => {
    const { rerender } = render(
      <ErrorBoundary region="Runs" resetKey="run-1">
        <Boom message="boom" />
      </ErrorBoundary>,
    );
    expect(screen.getByText(/Something went wrong rendering Runs/)).toBeInTheDocument();

    rerender(
      <ErrorBoundary region="Runs" resetKey="run-2">
        <p>fresh content</p>
      </ErrorBoundary>,
    );
    expect(screen.queryByText(/Something went wrong/)).not.toBeInTheDocument();
    expect(screen.getByText("fresh content")).toBeInTheDocument();
  });

  it("leaves a caught error in place while resetKey stays the same", () => {
    const { rerender } = render(
      <ErrorBoundary region="Runs" resetKey="run-1">
        <Boom message="boom" />
      </ErrorBoundary>,
    );
    expect(screen.getByText(/Something went wrong rendering Runs/)).toBeInTheDocument();

    rerender(
      <ErrorBoundary region="Runs" resetKey="run-1">
        <p>fresh content</p>
      </ErrorBoundary>,
    );
    expect(screen.getByText(/Something went wrong rendering Runs/)).toBeInTheDocument();
    expect(screen.queryByText("fresh content")).not.toBeInTheDocument();
  });
});
