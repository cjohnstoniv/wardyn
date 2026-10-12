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

	"github.com/cjohnstoniv/wardyn/internal/agentpolicy"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
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
//     the run. A request that carries none of the fields, and not the contract
//     marker (mode_contract), is an older client's and is untouched.
//
// What each lane lifts, by the path runModeUnavailable names:
//
//	tools[].tool_rules in an interactive run -> no lane for a harness but Claude Code, whose managed settings carry them
//	component tools                      -> A-L14 (component tools)
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
// the repositories tab, and they too are a new client's: UsesRunMode (and so
// runModeRequiredRefusal) refuses either without an experience.
func runModeCarriersUsed(req createRunRequest) bool {
	return req.Experience != "" || req.Workload != nil || len(req.Tools) > 0 || req.Startup != nil
}

// runModeFieldSet names the first run-mode field a request carries, or "".
func runModeFieldSet(req createRunRequest) string {
	switch {
	case req.ModeContract != "":
		return "mode_contract"
	case req.Experience != "":
		return "experience"
	case req.Workload != nil:
		return "workload"
	case len(req.Tools) > 0:
		return "tools"
	case req.Startup != nil:
		return "startup"
	case req.StartFolder != nil:
		return "start_folder"
	case req.NoRepositoriesOrDrives:
		return "no_repositories_or_drives"
	case req.Shape != "":
		return "shape"
	}
	return ""
}

// presetRunModeRefusal refuses a preset that carries a run-mode field, at write
// and again at launch. A preset is the older shape (launch fields plus a stored
// request); run templates carry the new choices, so a preset that names them is
// refused out loud instead of being stored and then failing at every launch.
func presetRunModeRefusal(req createRunRequest) *runRefusal {
	field := runModeFieldSet(req)
	if field == "" {
		return nil
	}
	return runError(http.StatusUnprocessableEntity, reasonRequestFieldUnavailable,
		fmt.Sprintf("request.%s cannot be stored in a preset: a preset carries the older run fields until run templates replace it.", field))
}

