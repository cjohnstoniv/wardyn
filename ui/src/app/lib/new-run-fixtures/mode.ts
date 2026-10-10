/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run-mode frames of the v16 to v19 mock (/m-nr/v16/mode-unset, …): the form's
// draft of what the person is starting and the answer the server gives it. The
// refusals are the server's own sentences, shared through run-mode-refusals.ts;
// a frame the server cannot honour yet carries the request_field_unavailable it
// answers today, naming the lane that lifts it, not a promise the mock makes.
import { NEW_RUN_REASON } from "../new-run-refusals";
import { RUN_MODE_REASON, RUN_MODE_REFUSAL } from "../run-mode-refusals";
import type { RunModeDraft } from "../run-contract-draft";
import type { IncludedTool } from "../types/new-run-contract";
import { inTab, noContract, preview, type FixtureBody, type NewRunFixture } from "./base";

const claude: IncludedTool = { id: "claude-code", kind: "harness", model_provider: "bedrock-team" };
const codex: IncludedTool = { id: "codex-cli", kind: "harness", model_provider: "openai-team" };

const PAYMENTS = { id: "5aa1f0c2-7a54-4c2e-9d1e-0c4f1b2a7e10", name: "payments", target: "/home/agent/work" };

const withMode = (mode: RunModeDraft) => ({ ...noContract(), mode });
const unavailable = (what: string) => ({ status: 422, reason: NEW_RUN_REASON.REQUEST_FIELD_UNAVAILABLE, text: RUN_MODE_REFUSAL.FIELD_UNAVAILABLE(what) });

const MODE_FRAMES: FixtureBody[] = [
  {
    route: "v16/mode-unset",
    note: "A fresh draft: neither mode is selected and the request carries no run-mode field. The server never infers one.",
    preview: preview(),
    contract: withMode({ tools: [] }),
  },
  {
    route: "v19/mode-attempted",
    note: "Continue or Launch was attempted with no mode: the field's own error, and the server's sentence if the request is sent anyway.",
    preview: preview(),
    contract: withMode({ tools: [], noRepositoriesOrDrives: true }),
    refusal: { status: 400, reason: RUN_MODE_REASON.REQUIRED, text: RUN_MODE_REFUSAL.RUN_MODE_REQUIRED() },
  },
  {
    route: "v16/background-agent",
    note: "Background task, agent task: one harness runs the task, with its own model provider. No startup, no terminal, SSH or web access.",
    preview: preview(),
    contract: withMode({
      experience: "background",
      workload: { kind: "agent_task", agent: "claude-code", task: "Fix the failing payment tests." },
      tools: [claude],
      noRepositoriesOrDrives: true,
    }),
  },
  {
    route: "v16/background-command",
    note: "Background task, command: runs a plain shell command in the governed sandbox. No harness and no model credential.",
    preview: preview(),
    contract: withMode({ experience: "background", workload: { kind: "command", command: "make test" }, tools: [], noRepositoriesOrDrives: true }),
  },
  {
    route: "v16/interactive-empty",
    note: "Interactive environment with no included tool. Valid in the product; this server needs a harness to select an image, so it refuses and says so.",
    preview: preview(),
    contract: withMode({ experience: "interactive", tools: [], noRepositoriesOrDrives: true }),
    refusal: unavailable("tools: an interactive environment with no included tool"),
  },
  {
    route: "v16/interactive-multiple",
    note: "Several included harnesses, at most one starts. Each carries its own provider. Refused until installed tools land (A-L9).",
    preview: preview(),
    contract: withMode({
      experience: "interactive",
      tools: [claude, codex],
      startup: { kind: "harness", tool: "claude-code" },
      noRepositoriesOrDrives: true,
    }),
    refusal: unavailable("tools: more than one included tool"),
  },
  {
    route: "v19/harness-rules",
    note: "Tool rules and a default effect per harness, never run-wide. The server refuses them until per-harness enforcement lands (A-L3).",
    preview: preview(),
    contract: withMode({
      experience: "interactive",
      tools: [
        { ...claude, tool_rules: [{ tool: "Bash", effect: "hold" }], default_effect: "allow" },
        { ...codex, tool_rules: [{ tool: "Write", effect: "deny" }], default_effect: "hold" },
      ],
      noRepositoriesOrDrives: true,
    }),
    refusal: unavailable("tools: more than one included tool"),
  },
  {
    route: "v16/starting-folder",
    note: "Exactly one starting folder, inside one attachment, with the other mounts still available. Refused until attachments apply it (A-L4, A-L7).",
    preview: preview(),
    workspaces: [PAYMENTS],
    contract: withMode({
      experience: "interactive",
      tools: [claude],
      startFolder: { kind: "attachment", attachment: PAYMENTS.id, subpath: "services/api" },
    }),
    refusal: unavailable("start_folder: a folder inside an attachment"),
  },
  {
    route: "v16/starting-folder-missing",
    note: "The attachment that held the starting folder was removed: the folder must be repaired, never silently switched.",
    preview: preview(),
    workspaces: [],
    contract: withMode({
      experience: "interactive",
      tools: [claude],
      startFolder: { kind: "attachment", attachment: PAYMENTS.id, subpath: "services/api" },
    }),
    refusal: { status: 400, reason: RUN_MODE_REASON.START_FOLDER_INVALID, text: RUN_MODE_REFUSAL.START_FOLDER_ATTACHMENT(PAYMENTS.id) },
  },
  {
    route: "v17/repos-none-chosen",
    note: "The explicit choice of no repositories or drives: complete, with the warning that nothing in the sandbox is kept unless it is pushed or copied out.",
    preview: preview(),
    contract: withMode({ experience: "interactive", tools: [claude], noRepositoriesOrDrives: true }),
  },
];

const frames = (...routes: string[]) => MODE_FRAMES.filter((f) => routes.includes(f.route));

/** Each frame on the tab that owns its control: the mode on Runner, the workload, tools and startup on Tools & Image, the folder on Repositories & Drives. */
export const MODE_FIXTURES: NewRunFixture[] = [
  ...inTab("runner", frames("v16/mode-unset", "v19/mode-attempted")),
  ...inTab("tools_image", frames("v16/background-agent", "v16/background-command", "v16/interactive-empty", "v16/interactive-multiple", "v19/harness-rules")),
  ...inTab("repositories_drives", frames("v16/starting-folder", "v16/starting-folder-missing", "v17/repos-none-chosen")),
];
