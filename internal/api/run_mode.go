// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The run-mode contract (0.9 New Run): what a person is starting. A new client
// sends `experience` with the carriers workload, tools, startup, start_folder and
// no_repositories_or_drives; an older client sends today's agent, task,
// task_mode, interactive and interactive_start and is read exactly as before.
//
// Three layers, so a request and a template meet one rule set:
//
//   - runModeShapeRefusal: each carrier that is present is well formed. It reads
//     only the carriers, so it holds after projection and a partial template can
//     omit any of them (runContractShapeChecks).
//   - runModeConflictRefusal: two things the request says contradict each other,
//     or contradict an older field the carriers replaced. Also present-safe, and
//     held to a template as well (validateTemplateContentDecoded).
//   - applyRunMode, at decode (decodeRunRequest, all three doors): a new client's
//     unset choices are refused and never inferred, what this server cannot honour
//     yet is refused with request_field_unavailable (never dropped), and what it
//     can honour is projected once onto the legacy fields, so one engine decides
//     the run. A request that carries none of the fields is untouched.
//
// What each lane lifts, by the path runModeUnavailable names:
//
//	tools[].tool_rules, default_effect   -> A-L3 (per-harness rule enforcement)
//	more than one tool, component tools  -> A-L9, A-L14 (installed tools, startup, image/config)
//	interactive with no included tool    -> A-L9, D-117 (a harness-free environment)
//	start_folder of an attachment        -> A-L4, A-L7, D-112-113 (attachments and the one working directory)
const (
	maxRunModeTools      = 16
	maxStartFolderSubdir = 512
)

// includedToolIDRE is a harness's agent name or a component tool's id: one token,
// so it is safe as a key, a rule-set lookup and a command argument.
var includedToolIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func runModeInvalid(format string, args ...any) *runRefusal {
	return runError(http.StatusBadRequest, reasonRunModeInvalid, fmt.Sprintf(format, args...))
}

func runModeConflict(field, instead string) *runRefusal {
	return runError(http.StatusUnprocessableEntity, reasonRunModeConflict, runModeConflictMsg(field, instead))
}

// runModeCarriersUsed reports whether the request carries the mode carriers that
// replace the legacy fields. start_folder and no_repositories_or_drives belong to
// the repositories tab and sit beside either style.
func runModeCarriersUsed(req createRunRequest) bool {
	return req.Experience != "" || req.Workload != nil || len(req.Tools) > 0 || req.Startup != nil
}

// runModeShapeRefusal checks each carrier that is present. It is idempotent
// under projection: it never reads a legacy field.
func runModeShapeRefusal(req createRunRequest) *runRefusal {
	if req.Experience != "" && !req.Experience.Valid() {
		return runModeInvalid("experience: %q is not one of background, interactive", req.Experience)
	}
	for _, check := range []func(createRunRequest) *runRefusal{workloadShapeRefusal, toolsShapeRefusal, startupShapeRefusal, startFolderShapeRefusal} {
		if refusal := check(req); refusal != nil {
			return refusal
		}
	}
	return nil
}

func runModeText(v string, multiline bool) bool {
	return strings.TrimSpace(v) != "" && utf8.RuneCountInString(v) <= maxRunTaskLen && runFieldCharsAllowed(v, multiline)
}

func workloadShapeRefusal(req createRunRequest) *runRefusal {
	w := req.Workload
	if w == nil {
		return nil
	}
	switch w.Kind {
	case client.WorkloadAgentTask:
		switch {
		case !includedToolIDRE.MatchString(w.Agent):
			return runModeInvalid("workload.agent: name the harness the task runs, such as claude-code")
		case !runModeText(w.Task, true):
			return runModeInvalid("workload.task: write what the agent should do, up to %d characters", maxRunTaskLen)
		case w.Command != "":
			return runModeInvalid("workload.command: an agent task has no command; use kind command to run one")
		}
	case client.WorkloadCommand:
		switch {
		case !runModeText(w.Command, true):
			return runModeInvalid("workload.command: give the shell command to run, up to %d characters", maxRunTaskLen)
		case w.Agent != "" || w.Task != "":
			return runModeInvalid("workload: a command runs no agent; use kind agent_task to run one")
		}
	default:
		return runModeInvalid("workload.kind: %q is not one of agent_task, command", w.Kind)
	}
	return nil
}

