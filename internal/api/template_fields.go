// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"reflect"
	"strings"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The template field registry: every request and policy field a template can
// meet, either carried (with its refinement part, the
// section of the Access tab it lives in, and what leaving it out means) or
// excluded with a reason. A template document
// is read against it, field by field, so an unclassified field cannot be
// accepted by accident. TestTemplateFieldRegistryCoversEveryReachableField
// walks the real types and fails when a field, at any depth, has no entry or
// an entry has no field, which is how a new request field is forced to choose.
//
// Omission is explicit. Leaving a field out of a template never means "off":
// it means the template does not specify it, and the launch baseline (which
// the organisation's current defaults can change) or an unset control takes
// over. A field present with an empty, false or zero value is a choice, and
// Empty says what that choice means.
//
// The exported fields are the JSON of ui/src/app/lib/template-fields.golden.json,
// which the console reads and TestTemplateFieldsGolden pins.

// What leaving a field out means, and what a present empty value means.
const (
	templateOmitBaseline = "baseline"
	templateOmitRequired = "unset_required"
	templateOmitOptional = "unset_optional"
	templateEmptySame    = "same_as_omitted"
	templateEmptyValue   = "value"
	templateEmptyAll     = "all"
	templateEmptyNever   = "never"
)

// Why a field is excluded.
const (
	templateExcludeSecret  = "secret"
	templateExcludeOrigin  = "origin"
	templateExcludeRetired = "retired"
	// templateExcludeLaunch: entered when the run is launched, and never
	// prefilled in this release.
	templateExcludeLaunch = "launch"
)

// Where a field sits in the Access tab. Push rules exist only inside the SCM
// entry that brings the repository access, one set per provider and
// organisation; tool rules, tool approvals and the model provider belong to
// each harness; component settings to the component.
const (
	templateSectionRunWide  = "run_wide"
	templateSectionSCMEntry = "scm_entry"
	templateSectionHarness  = "harness"
	templateSectionComp     = "component"
)

// TemplateFieldRule is one top-level request or policy field.
type TemplateFieldRule struct {
	Name string `json:"name"`
	// Part is the refinement group the field belongs to.
	Part client.TemplatePart `json:"part,omitempty"`
	// Omitted is what leaving the field out means: baseline (the launch default
	// applies, and it can change), unset_required (the draft needs a choice
	// before launch) or unset_optional (nothing is asked for).
	Omitted string `json:"omitted,omitempty"`
	// Empty is what a present empty, false or zero value means: same_as_omitted,
	// value (an explicit choice, such as an empty allowlist), all, or never.
	Empty string `json:"empty,omitempty"`
	// Meaning is the sentence the refinement flow shows for the omission.
	Meaning string `json:"meaning,omitempty"`
	// Sensitive marks free text a person writes for one run. It is kept only
	// when they choose to include it.
	Sensitive bool `json:"sensitive,omitempty"`
	// PersonOnly marks a field that names something one person has; a shared
	// template cannot carry it.
	PersonOnly bool `json:"person_only,omitempty"`
	// Section is where the field sits in the Access tab: run_wide, scm_entry,
	// harness or component. KeyedBy says what the owning entry is keyed by
	// (provider+org for an SCM entry, harness for a harness).
	Section string `json:"section,omitempty"`
	KeyedBy string `json:"keyed_by,omitempty"`
	// Pending names the lane that must land before a template can carry the
	// field. A document that names it is refused as unavailable, never
	// dropped. A request row with no CreateRunRequest field is a working name
	// for a carrier that does not exist yet: the owning lane picks the wire
	// name and renames the row when it lands.
	Pending string `json:"pending,omitempty"`
	// Excluded, when set, is why a template never carries the field: secret,
	// origin (launch-only selectors), retired, or launch (entered at launch).
	Excluded string `json:"excluded,omitempty"`
	// Why is the reason sentence for an excluded field.
	Why string `json:"why,omitempty"`
}

func carriedField(name string, part client.TemplatePart, omitted, empty, meaning string) TemplateFieldRule {
	return TemplateFieldRule{Name: name, Part: part, Omitted: omitted, Empty: empty, Meaning: meaning, Section: templateSectionRunWide}
}

