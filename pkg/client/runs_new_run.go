// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The 0.9 New Run wire contract: the request fields and the dry-run facts the
// console's five tabs (Info, Workspaces, Runner, Access, Policy) are built on. The
// shapes are final; the server fills them lane by lane and refuses, never
// drops, a field that changes a run's posture before its lane has landed.

// Placement says where a run's sandbox lives: PlacementRemote, the
// organisation's own executor, or PlacementLocal, a runner the person
// registered. The console calls the field "Runs on".
type Placement = placement.Placement

// The placements CreateRunRequest.Placement accepts.
const (
	PlacementRemote = placement.Remote
	PlacementLocal  = placement.Local
)

// PlacementRequest is the placement a request asks for, in the shape the
// placement package validates and resolves.
func (r CreateRunRequest) PlacementRequest() placement.Request {
	return placement.Request{Placement: r.Placement, RunnerID: r.RunnerID}
}

// RunResources is the CPU, memory and main-drive size a request asks for. A zero
// field asks for what the run's source policy sets, else the default. The server
// applies what it is given within the strictest of the pool's, the person's
// governance, the deployment's and (on their own runner) the runner's limits: a
// value over that cap is reduced to it, never refused, and the dry runs and the
// 201 say what was applied (AppliedResources, MainDriveApplied).
type RunResources struct {
	// CPUMillis is milli-CPU: 2000 is two CPUs. The console works in tenths of a CPU.
	CPUMillis int `json:"cpu_millis,omitempty"`
	// MemoryMiB is whole MiB.
	MemoryMiB int `json:"memory_mib,omitempty"`
	// DiskMiB is the main drive's size in MiB, 0 to 16 TiB. It is a request: over the
	// effective cap it is reduced and said so, never refused, and a runner that cannot
	// cap disk starts the run uncapped. A size the run's policy writes stays a
	// requirement, and a request may only lower it.
	DiskMiB int `json:"disk_mib,omitempty"`
}

// MainDriveBasis says where a run's main-drive size came from.
type MainDriveBasis string

// The bases of MainDriveApplied.
const (
	MainDriveBasisRequest MainDriveBasis = "request" // the run's own request
	MainDriveBasisPolicy  MainDriveBasis = "policy"  // a size the run's policy writes
	MainDriveBasisDefault MainDriveBasis = "default" // the pool's or the server's default
	MainDriveBasisNone    MainDriveBasis = "none"    // no size: unbounded scratch
)

// MainDriveClampSource says what cut a main-drive size.
type MainDriveClampSource string

// The clamp sources of MainDriveApplied.
const (
	MainDriveClampPool       MainDriveClampSource = "pool"
	MainDriveClampGovernance MainDriveClampSource = "governance"
	MainDriveClampDeployment MainDriveClampSource = "deployment"
	MainDriveClampPolicy     MainDriveClampSource = "policy"
)

// MainDriveApplied is the main-drive size the dry doors say a run gets: the same
// number dispatch creates the sandbox with.
type MainDriveApplied struct {
	// RequestedMiB is the request's own size; absent when it sent none.
	RequestedMiB int `json:"requested_mib,omitempty"`
	// AppliedMiB is what the run gets; 0 is no size, unbounded scratch.
	AppliedMiB int            `json:"applied_mib"`
	Basis      MainDriveBasis `json:"basis"`
	// DefaultSource names the limit whose default sized the run, with basis default.
	DefaultSource LimitSource `json:"default_source,omitempty"`
	// ClampedBy names what cut the size, when something did.
	ClampedBy MainDriveClampSource `json:"clamped_by,omitempty"`
	// Soft is true when a runner that cannot cap disk starts the run uncapped. A size
	// the run's policy writes is never soft.
	Soft bool `json:"soft"`
}

// AppliedResources is what a run's CPU and memory asked for and what it gets,
// on preflight, the policy preview, the 201 and the run.create audit row.
type AppliedResources struct {
	CPUMillis AppliedAmount `json:"cpu_millis"`
	MemoryMiB AppliedAmount `json:"memory_mib"`
}

