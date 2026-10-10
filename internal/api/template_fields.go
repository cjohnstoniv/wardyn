// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"reflect"
	"strings"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The template field registry: every request and policy field a template can
// meet, either carried (with its owning wizard tab, its refinement part, and
// what leaving it out means) or excluded with a reason. A template document
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

// The wizard tabs that own a field (docs: Info, Runner, Repositories & Drives,
// Tools & Image, Access, Resolved Policy). Resolved Policy owns none: it is read-only.
const (
	templateTabInfo       = "info"
	templateTabRunner     = "runner"
	templateTabRepoDrives = "repositories_drives"
	templateTabToolsImage = "tools_image"
	templateTabAccess     = "access"
)

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
)

// TemplateFieldRule is one top-level request or policy field.
type TemplateFieldRule struct {
	Name string `json:"name"`
	// Tab is the wizard tab that owns the control.
	Tab string `json:"tab,omitempty"`
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
	// Excluded, when set, is why a template never carries the field: secret,
	// origin (launch-only selectors) or retired.
	Excluded string `json:"excluded,omitempty"`
	// Why is the reason sentence for an excluded field.
	Why string `json:"why,omitempty"`
}

func carriedField(name, tab string, part client.TemplatePart, omitted, empty, meaning string) TemplateFieldRule {
	return TemplateFieldRule{Name: name, Tab: tab, Part: part, Omitted: omitted, Empty: empty, Meaning: meaning}
}

func (r TemplateFieldRule) sensitive() TemplateFieldRule  { r.Sensitive = true; return r }
func (r TemplateFieldRule) personOnly() TemplateFieldRule { r.PersonOnly = true; return r }

func excludedField(name, category, why string) TemplateFieldRule {
	return TemplateFieldRule{Name: name, Excluded: category, Why: why}
}

const (
	partInfo, partRunner, partResources = client.TemplatePartInfo, client.TemplatePartRunner, client.TemplatePartResources
	partRepos, partDrives, partTools    = client.TemplatePartRepositories, client.TemplatePartDrives, client.TemplatePartToolsImage
	partEgress, partCreds, partComps    = client.TemplatePartEgress, client.TemplatePartCredentials, client.TemplatePartComponents
	partRules, partPolicyRef            = client.TemplatePartAccessRules, client.TemplatePartPolicyRef
)

