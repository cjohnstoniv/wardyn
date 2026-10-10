// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The 0.9 run-mode contract: what a person is starting. A new client sends
// `experience` plus the carriers below; an older client keeps sending today's
// `agent`, `task`, `task_mode`, `interactive` and `interactive_start`, and the
// server reads those as before. A request never mixes the two (the server
// refuses it with run_mode_conflict), and a new client's unset mode is never
// inferred (run_mode_required).
//
// The legacy fields stay the engine's input in this release: the server
// projects the carriers onto them once, at decode, so one policy engine decides
// the run. What the engine cannot honour yet is refused with
// request_field_unavailable, never dropped (internal/api/run_mode.go).

// Experience is the canonical run mode.
type Experience = types.RunExperience

// The two experiences. Background is a task or a command with no interactive
// surface; Interactive is general sandboxed work through whatever surfaces the
// placement supports.
const (
	ExperienceBackground  = types.ExperienceBackground
	ExperienceInteractive = types.ExperienceInteractive
)

// WorkloadKind says what a background run executes.
type WorkloadKind string

const (
	// WorkloadAgentTask runs one harness on a task.
	WorkloadAgentTask WorkloadKind = "agent_task"
	// WorkloadCommand runs a plain shell command in the governed sandbox: no
	// harness and no model credential.
	WorkloadCommand WorkloadKind = "command"
)

// RunWorkload is what a Background run does. Exactly one of the two shapes:
// agent_task carries Agent and Task, command carries Command. Autonomous
// execution follows from the mode, so there is no separate autonomy choice.
type RunWorkload struct {
	Kind WorkloadKind `json:"kind"`
	// Agent is the harness an agent task runs (claude-code, codex-cli, a custom
	// agent): the id of the one included tool it runs.
	Agent string `json:"agent,omitempty"`
	// Task is the harness's prompt.
	Task string `json:"task,omitempty"`
	// Command is the shell command a command workload runs, as task_mode "exec"
	// runs a task today.
	Command string `json:"command,omitempty"`
}

// IncludedToolKind says what an included tool is.
type IncludedToolKind string

const (
	// IncludedToolHarness is an agent harness; its ID is its agent name.
	IncludedToolHarness IncludedToolKind = "harness"
	// IncludedToolComponent is a component with a bounded configuration.
	IncludedToolComponent IncludedToolKind = "component"
)

// IncludedTool is one tool instance the run includes. Including a tool is not
// starting it and grants it no credential: each tool carries its own model
// provider or configuration, its own tool rules and its own default effect, so
// independent tools never share one selection.
type IncludedTool struct {
	// ID names the instance, once per request. For a harness it is the agent
	// name; it is the key Startup.Tool, the approval broker and the per-harness
	// rules all use.
	ID   string           `json:"id"`
	Kind IncludedToolKind `json:"kind"`
	// ModelProvider is a harness's model provider (GET /model-providers, by id).
	// Empty takes the same defaults a run's model_provider does.
	ModelProvider string `json:"model_provider,omitempty"`
	// Component is a component tool's reference and its bounded configuration.
	Component *ComponentRef `json:"component,omitempty"`
	// ToolRules are this harness's tool rules, in the language of a policy's
	// tool_rules, and DefaultEffect its own "*" effect. The approval broker
	// evaluates the rules of the harness that raised the call.
	ToolRules     []types.ToolRule `json:"tool_rules,omitempty"`
	DefaultEffect types.ToolEffect `json:"default_effect,omitempty"`
}

// StartupKind is the one discriminator of what starts automatically.
type StartupKind string

const (
	StartupNone    StartupKind = "none"
	StartupHarness StartupKind = "harness"
	StartupCommand StartupKind = "command"
)

// RunStartup is what an Interactive run starts on its own: nothing, one included
// harness, or one command. It is one choice, never two competing launches, and
// removing the startup harness clears it; the server never substitutes another.
// Absent is the same as none.
type RunStartup struct {
	Kind StartupKind `json:"kind"`
	// Tool is the included tool a harness startup launches.
	Tool string `json:"tool,omitempty"`
	// Command is the startup command a command startup runs once at boot.
	Command string `json:"command,omitempty"`
}

// StartFolderKind says where the run's one working directory comes from.
type StartFolderKind string

const (
	// StartFolderImageDefault is the image's own working directory.
	StartFolderImageDefault StartFolderKind = "image_default"
	// StartFolderAttachment is a folder inside one attached workspace or drive.
	StartFolderAttachment StartFolderKind = "attachment"
)

// StartFolder is the run's one starting folder: the initial working directory of
// every start (a background command or task, an interactive terminal, a
// harness). Every other mount stays available without becoming a second one.
type StartFolder struct {
	Kind StartFolderKind `json:"kind"`
	// Attachment is the workspace id of one of the request's attachments, or
	// StartFolderDrive for the person's drive.
	Attachment string `json:"attachment,omitempty"`
	// Subpath is a relative path inside the attachment's mount: no "..", no
	// leading "/". Empty is the mount's root.
	Subpath string `json:"subpath,omitempty"`
}

// StartFolderDrive names the person's drive as a start folder attachment.
const StartFolderDrive = "drive"

// UsesRunMode reports whether the request carries any of the run-mode contract's
// fields, which makes it a new client's: its mode must then be explicit.
func (r CreateRunRequest) UsesRunMode() bool {
	return r.Experience != "" || r.Workload != nil || len(r.Tools) > 0 || r.Startup != nil || r.StartFolder != nil || r.NoRepositoriesOrDrives
}

// EffectiveExperience is the documented compatibility mapping from an older
// client's fields: a request is Interactive when it says so or has no task (the
// server coerces such a request to idle and attachable), and Background
// otherwise. A request that carries `experience` is its own answer. The mapping
// is for reading old requests; the server stores an experience only for a
// request that carried one, so an older client's run keeps every door's behaviour.
func (r CreateRunRequest) EffectiveExperience() Experience {
	switch {
	case r.Experience != "":
		return r.Experience
	case r.Interactive || strings.TrimSpace(r.Task) == "":
		return ExperienceInteractive
	}
	return ExperienceBackground
}