// AppliedAmount is one field of AppliedResources. Source names the limit that
// cut an over-ask when Capped, else the one whose default an ask of 0 got;
// SourceName is the pool's name (pool) or the governance profile's name
// (governance), and empty for the deployment and the runner's capacity.
type AppliedAmount struct {
	Requested  int         `json:"requested"` // the request's value, else its source policy's; 0 asks for the default
	Applied    int         `json:"applied"`   // what the sandbox gets
	Capped     bool        `json:"capped,omitempty"`
	Source     LimitSource `json:"source,omitempty"`
	SourceName string      `json:"source_name,omitempty"`
}

// RunOverrides carries the edits a person made on the New Run cards, one block
// per component (OD-1). They apply inside the server's resolve fold, before the
// ceiling clamp and the member grant pipeline, so every one is bounded by what
// the person's ceiling allows. types.OverrideNarrowingTable decides, per kind
// and operation, whether an edit may be asked for at all; Items lists a value's
// edits in the table's terms.
//
// An override that only narrows is always accepted. One that widens is clamped
// to the ceiling or refused (see the table). A server that cannot yet apply an
// override refuses it with request_field_unavailable instead of dropping it.
type RunOverrides struct {
	// Agent edits the agent component: its destinations, its header secrets and
	// its tool rules.
	Agent *AgentOverrides `json:"agent,omitempty"`
	// AzureDevOps narrows the run's one Azure DevOps capability set.
	AzureDevOps *ADOOverrides `json:"azure_devops,omitempty"`
	// GitPAT narrows a git_pat grant's scope, one entry per forge host.
	GitPAT []GitPATOverride `json:"git_pat,omitempty"`
	// PushRules adds push content rules, keyed by provider and organisation:
	// each Git section edits its own set and the proxy's git route applies the
	// set matching the repository's provider and organisation.
	PushRules []PushRuleOverride `json:"push_rules,omitempty"`
	// Hosts adds to the run-wide allowed and denied lists on top of the chosen
	// base policy. The base's own hosts are read-only.
	Hosts *RunHostOverrides `json:"hosts,omitempty"`
	// Egress sets the run-wide egress scalars on top of the chosen base policy.
	Egress *RunEgressOverrides `json:"egress,omitempty"`
}

// RunEgressOverrides are the run-wide egress choices a person makes beside the
// base policy's own. Both are bounded by the ceiling like any host they add.
type RunEgressOverrides struct {
	// FirstUseApproval is how a destination no list names is handled:
	// always_deny, deny_with_review or wait_for_review.
	FirstUseApproval types.FirstUseMode `json:"first_use_approval,omitempty"`
	// AllowAllEgress true requests every other public destination (the deny
	// list still wins). False is not an edit: omit it.
	AllowAllEgress *bool `json:"allow_all_egress,omitempty"`
}

// RunHostOverrides are the hosts a person adds to the run as a whole, beside the
// chosen base policy's own (the agent component's hosts are AgentOverrides').
type RunHostOverrides struct {
	// AddAllowed are destinations to reach. Bare DNS names only; the ceiling
	// still bounds them, and the preview shows the ones it dropped.
	AddAllowed []string `json:"add_allowed,omitempty"`
	// AddDenied are destinations to refuse. A host or a wildcard; a deny wins
	// over every allow, so it only takes reach away.
	AddDenied []string `json:"add_denied,omitempty"`
	// RemoveAllowed and RemoveDenied would drop a host of the base policy. They
	// are always refused (override_refused): the base is read-only on a run.
	RemoveAllowed []string `json:"remove_allowed,omitempty"`
	RemoveDenied  []string `json:"remove_denied,omitempty"`
}

// AgentOverrides are the edits on the agent component.
type AgentOverrides struct {
	// AddHosts are destinations to reach. Bare DNS names only: no wildcard, no
	// IP address, no port.
	AddHosts []string `json:"add_hosts,omitempty"`
	// RemoveHosts are destinations the source granted that this run drops.
	RemoveHosts []string `json:"remove_hosts,omitempty"`
	// AddSecrets are stored secrets of the caller's, injected as a request
	// header on a host. Request headers only.
	AddSecrets []SecretOverride `json:"add_secrets,omitempty"`
	// RemoveSecrets names secrets the source granted that this run drops.
	RemoveSecrets []string `json:"remove_secrets,omitempty"`
	// ToolRules are added tool rules. Hold and deny only: a rule that allows is
	// refused, and a rule is never removed.
	ToolRules []types.ToolRule `json:"tool_rules,omitempty"`
}