// templateRequestRules classifies every CreateRunRequest field, plus pool_id.
var templateRequestRules = []TemplateFieldRule{
	carriedField("agent", templateTabToolsImage, partTools, templateOmitRequired, templateEmptySame,
		"The run has no agent until the person picks one. A task is never turned into a shell command."),
	carriedField("repo", templateTabRepoDrives, partRepos, templateOmitOptional, templateEmptySame,
		"No repository is attached through this older single-repository field."),
	carriedField("task", templateTabToolsImage, partTools, templateOmitOptional, templateEmptySame,
		"No task text is carried: the person who uses the template writes it.").sensitive(),
	carriedField("title", templateTabInfo, partInfo, templateOmitOptional, templateEmptySame,
		"No title is carried: the person who uses the template names the run.").sensitive(),
	carriedField("description", templateTabInfo, partInfo, templateOmitOptional, templateEmptySame,
		"No description is carried.").sensitive(),
	carriedField("policy_id", templateTabAccess, partPolicyRef, templateOmitBaseline, templateEmptySame,
		"The organisation's default policy applies when the template is used, and that default can change."),
	carriedField("confinement_class", templateTabRunner, partRunner, templateOmitBaseline, templateEmptySame,
		"The barrier follows the organisation's default and floor when the template is used."),
	carriedField("interactive", templateTabRunner, partRunner, templateOmitBaseline, templateEmptyValue,
		"The run takes the default experience. False is a choice to run without an interactive session."),
	carriedField("workspace_id", templateTabRepoDrives, partRepos, templateOmitOptional, templateEmptySame,
		"No workspace is attached through this older single-workspace field."),
	carriedField("inline_policy", templateTabAccess, "", templateOmitBaseline, templateEmptyValue,
		"No policy fields are carried: each policy field below is left to the baseline unless it is listed."),
	carriedField("devcontainer_repo", templateTabToolsImage, partTools, templateOmitOptional, templateEmptySame,
		"No image is built from a repository."),
	carriedField("devcontainer_ref", templateTabToolsImage, partTools, templateOmitOptional, templateEmptySame,
		"The build uses the repository's default branch."),
	carriedField("image", templateTabToolsImage, partTools, templateOmitOptional, templateEmptySame,
		"No image of the person's own is named: the run uses the agent's or the organisation's image."),
	carriedField("task_mode", templateTabToolsImage, partTools, templateOmitBaseline, templateEmptySame,
		"A task runs with the agent harness. An existing task is never turned into a shell command."),
	carriedField("interactive_start", templateTabToolsImage, partTools, templateOmitBaseline, templateEmptySame,
		"An interactive run starts with its default."),
	carriedField("seed_auto_tools", templateTabAccess, partRules, templateOmitBaseline, templateEmptyValue,
		"The starting prompt asks before using tools. False is a choice to keep asking."),
	carriedField("tool_approvals", templateTabAccess, partRules, templateOmitBaseline, templateEmptySame,
		"Tool use follows the default approval behaviour."),
	carriedField("workspaces", templateTabRepoDrives, partRepos, templateOmitOptional, templateEmptyValue,
		"No workspace is attached. An empty list is a choice to attach none."),
	excludedField("integration_id", templateExcludeRetired,
		"The server refuses integration_id at launch. Choose a model provider instead."),
	carriedField("model_provider", templateTabAccess, partCreds, templateOmitBaseline, templateEmptySame,
		"The run uses the model provider the person's defaults give."),
	carriedField("drive", templateTabRepoDrives, partDrives, templateOmitOptional, templateEmptyValue,
		"No drive is attached or declined. enabled false is a choice to leave the drive out."),
	carriedField("components", templateTabAccess, partComps, templateOmitOptional, templateEmptyValue,
		"No component is attached."),
	carriedField("allowed_image", templateTabToolsImage, partTools, templateOmitOptional, templateEmptySame,
		"No image is chosen from the organisation's allowed list."),
	carriedField("placement", templateTabRunner, partRunner, templateOmitBaseline, templateEmptySame,
		"The run is placed by the pool defaults when the template is used."),
	carriedField("runner_id", templateTabRunner, partRunner, templateOmitOptional, templateEmptySame,
		"No runner is pinned: any of the person's eligible runners may take the run.").personOnly(),
	carriedField("resources", templateTabRunner, partResources, templateOmitBaseline, templateEmptySame,
		"CPU and memory follow the placement's defaults. A zero field asks for the default."),
	carriedField("overrides", templateTabAccess, partComps, templateOmitOptional, templateEmptyValue,
		"No per-run edits are applied to components."),
	excludedField("preset", templateExcludeOrigin,
		"A preset is a launch shortcut. A template records its own origin, and the setup it holds is carried field by field."),
	excludedField("preset_version", templateExcludeOrigin,
		"A preset's version only means something beside its preset."),
	carriedField("pool_id", templateTabRunner, partRunner, templateOmitBaseline, templateEmptySame,
		"The pool follows the organisation's default and the person's own default when the template is used."),
}