func toolsShapeRefusal(req createRunRequest) *runRefusal {
	if len(req.Tools) > maxRunModeTools {
		return runModeInvalid("tools: %d tools exceeds the %d-tool limit", len(req.Tools), maxRunModeTools)
	}
	for i, t := range req.Tools {
		if refusal := toolShapeRefusal(i, t, req.Tools[:i]); refusal != nil {
			return refusal
		}
	}
	return nil
}

func toolShapeRefusal(i int, t client.IncludedTool, before []client.IncludedTool) *runRefusal {
	if !includedToolIDRE.MatchString(t.ID) {
		return runModeInvalid("tools[%d].id: give one token such as claude-code", i)
	}
	if slices.ContainsFunc(before, func(o client.IncludedTool) bool { return o.ID == t.ID }) {
		return runModeInvalid("tools[%d].id: %s is included twice", i, t.ID)
	}
	switch t.Kind {
	case client.IncludedToolHarness:
		if t.Component != nil {
			return runModeInvalid("tools[%d].component: a harness has none", i)
		}
		if t.ModelProvider != "" && !modelProviderIDPattern.MatchString(t.ModelProvider) {
			return runModeInvalid("tools[%d].model_provider: %q is not a model provider id", i, t.ModelProvider)
		}
		return toolRulesShapeRefusal(i, t)
	case client.IncludedToolComponent:
		switch {
		case t.Component == nil:
			return runModeInvalid("tools[%d].component: name the component this tool is", i)
		case t.ModelProvider != "" || len(t.ToolRules) > 0 || t.DefaultEffect != "":
			return runModeInvalid("tools[%d]: only a harness has a model provider and tool rules", i)
		}
		if err := t.Component.Validate(proxy.ValidDomainEntry); err != nil {
			return runModeInvalid("tools[%d].component: %v", i, err)
		}
		return nil
	}
	return runModeInvalid("tools[%d].kind: %q is not one of harness, component", i, t.Kind)
}

func toolRulesShapeRefusal(i int, t client.IncludedTool) *runRefusal {
	if t.DefaultEffect != "" && !types.ValidToolEffect(t.DefaultEffect) {
		return runModeInvalid("tools[%d].default_effect: %q is not one of allow, hold, deny", i, t.DefaultEffect)
	}
	if err := validateToolRules(t.ToolRules); err != nil {
		return runModeInvalid("tools[%d].%v", i, err)
	}
	if slices.ContainsFunc(t.ToolRules, func(r types.ToolRule) bool { return r.Tool == "*" }) {
		return runModeInvalid("tools[%d].tool_rules: the default is default_effect, not a rule named *", i)
	}
	return nil
}

func startupShapeRefusal(req createRunRequest) *runRefusal {
	s := req.Startup
	if s == nil {
		return nil
	}
	switch s.Kind {
	case client.StartupNone:
		if s.Tool != "" || s.Command != "" {
			return runModeInvalid("startup: none starts nothing, so it carries no tool or command")
		}
	case client.StartupHarness:
		if !includedToolIDRE.MatchString(s.Tool) || s.Command != "" {
			return runModeInvalid("startup: a harness startup names one included tool and no command")
		}
	case client.StartupCommand:
		if !runModeText(s.Command, true) || s.Tool != "" {
			return runModeInvalid("startup: a command startup gives one command, up to %d characters, and no tool", maxRunTaskLen)
		}
	default:
		return runModeInvalid("startup.kind: %q is not one of none, harness, command", s.Kind)
	}
	return nil
}