// SecretOverride injects one stored secret, as a header, on one host.
type SecretOverride struct {
	SecretName string `json:"secret_name"`
	Host       string `json:"host"`
	// Header and Format default as an api_key grant's do.
	Header string `json:"header,omitempty"`
	Format string `json:"format,omitempty"`
}

// ADOOverrides is the capability set the run keeps. It must be a subset of the
// set the source resolved; a capability outside it is refused, never granted.
type ADOOverrides struct {
	Capabilities []string `json:"capabilities"`
}

// GitPATOverride narrows one git_pat grant, found by its forge host. Each field
// that is set narrows: Repos to a subset, Access to read, API to false. A
// value that would widen the grant (access write, api true) is refused.
type GitPATOverride struct {
	Host   string    `json:"host"`
	Repos  *[]string `json:"repos,omitempty"`
	Access string    `json:"access,omitempty"`
	API    *bool     `json:"api,omitempty"`
}

// PushRuleOverride adds push content rules for one provider host and
// organisation. Monotone: it can add a deny or a require-review path, never
// remove one, and it is met with the policy's and the ceiling's rules before the
// ceiling clamp (types.PushRuleSet).
type PushRuleOverride struct {
	// Provider is github or azure_devops.
	Provider string `json:"provider"`
	// Host is the provider's host, for a GitHub Enterprise Server or an Azure
	// DevOps Server. Empty is the provider's public host.
	Host string `json:"host,omitempty"`
	// Org is the GitHub owner or the Azure DevOps organisation, in the
	// canonical spelling types.CanonicalPushOrg accepts.
	Org                string   `json:"org"`
	DenyPaths          []string `json:"deny_paths,omitempty"`
	RequireReviewPaths []string `json:"require_review_paths,omitempty"`
}

// PushRuleKey is the SCM entry a push-rule override is keyed by: the provider
// host and the canonical organisation (types.PushRuleKey), "" when it names no
// entry. Two overrides with one key are refused as duplicates, so "Acme" and
// "acme" cannot both claim one entry.
func (p PushRuleOverride) PushRuleKey() string {
	key, _ := types.PushRuleKey(p.Provider, p.Host, p.Org)
	return key
}

// OverrideItem is one edit of a RunOverrides, in the narrowing table's terms.
// Subject names what is edited, for a sentence the caller can act on.
type OverrideItem struct {
	Kind    types.OverrideKind
	Op      types.OverrideOp
	Subject string
}

