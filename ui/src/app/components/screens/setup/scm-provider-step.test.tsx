/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// SCM-SEAM-5: the hosted-PAT rung's note claimed saving a credential "Also
// registers the hostname" — true only once Done is clicked (Done calls
// addHost; a secret save does not). Closing the dialog (X/Esc -> back to
// search) right after a successful secret save left that promise silently
// unfulfilled — the credential is stored, the host never is, and the
// integrations list renders it under a guessed hostname instead. The note is
// now pinned to the row that actually performs the write.
import type { ComponentProps } from "react";
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { AddProviderPanel2 } from "./scm-provider-step";

function renderPanel2(overrides: Partial<ComponentProps<typeof AddProviderPanel2>> = {}) {
  return render(
    <AddProviderPanel2
      kind="generic"
      host="ghes.example.com"
      title="GHES"
      showHostedNote
      saving={false}
      siteConfigLoaded
      onBack={vi.fn()}
      onClose={vi.fn()}
      onDone={vi.fn()}
      onOpenSecret={vi.fn()}
      onRecheck={vi.fn()}
      {...overrides}
    />,
  );
}

describe("AddProviderPanel2 — hosted-registration note (SCM-SEAM-5)", () => {
  it("names Done, not the PAT button, as what actually registers the hostname", () => {
    renderPanel2();
    const note = screen.getByText(/registers the hostname/i);
    expect(note.textContent).toMatch(/done/i);
    // The old copy sat right under Add PAT, claiming the save alone did it.
    expect(screen.queryByText("Also registers the hostname so runs can reach it.")).not.toBeInTheDocument();
  });

  it("stays silent for a fixed/well-known host — nothing to register", () => {
    renderPanel2({ showHostedNote: false });
    expect(screen.queryByText(/registers the hostname/i)).not.toBeInTheDocument();
  });

  it("stays silent on the github/ado ladders too — showHostedNote is a generic-only concern", () => {
    renderPanel2({ kind: "github", host: "github.com", title: "GitHub" });
    expect(screen.queryByText(/registers the hostname/i)).not.toBeInTheDocument();
  });
});