func (r TemplateFieldRule) in(section, keyedBy string) TemplateFieldRule {
	r.Section, r.KeyedBy = section, keyedBy
	return r
}

func (r TemplateFieldRule) sensitive() TemplateFieldRule  { r.Sensitive = true; return r }
func (r TemplateFieldRule) personOnly() TemplateFieldRule { r.PersonOnly = true; return r }

func excludedField(name, category, why string) TemplateFieldRule {
	return TemplateFieldRule{Name: name, Excluded: category, Why: why}
}

const (
	partRunner, partResources        = client.TemplatePartRunner, client.TemplatePartResources
	partRepos, partDrives, partTools = client.TemplatePartRepositories, client.TemplatePartDrives, client.TemplatePartToolsImage
	partEgress, partCreds, partComps = client.TemplatePartEgress, client.TemplatePartCredentials, client.TemplatePartComponents
	partRules, partPolicyRef         = client.TemplatePartAccessRules, client.TemplatePartPolicyRef
)

// templateRequestRules classifies every CreateRunRequest field, plus the working
// names of the carriers the run-mode and repositories lanes have yet to add.
var templateRequestRules = []TemplateFieldRule{
	carriedField("agent", partTools, templateOmitOptional, templateEmptySame,
		"No agent is chosen. A background agent task needs one; a command or an interactive environment does not. A task is never turned into a shell command.").
		in(templateSectionHarness, "harness"),
	carriedField("repo", partRepos, templateOmitOptional, templateEmptySame,
		"No repository is attached through this older single-repository field."),
	carriedField("task", partTools, templateOmitOptional, templateEmptySame,
		"No task text is carried: the person who uses the template writes it.").sensitive(),
	excludedField("title", templateExcludeLaunch,
		"A run is named when it is launched: the Name this run dialog opens empty in this release, so a template carries no title."),
	excludedField("description", templateExcludeLaunch,
		"A run is described when it is launched: the Name this run dialog opens empty in this release, so a template carries no description."),
	carriedField("policy_id", partPolicyRef, templateOmitBaseline, templateEmptySame,
		"The organisation's default policy applies when the template is used, and that default can change. It is a whole reference and never appears beside inline_policy."),
	carriedField("confinement_class", partRunner, templateOmitBaseline, templateEmptySame,
		"The barrier follows the organisation's default and floor when the template is used."),
	carriedField("interactive", partRunner, templateOmitRequired, templateEmptyValue,
		"No run mode is assumed: the person chooses Background task or Interactive environment. false is the choice of a background task."),
	carriedField("workspace_id", partRepos, templateOmitOptional, templateEmptySame,
		"No workspace is attached through this older single-workspace field."),
	carriedField("inline_policy", "", templateOmitBaseline, templateEmptyValue,
		"No policy field is set by the template. Only the policy keys it names are laid onto the source policy; a key it leaves out keeps the source's value."),
	excludedField("policy_overlay", templateExcludeOrigin,
		"In a document the overlay is written under inline_policy; policy_overlay is how a client sends it."),
	carriedField("devcontainer_repo", partTools, templateOmitOptional, templateEmptySame,
		"No image is built from a repository."),
	carriedField("devcontainer_ref", partTools, templateOmitOptional, templateEmptySame,
		"The build uses the repository's default branch."),
	carriedField("image", partTools, templateOmitOptional, templateEmptySame,
		"No image of the person's own is named: the run uses the agent's or the organisation's image."),
	carriedField("task_mode", partTools, templateOmitRequired, templateEmptySame,
		"Under a background task the person chooses an agent task or a command. A harness is inferred only when an agent is present, and a task is never turned into a command."),
	carriedField("interactive_start", partTools, templateOmitOptional, templateEmptySame,
		"Nothing starts automatically unless chosen. A removed startup tool is never replaced by another."),
	carriedField("seed_auto_tools", partRules, templateOmitBaseline, templateEmptyValue,
		"The starting prompt asks before using tools. False is a choice to keep asking.").
		in(templateSectionHarness, "harness"),
	carriedField("tool_approvals", partRules, templateOmitBaseline, templateEmptySame,
		"Tool use follows the default approval behaviour.").
		in(templateSectionHarness, "harness"),
	carriedField("workspaces", partRepos, templateOmitOptional, templateEmptyValue,
		"No workspace is attached. An empty list says no workspace; it is not the tab's explicit choice of no repositories or drives."),
	excludedField("integration_id", templateExcludeRetired,
		"The server refuses integration_id at launch. Choose a model provider instead."),
	carriedField("model_provider", partCreds, templateOmitBaseline, templateEmptySame,
		"The run uses the model provider the person's defaults give.").
		in(templateSectionHarness, "harness"),
	carriedField("drive", partDrives, templateOmitOptional, templateEmptyValue,
		"No drive is attached or declined. enabled false is a choice to leave the drive out."),
	carriedField("drives", partDrives, templateOmitOptional, templateEmptyValue,
		"No drive from the drive list is attached. Each drive is checked against the launching person's own grants."),
	carriedField("components", partComps, templateOmitOptional, templateEmptyValue,
		"No component is attached.").
		in(templateSectionComp, "component"),
	carriedField("allowed_image", partTools, templateOmitOptional, templateEmptySame,
		"No image is chosen from the organisation's allowed list."),
	carriedField("placement", partRunner, templateOmitBaseline, templateEmptySame,
		"The run is placed by the pool defaults when the template is used."),
	carriedField("runner_id", partRunner, templateOmitOptional, templateEmptySame,
		"No runner is pinned: any of the person's eligible runners may take the run.").personOnly(),
	carriedField("resources", partResources, templateOmitBaseline, templateEmptySame,
		"CPU and memory follow the placement's defaults. A zero field asks for the default."),
	carriedField("overrides", partComps, templateOmitOptional, templateEmptyValue,
		"No per-run edits are applied to components.").
		in(templateSectionComp, "component"),
	excludedField("preset", templateExcludeOrigin,
		"A preset is a launch shortcut. A template records its own origin, and the setup it holds is carried field by field."),
	excludedField("mode_contract", templateExcludeOrigin,
		"The contract marker belongs to the request a client sends, not to the setup a template holds."),
	excludedField("preset_version", templateExcludeOrigin,
		"A preset's version only means something beside its preset."),
	carriedField("runner_pool_id", partRunner, templateOmitBaseline, templateEmptySame,
		"The pool resolves from the person's own default, then the organisation's, when the template is used. A default that is unavailable or refused never falls through to another pool: the person chooses. A named pool narrows where the run may go and grants nothing, and the launcher's pool-use policy is checked at use."),
	carriedField("ends_after_sec", partRunner, templateOmitBaseline, templateEmptyNever,
		"The run takes the default end when the template is used, and that default can change. An end is a length of time from launch, checked again then, never a permission."),
	excludedField("max_wait_sec", templateExcludeLaunch,
		"How long a run may wait for a slot in a full pool is chosen at each launch; the default wait suits a template's runs."),
	carriedField("no_end", partRunner, templateOmitBaseline, templateEmptySame,
		"The run takes the default end when the template is used. Asking for no end is checked again at launch against the pool, the deployment and the person's own limits, and is never a permission."),
	// The run-mode carriers (run_mode.go): what a person is starting. A new client
	// sends them in place of interactive, agent, task, task_mode and
	// interactive_start, which stay only as the older-client mapping. What the run
	// cannot honour yet (more than one tool, per-tool rules, a folder inside an
	// attachment) is refused by runModeUnavailable at the run and in a template.
	carriedField("experience", partRunner, templateOmitRequired, templateEmptySame,
		"No mode is assumed: Background task or Interactive environment is chosen, and never inferred."),
	carriedField("shape", partRunner, templateOmitOptional, templateEmptySame,
		"No shape is recorded. The experience, workload and tools already say what the run is; a shape only names the choice it was set up from."),
	carriedField("workload", partTools, templateOmitRequired, templateEmptySame,
		"A background task needs an agent task or a command, and none is chosen. An interactive environment has no workload.").sensitive(),
	carriedField("tools", partTools, templateOmitOptional, templateEmptyValue,
		"No tool is included, which is valid: the sandbox starts from the agent base image. An interactive environment or a command may include several harnesses, and at most one starts. Each harness has its own model provider, tool rules and default effect, and nothing is shared between tools.").
		in(templateSectionHarness, "harness"),
	carriedField("startup", partTools, templateOmitOptional, templateEmptySame,
		"Nothing starts automatically unless chosen: one included harness, which may open with a starting prompt, one command, or none. A removed startup tool is never replaced by another.").sensitive(),
	carriedField("start_folder", partRepos, templateOmitBaseline, templateEmptySame,
		"The first attached repository, or the sandbox's default working directory when none is attached. A run has exactly one starting folder."),
	carriedField("no_repositories_or_drives", partRepos, templateOmitRequired, templateEmptySame,
		"The tab stays incomplete until something is attached or the person explicitly chooses no repositories or drives. Choosing none warns that nothing in the sandbox is kept unless it is pushed or copied out."),
}