// Items lists every edit, in a fixed order, with its narrowing-table kind and
// operation. A value with no edits has none.
func (o RunOverrides) Items() []OverrideItem {
	var out []OverrideItem
	if a := o.Agent; a != nil {
		for _, h := range a.AddHosts {
			kind := types.OverrideAgentHost
			if types.IsWildcardHost(h) {
				kind = types.OverrideAgentHostWildcard
			}
			out = append(out, OverrideItem{kind, types.OverrideAdd, h})
		}
		for _, h := range a.RemoveHosts {
			out = append(out, OverrideItem{types.OverrideAgentHost, types.OverrideRemove, h})
		}
		for _, s := range a.AddSecrets {
			out = append(out, OverrideItem{types.OverrideAgentSecret, types.OverrideAdd, s.SecretName})
		}
		for _, n := range a.RemoveSecrets {
			out = append(out, OverrideItem{types.OverrideAgentSecret, types.OverrideRemove, n})
		}
		for _, r := range a.ToolRules {
			kind := types.OverrideToolRuleRestrict
			if r.Effect == types.ToolAllow {
				kind = types.OverrideToolRuleAllow
			}
			out = append(out, OverrideItem{kind, types.OverrideAdd, r.Tool})
		}
	}
	if o.AzureDevOps != nil {
		out = append(out, OverrideItem{types.OverrideADOCapability, types.OverrideNarrow, "azure_devops.capabilities"})
	}
	for _, g := range o.GitPAT {
		if g.Repos != nil || g.Access == types.PATAccessRead || g.API != nil && !*g.API {
			out = append(out, OverrideItem{types.OverrideGitPATScope, types.OverrideNarrow, g.Host})
		}
		if g.Access == types.PATAccessWrite || g.API != nil && *g.API {
			out = append(out, OverrideItem{types.OverrideGitPATScope, types.OverrideAdd, g.Host})
		}
	}
	for _, p := range o.PushRules {
		for _, path := range p.DenyPaths {
			out = append(out, OverrideItem{types.OverridePushDeny, types.OverrideAdd, p.PushRuleKey() + " " + path})
		}
		for _, path := range p.RequireReviewPaths {
			out = append(out, OverrideItem{types.OverridePushReview, types.OverrideAdd, p.PushRuleKey() + " " + path})
		}
	}
	if h := o.Hosts; h != nil {
		for _, host := range h.AddAllowed {
			kind := types.OverrideRunHostAllow
			if types.IsWildcardHost(host) {
				kind = types.OverrideRunHostWildcard
			}
			out = append(out, OverrideItem{kind, types.OverrideAdd, host})
		}
		for _, host := range h.AddDenied {
			out = append(out, OverrideItem{types.OverrideRunHostDeny, types.OverrideAdd, host})
		}
	}
	if e := o.Egress; e != nil {
		if e.FirstUseApproval != "" {
			out = append(out, OverrideItem{types.OverrideFirstUseApproval, types.OverrideSet, string(e.FirstUseApproval)})
		}
		if e.AllowAllEgress != nil {
			out = append(out, OverrideItem{types.OverrideAllowAllEgress, types.OverrideSet, "allow_all_egress"})
		}
	}
	return out
}

// Pruned is o with every block that holds no edit dropped, so a record of what
// took effect carries no empty shell.
func (o RunOverrides) Pruned() RunOverrides {
	if a := o.Agent; a != nil && len(a.AddHosts)+len(a.RemoveHosts)+len(a.AddSecrets)+len(a.RemoveSecrets)+len(a.ToolRules) == 0 {
		o.Agent = nil
	}
	if h := o.Hosts; h != nil && len(h.AddAllowed)+len(h.AddDenied)+len(h.RemoveAllowed)+len(h.RemoveDenied) == 0 {
		o.Hosts = nil
	}
	if e := o.Egress; e != nil && e.FirstUseApproval == "" && e.AllowAllEgress == nil {
		o.Egress = nil
	}
	return o
}

// Empty reports whether a recorded diff holds no edit and no clamped edit.
func (d RunOverridesDiff) Empty() bool { return len(d.Pruned().Items()) == 0 && len(d.Clamped) == 0 }

// The kinds a LocalPlacementFact is about.
const (
	LocalPlacementHost      = "host"
	LocalPlacementSource    = "source"
	LocalPlacementComponent = "component"
)

// LocalPlacementFact says whether one entry of the run can be placed on the
// person's own runner. An entry with no row has not been evaluated; the
// answers come from the placement lane, so an older server sends none.
type LocalPlacementFact struct {
	// Kind is host, source or component; Key is the host, the workspace
	// source's id or the component's fact id.
	Kind string `json:"kind"`
	Key  string `json:"key"`
	// LocalPlaceable is false when the runner cannot honour the entry.
	LocalPlaceable bool `json:"local_placeable"`
	// Reason is the placement refusal reason that applies when it is false.
	Reason placement.Reason `json:"reason,omitempty"`
}

// ComponentSecretFact is one secret of a component: how it reaches the run,
// and whether the organisation provides it. Never its name.
type ComponentSecretFact struct {
	Delivery string `json:"delivery"` // header | env | file
	Shared   bool   `json:"shared"`
	// LocalDelivery is how an organisation-held secret would reach the person's
	// runner (OD-12): via_org, runner_resident or refuse. Absent for the
	// person's own secret and while the placement lane has not decided.
	LocalDelivery placement.Mode `json:"local_delivery,omitempty"`
}