// runModeShapeRefusal checks each carrier that is present. It is idempotent
// under projection: it never reads a legacy field.
func runModeShapeRefusal(req createRunRequest) *runRefusal {
	if req.ModeContract != "" && req.ModeContract != client.RunModeContractV09 {
		return runModeInvalid("mode_contract: %q is not a run-mode contract version this server reads (%s)", req.ModeContract, client.RunModeContractV09)
	}
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
	for j, r := range t.ToolRules {
		if !exactToolNameRE.MatchString(r.Tool) {
			return runModeInvalid("tools[%d].tool_rules[%d]: %q is not one tool's exact name. Name a tool such as Bash, WebFetch or mcp__server__tool; patterns aren't supported.", i, j, r.Tool)
		}
	}
	// Whether an allow is within the policy needs the resolved policy, so the
	// fold answers it (toolRulesWithinPolicyRefusal), not this shape layer.
	return harnessToolRulesRefusal(t)
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
		if s.Prompt != "" && !runModeText(s.Prompt, true) {
			return runModeInvalid("startup.prompt: write the starting prompt, up to %d characters", maxRunTaskLen)
		}
		return nil
	case client.StartupCommand:
		if !runModeText(s.Command, true) || s.Tool != "" {
			return runModeInvalid("startup: a command startup gives one command, up to %d characters, and no tool", maxRunTaskLen)
		}
	default:
		return runModeInvalid("startup.kind: %q is not one of none, harness, command", s.Kind)
	}
	if s.Prompt != "" {
		return runError(http.StatusBadRequest, reasonRunModeInvalid, startupPromptShapeMsg())
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
		if f.Attachment != "" || f.Repo != "" || f.DriveID != "" || f.Subpath != "" {
			return runError(http.StatusBadRequest, reasonStartFolderInvalid, "start_folder: the image default carries no attachment or subpath")
		}
	case client.StartFolderAttachment:
		if (f.Attachment == "") == (f.DriveID == "") || len(f.Attachment) > maxRunRepoLen || !controlCharFree(f.Attachment) ||
			len(f.DriveID) > maxRunRepoLen || !controlCharFree(f.DriveID) {
			return runError(http.StatusBadRequest, reasonStartFolderInvalid, "start_folder.attachment: name one attachment of this run")
		}
		if f.Repo != "" && (f.Attachment == "" || f.Attachment == client.StartFolderDrive || len(f.Repo) > maxRunRepoLen || !controlCharFree(f.Repo)) {
			return runError(http.StatusBadRequest, reasonStartFolderInvalid, "start_folder.repo: name a repository of the workspace attachment")
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
	if req.Repo != "" || req.WorkspaceID != nil || len(req.Workspaces) > 0 || (req.Drive != nil && req.Drive.Enabled) || len(req.Drives) > 0 {
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
	pol, at := requestPolicyRules(req)
	switch {
	case pol != nil && len(pol.ToolRules) > 0:
		return at + ".tool_rules", perTool
	case pol != nil && pol.PushRules != nil:
		return at + ".push_rules", "Push rules belong to the Git entry that brings the repository: use overrides.push_rules, one set per provider and organisation."
	case req.Overrides != nil && req.Overrides.Agent != nil && len(req.Overrides.Agent.ToolRules) > 0:
		return "overrides.agent.tool_rules", perTool
	}
	return "", ""
}

// experienceConflictRefusal holds the carriers to the mode they sit in.
func experienceConflictRefusal(req createRunRequest) *runRefusal {
	w := req.Workload
	pol, at := requestPolicyRules(req)
	switch req.Experience {
	case client.ExperienceBackground:
		switch {
		case req.Startup != nil:
			return runModeConflict("startup", "A background task starts its workload; startup is for an interactive environment.")
		case pol != nil && pol.AutoStopAfterSec != 0:
			return runError(http.StatusUnprocessableEntity, reasonRunModeConflict, idleStopBackgroundMsg())
		case pol != nil && len(pol.UIApps) > 0:
			return runModeConflict(at+".ui_apps", "A background task has no web application gateway.")
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
	for i, t := range req.Tools {
		if t.Kind == client.IncludedToolComponent {
			p := fmt.Sprintf("tools[%d]", i)
			return p, p + ": a component tool"
		}
	}
	return "", ""
}

// startFolderAttachmentKnown reports whether the attachment is one the request
// carries: a workspace by id, or the person's drive.
func startFolderAttachmentKnown(req createRunRequest, attachment string) bool {
	if attachment == client.StartFolderDrive {
		return req.Drive != nil && req.Drive.Enabled
	}
	if req.WorkspaceID != nil && req.WorkspaceID.String() == attachment {
		return true
	}
	return slices.ContainsFunc(req.Workspaces, func(sel client.WorkspaceSelection) bool { return sel.WorkspaceID == attachment })
}

// startFolderRefusal holds a start folder to the attachments of this run, by the
// request alone: a workspace or the legacy drive, or a drive_id of drives[]. The subpath is already
// cleaned and has no ".." element (startFolderShapeRefusal), so the resolved
// folder stays inside the mount. The folder itself, a named repo included, is
// resolved over the run's targets once its workspaces are seeded (startFolderDir).
func startFolderRefusal(req createRunRequest) *runRefusal {
	f := req.StartFolder
	switch {
	case f == nil || f.Kind != client.StartFolderAttachment:
		return nil
	case f.DriveID != "":
		if driveStartFolderKnown(req, f.DriveID) {
			return nil
		}
		return runError(http.StatusBadRequest, reasonStartFolderInvalid, startFolderAttachmentMsg(f.DriveID))
	case startFolderAttachmentKnown(req, f.Attachment):
		return nil
	}
	return runError(http.StatusBadRequest, reasonStartFolderInvalid, startFolderAttachmentMsg(f.Attachment))
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
	// Every request, older clients' too: the overlay is read whole before any
	// gate (the run-mode checks below among them) reads its keys.
	if refusal := policyOverlayRefusal(*req); refusal != nil {
		return refusal
	}
	if !req.UsesRunMode() {
		return nil
	}
	for _, check := range []func(createRunRequest) *runRefusal{
		runModeShapeRefusal, runModeConflictRefusal, runModeRequiredRefusal, startFolderRefusal, runModeUnavailableRefusal, attendedToolRulesRefusal,
	} {
		if refusal := check(*req); refusal != nil {
			return refusal
		}
	}
	projectRunMode(req)
	return nil
}

// attendedToolRulesRefusal: an interactive run's tool rules are enforced by the
// harness's own settings. Claude Code's are written into its managed settings
// (agentpolicy.WithToolRules), and its default is answered in the fold
// (attendedDefaultRefusal), where the level that picks those settings is
// known. No other harness has such settings, so any choice but the lane's own
// (the harness asks you, a hold default) is refused for it.
func attendedToolRulesRefusal(req createRunRequest) *runRefusal {
	if req.Experience != client.ExperienceInteractive {
		return nil
	}
	for i, t := range req.Tools {
		if t.ID != claudeCodeHarness && (len(t.ToolRules) > 0 || (t.DefaultEffect != "" && t.DefaultEffect != types.ToolHold)) {
			return runError(http.StatusUnprocessableEntity, reasonRequestFieldUnavailable,
				runModeUnavailableMsg(fmt.Sprintf("tools[%d]: tool rules in an interactive run", i)))
		}
	}
	return nil
}

// attendedDefaultRefusal refuses a default Claude Code cannot take in an
// interactive run, with the sentence step 5 greys it with: its managed settings
// name tools, and a tool no rule names is answered by its mode, which asks.
func attendedDefaultRefusal(req createRunRequest, level types.AutonomyLevel) *runRefusal {
	if !requestIsInteractive(req) {
		return nil
	}
	for _, t := range req.Tools {
		if code := attendedDefaultReasons(level)[t.DefaultEffect]; t.ID == claudeCodeHarness && code != "" {
			return runError(http.StatusUnprocessableEntity, reasonRequestFieldUnavailable, accessSentences[code])
		}
	}
	return nil
}

// attendedToolNamesRefusal refuses an interactive Claude Code run whose policy or
// own rules name a tool its managed settings could not carry exactly (a pattern,
// or a whole MCP server), here rather than at dispatch after the 201. Dispatch
// still refuses it (agentPolicyFor), for a run no door folded.
func attendedToolNamesRefusal(req createRunRequest, spec types.RunPolicySpec) *runRefusal {
	if !requestIsInteractive(req) {
		return nil
	}
	rules, claude := spec.ToolRules, req.Agent == claudeCodeHarness
	for _, t := range req.Tools {
		if t.ID == claudeCodeHarness && t.Kind == client.IncludedToolHarness {
			rules, claude = slices.Concat(rules, t.ToolRules), true
		}
	}
	if err := agentpolicy.CheckToolRules(rules); claude && err != nil {
		return runError(http.StatusUnprocessableEntity, reasonRequestFieldUnavailable,
			"This run's tool rules can't be written into Claude Code's managed settings: "+err.Error())
	}
	return nil
}

// attendedDefaultReasons is why Claude Code's default in an interactive run at
// level can be neither "Run it" nor "Block it": where the managed settings the
// level brings lock bypass off, it can never skip asking; elsewhere Wardyn has
// no setting for it. The locked L2 document leaves bypass on as L2's does.
func attendedDefaultReasons(level types.AutonomyLevel) map[types.ToolEffect]string {
	skip := "attended_rules_unavailable"
	if _, doc, _ := agentpolicy.ForAgent(claudeCodeHarness, level, false, false); agentpolicy.DisablesBypass(doc) {
		skip = "attended_no_skip"
	}
	return map[types.ToolEffect]string{types.ToolAllow: skip, types.ToolDeny: "attended_rules_unavailable"}
}

// projectRunMode writes the honourable carriers onto the legacy fields the one
// engine reads. It never chooses: every value is one the person sent. The
// legacy agent and model provider are the PRIMARY harness's (primaryHarness):
// the startup harness, else the first; none when the run includes no harness,
// which starts from the agent base image (harnessSetImage).
func projectRunMode(req *createRunRequest) {
	switch req.Experience {
	case client.ExperienceBackground:
		w := req.Workload
		if w.Kind == client.WorkloadCommand {
			req.TaskMode, req.Task = "exec", w.Command
			if t, ok := primaryHarness(*req); ok {
				req.Agent, req.ModelProvider = t.ID, t.ModelProvider
			}
			return
		}
		req.Agent, req.Task = w.Agent, w.Task
		if len(req.Tools) == 1 {
			req.ModelProvider = req.Tools[0].ModelProvider
		}
	case client.ExperienceInteractive:
		req.Interactive = true
		if t, ok := primaryHarness(*req); ok {
			req.Agent, req.ModelProvider = t.ID, t.ModelProvider
		}
		if s := req.Startup; s != nil {
			switch s.Kind {
			case client.StartupHarness:
				// The starting prompt is the harness's first message: the boot seed
				// (interactiveBootSeed) the started harness opens with.
				req.InteractiveStart, req.Task = "agent", s.Prompt
			case client.StartupCommand:
				req.InteractiveStart, req.Task = "shell", s.Command
			}
		}
	}
}