// startFolderSubpathOK is the rule for a path inside an attachment: relative,
// already cleaned, no ".." element, printable. Whether it names an existing
// folder, and that no symlink on the way leaves the mount, is resolved inside the
// sandbox by the lane that applies it.
func startFolderSubpathOK(sub string) bool {
	if sub == "" {
		return true
	}
	return len(sub) <= maxStartFolderSubdir && !strings.HasPrefix(sub, "/") && path.Clean(sub) == sub &&
		!slices.Contains(strings.Split(sub, "/"), "..") && sub != "." && controlCharFree(sub) && !strings.Contains(sub, `\`)
}

func startFolderShapeRefusal(req createRunRequest) *runRefusal {
	f := req.StartFolder
	if f == nil {
		return nil
	}
	switch f.Kind {
	case client.StartFolderImageDefault:
		if f.Attachment != "" || f.Subpath != "" {
			return runError(http.StatusBadRequest, reasonStartFolderInvalid, "start_folder: the image default carries no attachment or subpath")
		}
	case client.StartFolderAttachment:
		if f.Attachment == "" || len(f.Attachment) > maxRunRepoLen || !controlCharFree(f.Attachment) {
			return runError(http.StatusBadRequest, reasonStartFolderInvalid, "start_folder.attachment: name one attachment of this run")
		}
		if !startFolderSubpathOK(f.Subpath) {
			return runError(http.StatusBadRequest, reasonStartFolderInvalid, startFolderShapeMsg(f.Subpath))
		}
	default:
		return runError(http.StatusBadRequest, reasonStartFolderInvalid, fmt.Sprintf("start_folder.kind: %q is not one of image_default, attachment", f.Kind))
	}
	return nil
}

// runAttachesAnything reports whether the request attaches a repository, a
// workspace or a drive, by any of the doors the older fields give.
func runAttachesAnything(req createRunRequest) bool {
	if req.Repo != "" || req.WorkspaceID != nil || len(req.Workspaces) > 0 || (req.Drive != nil && req.Drive.Enabled) {
		return true
	}
	p := req.InlinePolicy
	return p != nil && (len(p.WorkspaceMounts) > 0 || len(p.WorkspaceRepos) > 0)
}

// runModeConflictRefusal refuses what the request says twice in different words.
// Present-safe, and idempotent only before projection: applyRunMode runs it
// first, and a template (which projects nothing) is held to it as written.
func runModeConflictRefusal(req createRunRequest) *runRefusal {
	if runModeCarriersUsed(req) {
		if field, instead := runModeLegacyField(req); field != "" {
			return runModeConflict(field, instead)
		}
		if field, instead := runModeRunWideRules(req); field != "" {
			return runModeConflict(field, instead)
		}
	}
	if req.NoRepositoriesOrDrives && (runAttachesAnything(req) || req.StartFolder != nil && req.StartFolder.Kind == client.StartFolderAttachment) {
		return runError(http.StatusUnprocessableEntity, reasonRunModeConflict, noRepositoriesConflictMsg())
	}
	return experienceConflictRefusal(req)
}

// runModeLegacyField names the first older field the carriers replace that the
// request also sets, with the carrier to use instead.
func runModeLegacyField(req createRunRequest) (field, instead string) {
	switch {
	case req.Agent != "":
		return "agent", "Name the harness in workload.agent or in tools."
	case req.Task != "":
		return "task", "Write the prompt in workload.task, or the command in workload.command or startup.command."
	case req.TaskMode != "":
		return "task_mode", "Use workload.kind."
	case req.Interactive:
		return "interactive", "Use experience."
	case req.InteractiveStart != "":
		return "interactive_start", "Use startup."
	case req.SeedAutoTools:
		return "seed_auto_tools", "Approval belongs to each tool: use tools[].default_effect and tools[].tool_rules."
	case req.ToolApprovals != "":
		return "tool_approvals", "Approval belongs to each tool: use tools[].default_effect and tools[].tool_rules."
	case req.ModelProvider != "":
		return "model_provider", "Use tools[].model_provider."
	}
	return "", ""
}

// runModeRunWideRules names a run-wide rule block that only an older client may
// send: tool rules belong to each included harness, and push rules to the Git
// entry that brings the repository.
func runModeRunWideRules(req createRunRequest) (field, instead string) {
	const perTool = "Tool rules belong to each tool: use tools[].tool_rules and default_effect."
	switch {
	case req.InlinePolicy != nil && len(req.InlinePolicy.ToolRules) > 0:
		return "inline_policy.tool_rules", perTool
	case req.InlinePolicy != nil && req.InlinePolicy.PushRules != nil:
		return "inline_policy.push_rules", "Push rules belong to the Git entry that brings the repository: use overrides.push_rules, one set per provider and organisation."
	case req.Overrides != nil && req.Overrides.Agent != nil && len(req.Overrides.Agent.ToolRules) > 0:
		return "overrides.agent.tool_rules", perTool
	}
	return "", ""
}

// experienceConflictRefusal holds the carriers to the mode they sit in.
func experienceConflictRefusal(req createRunRequest) *runRefusal {
	w := req.Workload
	switch req.Experience {
	case client.ExperienceBackground:
		switch {
		case req.Startup != nil:
			return runModeConflict("startup", "A background task starts its workload; startup is for an interactive environment.")
		case req.InlinePolicy != nil && len(req.InlinePolicy.UIApps) > 0:
			return runModeConflict("inline_policy.ui_apps", "A background task has no web application gateway.")
		case w != nil && w.Kind == client.WorkloadCommand && len(req.Tools) > 0:
			return runModeConflict("tools", "A command runs no harness.")
		case w != nil && w.Kind == client.WorkloadAgentTask && slices.ContainsFunc(req.Tools, func(t client.IncludedTool) bool { return t.ID != w.Agent }):
			return runModeConflict("tools", "A background agent task includes only the harness it runs, which is workload.agent.")
		}
	case client.ExperienceInteractive:
		if w != nil {
			return runModeConflict("workload", "An interactive environment has no workload; use startup to start something.")
		}
	}
	return nil
}

// runModeUnavailableRefusal refuses a valid carrier this server cannot honour
// yet, by name. Present-safe, so a template is held to it too
// (checkAvailable mirrors it through unappliedFieldsRefusal).
func runModeUnavailableRefusal(req createRunRequest) *runRefusal {
	if _, what := runModeUnavailable(req); what != "" {
		return runError(http.StatusUnprocessableEntity, reasonRequestFieldUnavailable, runModeUnavailableMsg(what))
	}
	return nil
}

// runModeUnavailable names the first valid carrier this server cannot honour yet:
// its request path, and what about it is unavailable.
func runModeUnavailable(req createRunRequest) (path, what string) {
	if len(req.Tools) > 1 {
		return "tools", "tools: more than one included tool"
	}
	for i, t := range req.Tools {
		p := fmt.Sprintf("tools[%d]", i)
		switch {
		case t.Kind == client.IncludedToolComponent:
			return p, p + ": a component tool"
		case len(t.ToolRules) > 0 || t.DefaultEffect != "":
			return p, p + ": per-tool tool rules"
		}
	}
	if f := req.StartFolder; f != nil && f.Kind == client.StartFolderAttachment {
		return "start_folder", "start_folder: a folder inside an attachment"
	}
	return "", ""
}

// startFolderAttachmentKnown reports whether the attachment is one the request
// carries, and the mount target it names when it names one.
func startFolderAttachmentKnown(req createRunRequest, attachment string) (known bool, target string) {
	if attachment == client.StartFolderDrive {
		return req.Drive != nil && req.Drive.Enabled, runner.DriveTarget
	}
	if req.WorkspaceID != nil && req.WorkspaceID.String() == attachment {
		return true, ""
	}
	for _, sel := range req.Workspaces {
		if sel.WorkspaceID == attachment {
			return true, sel.Target
		}
	}
	return false, ""
}

// startFolderRefusal holds a start folder to the attachments of this run and to
// the mount it names: the resolved folder must stay inside the admitted sandbox
// filesystem under the same rule an authored mount target meets.
func startFolderRefusal(req createRunRequest) *runRefusal {
	f := req.StartFolder
	if f == nil || f.Kind != client.StartFolderAttachment {
		return nil
	}
	known, target := startFolderAttachmentKnown(req, f.Attachment)
	if !known {
		return runError(http.StatusBadRequest, reasonStartFolderInvalid, startFolderAttachmentMsg(f.Attachment))
	}
	if target != "" && f.Attachment != client.StartFolderDrive {
		if err := workspaceTargetShape(path.Join(target, f.Subpath)); err != nil {
			return runError(http.StatusBadRequest, reasonStartFolderInvalid, startFolderShapeMsg(f.Subpath))
		}
	}
	return nil
}

// runModeRequiredRefusal is what only the run door knows: a new client's mode and
// choices are present. A partial template may leave any of them out.
func runModeRequiredRefusal(req createRunRequest) *runRefusal {
	switch {
	case req.Experience == "":
		return runError(http.StatusBadRequest, reasonRunModeRequired, runModeRequiredMsg())
	case req.Experience == client.ExperienceBackground && req.Workload == nil:
		return runError(http.StatusBadRequest, reasonRunModeRequired, runWorkloadRequiredMsg())
	case req.Startup != nil && req.Startup.Kind == client.StartupHarness &&
		!slices.ContainsFunc(req.Tools, func(t client.IncludedTool) bool {
			return t.ID == req.Startup.Tool && t.Kind == client.IncludedToolHarness
		}):
		return runError(http.StatusUnprocessableEntity, reasonRunModeConflict, startupToolUnknownMsg(req.Startup.Tool))
	}
	return nil
}

// applyRunMode is the run door's half of the contract, called once at decode by
// all three doors. It refuses, then projects the carriers it can honour onto the
// legacy fields; a request with none of the mode fields is returned untouched.
func applyRunMode(req *createRunRequest) *runRefusal {
	if !req.UsesRunMode() {
		return nil
	}
	for _, check := range []func(createRunRequest) *runRefusal{
		runModeShapeRefusal, runModeConflictRefusal, runModeRequiredRefusal, startFolderRefusal, runModeUnavailableRefusal, interactiveToolRequiredRefusal,
	} {
		if refusal := check(*req); refusal != nil {
			return refusal
		}
	}
	projectRunMode(req)
	return nil
}

// interactiveToolRequiredRefusal: an interactive environment with no included
// harness has no image to select, so it waits for the lane that picks one.
func interactiveToolRequiredRefusal(req createRunRequest) *runRefusal {
	if req.Experience == client.ExperienceInteractive && len(req.Tools) == 0 {
		return runError(http.StatusUnprocessableEntity, reasonRequestFieldUnavailable, runModeUnavailableMsg("tools: an interactive environment with no included tool"))
	}
	return nil
}

// projectRunMode writes the honourable carriers onto the legacy fields the one
// engine reads. It never chooses: every value is one the person sent.
func projectRunMode(req *createRunRequest) {
	switch req.Experience {
	case client.ExperienceBackground:
		w := req.Workload
		if w.Kind == client.WorkloadCommand {
			req.TaskMode, req.Task = "exec", w.Command
			return
		}
		req.Agent, req.Task = w.Agent, w.Task
		if len(req.Tools) == 1 {
			req.ModelProvider = req.Tools[0].ModelProvider
		}
	case client.ExperienceInteractive:
		req.Interactive = true
		req.Agent, req.ModelProvider = req.Tools[0].ID, req.Tools[0].ModelProvider
		if s := req.Startup; s != nil {
			switch s.Kind {
			case client.StartupHarness:
				req.InteractiveStart = "agent"
			case client.StartupCommand:
				req.InteractiveStart, req.Task = "shell", s.Command
			}
		}
	}
}