// TokenScopeFact is one scope of the token a person creates for an Azure
// DevOps own-token sign-in, with the capabilities it covers (#1880).
type TokenScopeFact struct {
	Scope  string   `json:"scope"`
	Covers []string `json:"covers"`
}

// RepoAccessFact is the access a run has to one repository of a Git provider.
type RepoAccessFact struct {
	Repo   string `json:"repo"`
	Access string `json:"access"` // read | write
	// CanWrite is whether the person may raise a read repository to write.
	CanWrite bool `json:"can_write"`
}

// AgentModelProviderFact is the model provider the agent calls through: its
// name and kind, never its id.
type AgentModelProviderFact struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// AgentHostFact is a destination the agent reaches. Role is provider for the
// model provider's own lane and login for a sign-in host; the source chip comes
// from the response's provenance.
type AgentHostFact struct {
	Host string `json:"host"`
	Role string `json:"role"`
}

// AgentSecretFact is the credential the agent's model calls carry.
type AgentSecretFact struct {
	// Kind is key, token, aws or subscription.
	Kind string `json:"kind"`
	// Owner is own or shared.
	Owner string `json:"owner"`
	// Residency is proxy, sandbox or resolved_at_launch.
	Residency string `json:"residency"`
	// LocalDelivery is the OD-12 mode for an organisation-held one.
	LocalDelivery placement.Mode `json:"local_delivery,omitempty"`
}

// ManagedSettingsFact is the exact frozen managed-settings document Wardyn
// writes for the resolved autonomy level (OD-2). Read-only: Document is the
// bytes agentpolicy.ForAgent returns.
type ManagedSettingsFact struct {
	Path     string `json:"path"`
	Document string `json:"document"`
	// Harness names whose document this is: the started agent's, else the first harness of the set that
	// brings one, so a run that starts one harness can carry another's document.
	Harness string `json:"harness,omitempty"`
	// Locked is the B-P2 variant that adds the managed-only locks; LockedBy is
	// profile or organisation, and Profile names the profile.
	Locked   bool   `json:"locked,omitempty"`
	LockedBy string `json:"locked_by,omitempty"`
	Profile  string `json:"profile,omitempty"`
}

// TelemetryFact says whether agent telemetry is off and which variables Wardyn
// sets to turn it off.
type TelemetryFact struct {
	Off bool     `json:"off"`
	Env []string `json:"env,omitempty"`
}

// AgentFact is the agent component's fact (kind "agent"): the model provider
// it calls through, the destinations and secrets that lane carries, and the
// managed settings Wardyn writes for it.
type AgentFact struct {
	Agent           string                  `json:"agent"`
	ModelProvider   *AgentModelProviderFact `json:"model_provider,omitempty"`
	Hosts           []AgentHostFact         `json:"hosts"`
	Secrets         []AgentSecretFact       `json:"secrets"`
	ManagedSettings *ManagedSettingsFact    `json:"managed_settings,omitempty"`
	Telemetry       *TelemetryFact          `json:"telemetry,omitempty"`
}

