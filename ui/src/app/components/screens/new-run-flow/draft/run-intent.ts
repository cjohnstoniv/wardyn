/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The one New Run draft. Guided, the YAML/JSON editor, templates, Run again
// and Review all read and write it; intentToWire is its only way to the wire.

import type { RunWireInput } from "../../../../lib/api/runs";
import type { DriveAttachment } from "../../../../lib/types/new-run-contract";
import type { RunPolicySpec } from "../../../../lib/types/policy";
import type { CreateRunInput } from "../../../../lib/types/run-create";
import type { RunShape } from "../../../../lib/types/runs";

/** The create-request fields with the 0.9 carriers (the request half of RunWireInput). */
type CreateRunRequestFields = Extract<RunWireInput, CreateRunInput>;

/**
 * The `intent` of a wardyn/v1 `kind: Run` document. Presence-preserving: an
 * absent key means "the server decides"; explicit false, 0 and [] are choices
 * and are sent. Title and description belong to the Name dialog, the contract
 * marker to the serializer.
 */
export type RunIntent = Partial<Omit<CreateRunRequestFields, "title" | "description" | "mode_contract" | "inline_policy">> & {
  shape?: RunShape;
  /** Registry fields a document may carry that the older request type does not declare. */
  devcontainer_repo?: string;
  devcontainer_ref?: string;
  /** Drives from GET /me/drives by id (pkg/client.CreateRunRequest.Drives); never sent beside `drive`. */
  drives?: DriveAttachment[];
  /** Overlay onto the caller's default policy. Never a whole policy. */
  inline_policy?: Partial<RunPolicySpec>;
};

export type NewRunOrigin =
  | { kind: "scratch" }
  | { kind: "template"; id: string; revision: number; name: string; templateKind: "template" | "policy" }
  | { kind: "again"; runId: string; title: string }
  | { kind: "import" }
  | { kind: "composer" }
  /**
   * runId is absent while the older clone door's prefill does not carry it.
   * policyDropped: the source run's inline policy was not kept, so the clone
   * falls back to the default policy; Review states it.
   */
  | { kind: "clone"; runId?: string; policyDropped?: boolean };

/** UI-only facts with no wire field. Never serialised, never saved, never sent. */
export interface GuidedFacts {
  startFrom: "scratch" | "template" | "again";
  /** The main drive size as typed; committed to the intent on blur or Continue. */
  mainDriveText: string;
  /** The pool's default the field was filled with, while the person has not changed it. */
  mainDriveFill?: string;
  mainDriveClampNote?: string;
  /** Free-text ref per `${workspaceId}/${repo}` while the branch list is unavailable. */
  refText: Record<string, string>;
}

export interface DocDiagnostic {
  path: string;
  line?: number;
  column?: number;
  reason?: string;
  text: string;
}

export interface DocBuffer {
  format: "yaml" | "json";
  text: string;
  diagnostics: DocDiagnostic[];
  /** The server door accepted exactly this text. */
  validated: boolean;
}

export interface NewRunDraft {
  intent: RunIntent;
  /** What the origin loaded ({} for scratch); "Only what I changed" diffs against it. */
  base: RunIntent;
  guided: GuidedFacts;
  /** Present only while the editor text differs from the serialised intent. */
  doc?: DocBuffer;
  origin: NewRunOrigin;
  /** The principal the draft belongs to. */
  owner: string;
  /** +1 on every intent change. */
  revision: number;
  /** The Name dialog's fields: never in the intent, never saved. */
  launch: { title: string; description: string };
}

export const emptyGuided = (startFrom: GuidedFacts["startFrom"] = "scratch"): GuidedFacts => ({
  startFrom, mainDriveText: "", refText: {},
});

export function initialDraft(owner: string): NewRunDraft {
  return {
    intent: {}, base: {}, guided: emptyGuided(), origin: { kind: "scratch" }, owner, revision: 0,
    launch: { title: "", description: "" },
  };
}