// templatePolicyRules classifies every RunPolicySpec field, spoken of as
// inline_policy.<name> in a template.
var templatePolicyRules = []TemplateFieldRule{
	carriedField("allowed_domains", partEgress, templateOmitBaseline, templateEmptyValue,
		"The organisation's default allowlist applies. An empty list is a choice: nothing is reachable unless a component adds it."),
	carriedField("denied_domains", partEgress, templateOmitBaseline, templateEmptyValue,
		"Managed denials still apply. A template that drops its own denial can widen access."),
	carriedField("allow_all_egress", partEgress, templateOmitBaseline, templateEmptyValue,
		"Egress follows the baseline. False is a choice of default-deny."),
	carriedField("first_use_approval", partEgress, templateOmitBaseline, templateEmptySame,
		"Unknown destinations are handled as the baseline says."),
	carriedField("first_use_hold_seconds", partEgress, templateOmitBaseline, templateEmptySame,
		"A held connection waits the built-in time. Zero is the same."),
	carriedField("max_holds", partEgress, templateOmitBaseline, templateEmptySame,
		"The built-in cap on concurrent holds applies. Zero is the same."),
	carriedField("allowed_methods", partEgress, templateOmitBaseline, templateEmptyAll,
		"Methods follow the baseline. An empty list allows every method."),
	carriedField("min_confinement_class", partRunner, templateOmitBaseline, templateEmptySame,
		"The floor follows the organisation's. A template cannot lower a managed floor."),
	carriedField("eligible_grants", partCreds, templateOmitOptional, templateEmptyValue,
		"No credential eligibility comes from the template: the person's own ceiling decides."),
	carriedField("auto_stop_after_sec", partResources, templateOmitBaseline, templateEmptyNever,
		"Idle stop follows the baseline. Zero or a negative number is a choice to never stop for idleness."),
	carriedField("workspace_mounts", partRepos, templateOmitOptional, templateEmptyValue,
		"No host path is mounted."),
	carriedField("workspace_repos", partRepos, templateOmitOptional, templateEmptyValue,
		"No extra repository is cloned."),
	carriedField("llm_inspection", partRules, templateOmitBaseline, templateEmptyValue,
		"Content inspection follows the organisation's policy, off unless it is turned on."),
	carriedField("ui_apps", partTools, templateOmitOptional, templateEmptyValue,
		"No application is declared for the browser."),
	carriedField("resources", partResources, templateOmitBaseline, templateEmptySame,
		"Limits follow the platform defaults. A zero field means the default."),
	carriedField("tool_rules", partRules, templateOmitOptional, templateEmptySame,
		"No tool rules are added.").
		in(templateSectionHarness, "harness"),
	carriedField("git_push_any_branch", partRules, templateOmitBaseline, templateEmptyValue,
		"Pushes stay inside the run's own branch namespace. False is a choice to keep that.").
		in(templateSectionSCMEntry, "provider+org"),
	carriedField("push_rules", partRules, templateOmitOptional, templateEmptySame,
		"Push rules exist only inside an SCM entry, one set per provider and organisation (overrides.push_rules).").
		in(templateSectionSCMEntry, "provider+org"),
	carriedField("azure_devops_capabilities", partCreds, templateOmitBaseline, templateEmptySame,
		"The provider's default profile applies.").
		in(templateSectionSCMEntry, "provider+org"),
	carriedField("github_capabilities", partCreds, templateOmitBaseline, templateEmptySame,
		"The provider's default profile applies.").
		in(templateSectionSCMEntry, "provider+org"),
}