// ComponentFact is one row of `components`: something the run is given access
// to, as the door decided it. It mirrors the server's fact; a refused request
// has no facts.
type ComponentFact struct {
	Kind         types.ComponentKind `json:"kind"`
	Provider     string              `json:"provider,omitempty"`
	ID           string              `json:"id"`
	Name         string              `json:"name,omitempty"`
	Version      int                 `json:"version,omitempty"`
	Reason       string              `json:"reason"`
	Status       string              `json:"status"`
	Requirements []PreflightItem     `json:"requirements"`

	Lane  string   `json:"lane,omitempty"`
	Org   string   `json:"org,omitempty"`
	Repos []string `json:"repos,omitempty"`

	Hosts        []string              `json:"hosts,omitempty"`
	Secrets      []ComponentSecretFact `json:"secrets,omitempty"`
	ConfigKeys   []string              `json:"config_keys,omitempty"`
	SelfDefined  bool                  `json:"self_defined,omitempty"`
	AutonomyCap  types.AutonomyLevel   `json:"autonomy_cap,omitempty"`
	VaultFloor   bool                  `json:"vault_floor,omitempty"`
	TLSIntercept bool                  `json:"tls_intercept,omitempty"`
	HighRisk     bool                  `json:"high_risk,omitempty"`

	// Agent is the agent component's own facts (kind agent).
	Agent *AgentFact `json:"agent,omitempty"`

	// A git_provider's additions. Capabilities is the run's resolved set on the
	// lane and CapabilityCeiling the most the person's row allows. PushRules are
	// the rules in force for this provider and organisation. TokenMode and
	// TokenScopes describe the Azure DevOps token; RepoAccess each repository's
	// access; InstallURL is the GitHub App install link, sent to operators only.
	Capabilities      []string             `json:"capabilities,omitempty"`
	CapabilityCeiling []string             `json:"capability_ceiling,omitempty"`
	PushRules         *types.PushRulesSpec `json:"push_rules,omitempty"`
	TokenMode         types.ADOTokenMode   `json:"token_mode,omitempty"`
	TokenScopes       []TokenScopeFact     `json:"token_scopes,omitempty"`
	RepoAccess        []RepoAccessFact     `json:"repo_access,omitempty"`
	InstallURL        string               `json:"install_url,omitempty"`
}

// The kinds of source a run's resolved image comes from.
const (
	ImageSourceBase       = "base"        // the agent base image, for a run with no harness CLI
	ImageSourceAgent      = "agent"       // the agent's own convention image
	ImageSourceHarnessSet = "harness_set" // an image the operator declared for several harnesses; Name is its key
	ImageSourceWorkspace  = "workspace"   // a workspace's base image; Name is the workspace
	ImageSourceBuild      = "build"       // the person's own build (a custom image or devcontainer)
	ImageSourceOrgAllowed = "org_allowed" // one of the images the organisation allows, chosen by the person
)

// ImageSource says where the resolved image comes from, so the Runner tab can
// say "from {source}". Name is the workspace, the build, the allowed entry, the
// harness (agent) or the declared set's key (harness_set); it is empty for base.
type ImageSource struct {
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`
}

// ImageFact is the image the run would start from, as the door resolved it. A
// door that has not resolved one sends none.
type ImageFact struct {
	Ref    string      `json:"ref"`
	Source ImageSource `json:"source"`
}

// HarnessSetFact is the run's included harnesses as this door resolved them.
// Absent for an older client's request and for a run with no harness.
type HarnessSetFact struct {
	Harnesses []HarnessFact `json:"harnesses"`
	// SharedModelAccess: two or more harnesses carry a model credential. The
	// proxy injects each by destination host, so any process in the sandbox can
	// use each one.
	SharedModelAccess bool `json:"shared_model_access"`
}

// HarnessFact is one included harness: whether it starts with the run, and the
// model credential it gets.
type HarnessFact struct {
	ID     string `json:"id"`
	Starts bool   `json:"starts"`
	// ModelCredential is this harness's provider, kind and residency; absent when
	// the harness takes none or its choice is still open.
	ModelCredential *HarnessModelCredential `json:"model_credential,omitempty"`
}

// HarnessModelCredential is one harness's model credential: its provider, the
// provider's kind, and where the credential lands (proxy or sandbox).
type HarnessModelCredential struct {
	Provider  string `json:"provider"`
	Kind      string `json:"kind"`
	Residency string `json:"residency"`
}

// AllowedImage is one image the organisation lets a person choose for a run
// (CreateRunRequest.AllowedImage). The list is empty until the image lane
// fills it.
type AllowedImage struct {
	Ref  string `json:"ref"`
	Name string `json:"name,omitempty"`
}

// ProvenanceSource identifies the source of one resolved policy entry.
// Unknown kinds must not be presented as a known source.
type ProvenanceSource struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// ProvenanceEntry explains one redaction-safe policy entry in preflight.
// Effect is added, clamped, narrowed or removed; readers tolerate future values.
type ProvenanceEntry struct {
	Field  string           `json:"field"`
	Value  string           `json:"value"`
	Source ProvenanceSource `json:"source"`
	Effect string           `json:"effect"`
}