// templatePolicyRules classifies every RunPolicySpec field, spoken of as
// inline_policy.<name> in a template.
var templatePolicyRules = []TemplateFieldRule{
	carriedField("allowed_domains", templateTabAccess, partEgress, templateOmitBaseline, templateEmptyValue,
		"The organisation's default allowlist applies. An empty list is a choice: nothing is reachable unless a component adds it."),
	carriedField("denied_domains", templateTabAccess, partEgress, templateOmitBaseline, templateEmptyValue,
		"Managed denials still apply. A template that drops its own denial can widen access."),
	carriedField("allow_all_egress", templateTabAccess, partEgress, templateOmitBaseline, templateEmptyValue,
		"Egress follows the baseline. False is a choice of default-deny."),
	carriedField("first_use_approval", templateTabAccess, partEgress, templateOmitBaseline, templateEmptySame,
		"Unknown destinations are handled as the baseline says."),
	carriedField("first_use_hold_seconds", templateTabAccess, partEgress, templateOmitBaseline, templateEmptySame,
		"A held connection waits the built-in time. Zero is the same."),
	carriedField("max_holds", templateTabAccess, partEgress, templateOmitBaseline, templateEmptySame,
		"The built-in cap on concurrent holds applies. Zero is the same."),
	carriedField("allowed_methods", templateTabAccess, partEgress, templateOmitBaseline, templateEmptyAll,
		"Methods follow the baseline. An empty list allows every method."),
	carriedField("min_confinement_class", templateTabRunner, partRunner, templateOmitBaseline, templateEmptySame,
		"The floor follows the organisation's. A template cannot lower a managed floor."),
	carriedField("eligible_grants", templateTabAccess, partCreds, templateOmitOptional, templateEmptyValue,
		"No credential eligibility comes from the template: the person's own ceiling decides."),
	carriedField("auto_stop_after_sec", templateTabRunner, partResources, templateOmitBaseline, templateEmptyNever,
		"Idle stop follows the baseline. Zero or a negative number is a choice to never stop for idleness."),
	carriedField("workspace_mounts", templateTabRepoDrives, partRepos, templateOmitOptional, templateEmptyValue,
		"No host path is mounted."),
	carriedField("workspace_repos", templateTabRepoDrives, partRepos, templateOmitOptional, templateEmptyValue,
		"No extra repository is cloned."),
	carriedField("llm_inspection", templateTabAccess, partRules, templateOmitBaseline, templateEmptyValue,
		"Content inspection follows the organisation's policy, off unless it is turned on."),
	carriedField("ui_apps", templateTabToolsImage, partTools, templateOmitOptional, templateEmptyValue,
		"No application is declared for the browser."),
	carriedField("resources", templateTabRunner, partResources, templateOmitBaseline, templateEmptySame,
		"Limits follow the platform defaults. A zero field means the default."),
	carriedField("tool_rules", templateTabAccess, partRules, templateOmitOptional, templateEmptySame,
		"No tool rules are added."),
	carriedField("git_push_any_branch", templateTabAccess, partRules, templateOmitBaseline, templateEmptyValue,
		"Pushes stay inside the run's own branch namespace. False is a choice to keep that."),
	carriedField("push_rules", templateTabAccess, partRules, templateOmitOptional, templateEmptySame,
		"No push content rules are added. An all-zero block reads as absent."),
	carriedField("azure_devops_capabilities", templateTabAccess, partCreds, templateOmitBaseline, templateEmptySame,
		"The provider's default profile applies."),
	carriedField("github_capabilities", templateTabAccess, partCreds, templateOmitBaseline, templateEmptySame,
		"The provider's default profile applies."),
}

// templateNestedCarried lists, per nested struct type, the JSON fields a
// template carries; templateNestedExcluded names the nested fields it never
// does. A nested field in neither is a field the guard test refuses.
var templateNestedCarried = map[string]string{
	"ADOOverrides":        "capabilities",
	"AgentOverrides":      "add_hosts remove_hosts add_secrets remove_secrets tool_rules",
	"ComponentDefinition": "hosts secrets config",
	"ComponentDelivery":   "mode host header format plain_http var file",
	"ComponentRef":        "id inline name builtin org repos access",
	"ComponentSecret":     "secret_name shared delivery",
	"DriveSelection":      "enabled read_only",
	"GitPATOverride":      "host repos access api",
	"GrantSpec":           "kind scope ttl_seconds requires_approval owner_only",
	"LLMInspectionSpec": "mode workspace_secret_names detect_secrets detect_secret_patterns detect_entropy detect_pii detector_sidecar_url " +
		"classified_markers scan_attachments inspect_forward_egress max_scan_bytes on_scanner_error require_inspectable_llm intercept_tls block_min_severity",
	"PushRuleOverride":   "provider org deny_paths require_review_paths",
	"PushRulesSpec":      "deny_paths max_inspect_pack_mib require_review_paths hold_seconds deny_new_executables max_file_size_mib",
	"ResourceLimits":     "cpu_millis memory_mib pids_limit disk_mib",
	"RunOverrides":       "agent azure_devops git_pat push_rules",
	"RunResources":       "cpu_millis memory_mib",
	"SecretOverride":     "secret_name host header format",
	"ToolRule":           "tool effect",
	"UIApp":              "name port path",
	"WorkspaceMount":     "source target read_only",
	"WorkspaceRepo":      "repo target ref",
	"WorkspaceSelection": "workspace_id enabled_optional read_only target",
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