// templateNestedCarried lists, per nested struct type, the JSON fields a
// template carries; templateNestedExcluded names the nested fields it never
// does. A nested field in neither is a field the guard test refuses.
var templateNestedCarried = map[string]string{
	"ADOOverrides":        "capabilities",
	"AgentOverrides":      "add_hosts remove_hosts add_secrets remove_secrets tool_rules",
	"CLIAWS":              "start_url sso_region account_id role_name region",
	"CLIAzure":            "subscriptions access",
	"CLIGitHub":           "capabilities",
	"ComponentCLI":        "kind azure aws github",
	"ComponentDefinition": "description hosts secrets config cli mcp",
	"ComponentDelivery":   "mode host header format plain_http var file",
	"ComponentMCP":        "url transport",
	"ComponentRef":        "id inline name builtin org repos access mcp_harnesses",
	"ComponentSecret":     "secret_name shared delivery",
	"DriveSelection":      "enabled read_only",
	"GitPATOverride":      "host repos access api",
	"GrantSpec":           "kind scope ttl_seconds requires_approval owner_only",
	"IncludedTool":        "id kind model_provider component tool_rules default_effect",
	"LLMInspectionSpec": "mode workspace_secret_names detect_secrets detect_secret_patterns detect_entropy detect_pii detector_sidecar_url " +
		"classified_markers scan_attachments inspect_forward_egress max_scan_bytes on_scanner_error require_inspectable_llm intercept_tls block_min_severity",
	"PushRuleOverride":   "provider host org deny_paths require_review_paths",
	"PushRulesSpec":      "deny_paths max_inspect_pack_mib require_review_paths hold_seconds deny_new_executables max_file_size_mib",
	"DriveAttachment":    "drive_id read_only",
	"ResourceLimits":     "cpu_millis memory_mib pids_limit disk_mib",
	"RunHostOverrides":   "add_allowed add_denied remove_allowed remove_denied",
	"RunEgressOverrides": "first_use_approval allow_all_egress",
	"RunOverrides":       "agent azure_devops git_pat push_rules hosts egress",
	"RunResources":       "cpu_millis memory_mib disk_mib",
	"RunStartup":         "kind tool command prompt",
	"RunWorkload":        "kind agent task command",
	"RepoSelection":      "repo ref",
	"StartFolder":        "kind attachment repo drive_id subpath",
	"SecretOverride":     "secret_name host header format",
	"ToolRule":           "tool effect",
	"UIApp":              "name port path",
	"WorkspaceMount":     "source target read_only",
	"WorkspaceRepo":      "repo target ref",
	"WorkspaceSelection": "workspace_id enabled_optional read_only target repos",
}

var templateNestedExcluded = map[string]TemplateFieldRule{
	"LLMInspectionSpec.workspace_secret_values": excludedField("workspace_secret_values", templateExcludeSecret,
		"Secret values are runtime-only: the server resolves them, and a template names secrets instead."),
}

// templateRule finds a top-level rule by name.
func templateRule(rules []TemplateFieldRule, name string) (TemplateFieldRule, bool) {
	for _, r := range rules {
		if r.Name == name {
			return r, true
		}
	}
	return TemplateFieldRule{}, false
}

// templateStructField finds a struct field by its JSON name.
func templateStructField(t reflect.Type, name string) (reflect.StructField, bool) {
	for i := range t.NumField() {
		f := t.Field(i)
		if tag, _, _ := strings.Cut(f.Tag.Get("json"), ","); tag == name {
			return f, true
		}
	}
	return reflect.StructField{}, false
}
