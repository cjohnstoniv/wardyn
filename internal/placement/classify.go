// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"reflect"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// Class is the credential class of a dispatched field (design §6.3).
type Class string

const (
	ClassOwn      Class = "own"      // the launching principal's own material
	ClassOperator Class = "operator" // the operator namespace, a shared component secret, or org network configuration
	ClassBrokered Class = "brokered" // minted by the org's broker from an org-held identity
	ClassPlatform Class = "platform" // minted for this run by the platform
	ClassExempt   Class = "exempt"   // not credential-bearing
	// ClassComposite marks a field that holds a classified struct of its own.
	ClassComposite Class = "composite"
)

// Rule is what local placement does with a field.
type Rule string

const (
	RuleAllow         Rule = "allow"
	RuleDelivery      Rule = "delivery"       // OD-12: the class's configured mode, else refuse
	RuleViaOrgRefuse  Rule = "via_org_refuse" // via_org or refuse, never runner_resident
	RuleRefuse        Rule = "refuse"         // refused with Entry.Reason
	RuleBound         Rule = "bound"          // allowed only bound to this runner and inside its roots
	RuleNotSent       Rule = "not_sent"       // never dispatched; the runner's own configuration applies
	RuleStrip         Rule = "strip"          // dropped or narrowed in the laptop-bound copy
	RuleExempt        Rule = "exempt"         // sent as dispatched
	RuleOwnerOnlyOwn  Rule = "owner_only_own" // allowed, persisted OwnerOnly=true
	RuleNotConfigured Rule = "not_configured" // refused, and not a configurable class
)

// The structs the table covers.
const (
	StructSandboxSpec = "SandboxSpec"
	StructProxyConfig = "ProxyConfig"
	// StructGrantKind names grant kinds with no SandboxSpec field, minted
	// through the proxy's local mint route.
	StructGrantKind = "GrantKind"
)

// Entry is one row of the classification table. Delivery names the OD-12 class
// key a refusal or mode applies to, when one does.
type Entry struct {
	Struct   string
	Field    string
	Path     string // dotted path under Field, "" for the field itself
	Variant  string // which case of the field this row covers
	Class    Class
	Rule     Rule
	Delivery string
	Reason   Reason // set when the rule refuses
}

// Table is the one classification table. Create, dispatch and revive all read
// it; a field not named here fails TestDispatchPlanFieldsClassified.
var Table = []Entry{
	// runner.SandboxSpec
	{Struct: StructSandboxSpec, Field: "SecretEnv", Variant: "env_secret grant, OwnerOnly", Class: ClassOwn, Rule: RuleOwnerOnlyOwn},
	{Struct: StructSandboxSpec, Field: "SecretEnv", Variant: "env_secret grant, not OwnerOnly", Class: ClassOperator, Rule: RuleDelivery, Delivery: ClassEnvSecret, Reason: ReasonPlacementCredential},
	{Struct: StructSandboxSpec, Field: "SecretEnv", Variant: "Bedrock role credentials, the person's own bedrock_sso", Class: ClassOwn, Rule: RuleOwnerOnlyOwn},
	{Struct: StructSandboxSpec, Field: "SecretEnv", Variant: "Bedrock role credentials, otherwise", Class: ClassOperator, Rule: RuleDelivery, Delivery: ClassBedrockRoleCreds, Reason: ReasonPlacementCredential},
	{Struct: StructSandboxSpec, Field: "ManagedFiles", Variant: "managed settings", Class: ClassPlatform, Rule: RuleAllow},
	{Struct: StructSandboxSpec, Field: "ManagedFiles", Variant: "AgentOwned file_secret, OwnerOnly", Class: ClassOwn, Rule: RuleOwnerOnlyOwn},
	{Struct: StructSandboxSpec, Field: "ManagedFiles", Variant: "AgentOwned file_secret, not OwnerOnly", Class: ClassOperator, Rule: RuleDelivery, Delivery: ClassFileSecret, Reason: ReasonPlacementCredential},
	{Struct: StructSandboxSpec, Field: "Mounts", Variant: "operator-authored, including host-mode ~/.aws", Class: ClassOperator, Rule: RuleRefuse, Reason: ReasonPlacementCapability},
	{Struct: StructSandboxSpec, Field: "Mounts", Variant: "MemberAuthored local_dir", Class: ClassOwn, Rule: RuleBound, Reason: ReasonPlacementLocalPath},
	{Struct: StructSandboxSpec, Field: "Drive", Variant: "host_path", Class: ClassOwn, Rule: RuleBound, Reason: ReasonPlacementLocalPath},
	{Struct: StructSandboxSpec, Field: "Drive", Variant: "k8s_pvc, k8s_pvc_static, the org's docker_volume", Class: ClassOperator, Rule: RuleRefuse, Reason: ReasonPlacementCapability},
	{Struct: StructSandboxSpec, Field: "Image", Variant: "any, including a workspace image in the org registry; pulled with the runner's own registry credentials, never the org's pull secrets", Class: ClassOwn, Rule: RuleAllow, Reason: ReasonPlacementCapability},
	{Struct: StructSandboxSpec, Field: "Env", Variant: "platform variables and the person's own or self-defined component config", Class: ClassExempt, Rule: RuleExempt},
	{Struct: StructSandboxSpec, Field: "Env", Variant: "an org component's config: operator-authored, so a value there may be a secret the org never meant to send", Class: ClassOperator, Rule: RuleStrip},
	{Struct: StructSandboxSpec, Field: "Labels", Class: ClassExempt, Rule: RuleExempt},
	{Struct: StructSandboxSpec, Field: "UserMountRoots", Class: ClassExempt, Rule: RuleExempt, Variant: "re-checked against allowed_roots"},
	{Struct: StructSandboxSpec, Field: "RunID", Class: ClassExempt, Rule: RuleExempt},
	{Struct: StructSandboxSpec, Field: "RunnerID", Class: ClassExempt, Rule: RuleExempt, Variant: "routing carrier"},
	{Struct: StructSandboxSpec, Field: "ConfinementClass", Class: ClassExempt, Rule: RuleExempt},
	{Struct: StructSandboxSpec, Field: "Resources", Class: ClassExempt, Rule: RuleExempt, Variant: "checked against the advertised capacity"},
	{Struct: StructSandboxSpec, Field: "Interactive", Class: ClassExempt, Rule: RuleExempt},
	{Struct: StructSandboxSpec, Field: "OnWaiting", Class: ClassExempt, Rule: RuleNotSent, Variant: "a callback; waiting events replace it"},
	{Struct: StructSandboxSpec, Field: "ExecOutput", Class: ClassExempt, Rule: RuleNotSent, Variant: "a writer; the output stream replaces it"},
	{Struct: StructSandboxSpec, Field: "ProxyConfig", Class: ClassComposite, Rule: RuleStrip, Variant: "classified field by field under ProxyConfig"},

	// runner.ProxyConfig
	{Struct: StructProxyConfig, Field: "RunToken", Class: ClassPlatform, Rule: RuleAllow, Variant: "bounded by the runner_id binding"},
	{Struct: StructProxyConfig, Field: "ControlPlaneCAPEM", Class: ClassPlatform, Rule: RuleAllow, Variant: "public"},
	{Struct: StructProxyConfig, Field: "MITMCACertPEM", Class: ClassPlatform, Rule: RuleAllow},
	{Struct: StructProxyConfig, Field: "MITMCAKeyPEM", Class: ClassPlatform, Rule: RuleAllow, Variant: "per run; gains MITM of one's own sandbox only"},
	{Struct: StructProxyConfig, Field: "Injection", Variant: "stored api_key, own namespace", Class: ClassOwn, Rule: RuleOwnerOnlyOwn},
	{Struct: StructProxyConfig, Field: "Injection", Variant: "operator namespace or shared component", Class: ClassOperator, Rule: RuleDelivery, Delivery: ClassAPIKey, Reason: ReasonPlacementCredential},
	{Struct: StructProxyConfig, Field: "Injection", Variant: "the person's model-provider key or own Claude sign-in", Class: ClassOwn, Rule: RuleAllow},
	{Struct: StructProxyConfig, Field: "Injection", Variant: "captured AWS SSO, credential_source per_user", Class: ClassOwn, Rule: RuleAllow},
	{Struct: StructProxyConfig, Field: "Injection", Variant: "captured AWS SSO shared", Class: ClassBrokered, Rule: RuleDelivery, Delivery: ClassAWSSSOBearer, Reason: ReasonPlacementCredential},
	{Struct: StructProxyConfig, Field: "Injection", Variant: "Bedrock bearer", Class: ClassBrokered, Rule: RuleDelivery, Delivery: ClassBedrockBearer, Reason: ReasonPlacementCredential},
	{Struct: StructProxyConfig, Field: "GitGrants", Variant: "GitHub App installation token", Class: ClassBrokered, Rule: RuleViaOrgRefuse, Delivery: ClassGitHubToken, Reason: ReasonPlacementCredential},
	{Struct: StructProxyConfig, Field: "PATGrants", Variant: "per_user stored PAT", Class: ClassOwn, Rule: RuleAllow},
	{Struct: StructProxyConfig, Field: "PATGrants", Variant: "shared CredentialSource", Class: ClassOperator, Rule: RuleDelivery, Delivery: ClassGitPATBroker, Reason: ReasonPlacementCredential},
	{Struct: StructProxyConfig, Field: "BrokeredPATGrantIDs", Variant: "follows PATGrants", Class: ClassOperator, Rule: RuleDelivery, Delivery: ClassGitPATBroker, Reason: ReasonPlacementCredential},
	{Struct: StructProxyConfig, Field: "ADOGrant", Variant: "minted PAT from a captured Entra sign-in", Class: ClassBrokered, Rule: RuleViaOrgRefuse, Delivery: ClassADOMintedPAT, Reason: ReasonPlacementCredential},
	{Struct: StructProxyConfig, Field: "ADOGrant", Variant: "own_pat token mode", Class: ClassOwn, Rule: RuleAllow},
	{Struct: StructProxyConfig, Field: "AzureGates", Variant: "Azure Foundry, the person's own Entra sign-in", Class: ClassOwn, Rule: RuleAllow},
	{Struct: StructProxyConfig, Field: "AzureGates", Variant: "Azure Foundry, any shared capture", Class: ClassBrokered, Rule: RuleDelivery, Delivery: ClassAzureFoundryToken, Reason: ReasonPlacementCredential},
	{Struct: StructProxyConfig, Field: "UpstreamProxyURL", Class: ClassOperator, Rule: RuleNotSent, Variant: "the runner's own upstream-proxy config applies"},
	{Struct: StructProxyConfig, Field: "TrustedCAPEM", Class: ClassOperator, Rule: RuleNotSent, Variant: "org network; destinations needing it take via_org"},
	{Struct: StructProxyConfig, Field: "InternalHosts", Class: ClassOperator, Rule: RuleNotSent, Variant: "org network"},
	{Struct: StructProxyConfig, Field: "UpstreamProxyNoProxy", Class: ClassOperator, Rule: RuleNotSent, Variant: "org network"},
	{Struct: StructProxyConfig, Field: "LLMUpstreams", Class: ClassOperator, Rule: RuleNotSent, Variant: "org network"},
	{Struct: StructProxyConfig, Field: "MITMHosts", Class: ClassOperator, Rule: RuleStrip, Variant: "only hosts of own and runner_resident injections and every via_org destination; operator-only hosts dropped"},
	{Struct: StructProxyConfig, Field: "Policy", Path: "EligibleGrants", Class: ClassOperator, Rule: RuleStrip, Variant: "grants not delivered own or runner_resident dropped; a via_org grant keeps only its grant_id and host"},
	{Struct: StructProxyConfig, Field: "Policy", Path: "LLMInspection.WorkspaceSecretNames", Class: ClassOperator, Rule: RuleStrip, Variant: "resolved values stripped; the org-side via_org scan may still use them"},
	{Struct: StructProxyConfig, Field: "Policy", Class: ClassExempt, Rule: RuleExempt, Variant: "the rest of Policy"},
	{Struct: StructProxyConfig, Field: "ControlPlaneURL", Class: ClassExempt, Rule: RuleExempt},
	{Struct: StructProxyConfig, Field: "MITMLLM", Class: ClassExempt, Rule: RuleExempt},
	{Struct: StructProxyConfig, Field: "LLMChannelHosts", Class: ClassExempt, Rule: RuleExempt},
	{Struct: StructProxyConfig, Field: "LLMUnavailableDetail", Class: ClassExempt, Rule: RuleExempt},
	{Struct: StructProxyConfig, Field: "Unattended", Class: ClassExempt, Rule: RuleExempt},
	{Struct: StructProxyConfig, Field: "Attribution", Class: ClassExempt, Rule: RuleExempt},

	// Grant kinds with no SandboxSpec field.
	{Struct: StructGrantKind, Field: "ssh_key", Variant: "own key", Class: ClassOwn, Rule: RuleAllow},
	{Struct: StructGrantKind, Field: "ssh_key", Variant: "operator key", Class: ClassOperator, Rule: RuleDelivery, Delivery: ClassSSHKey, Reason: ReasonPlacementCredential},
	{Struct: StructGrantKind, Field: "git_pat", Variant: "helper shape, own", Class: ClassOwn, Rule: RuleAllow},
	{Struct: StructGrantKind, Field: "git_pat", Variant: "helper shape, operator", Class: ClassOperator, Rule: RuleDelivery, Delivery: ClassGitPATHelper, Reason: ReasonPlacementCredential},
	{Struct: StructGrantKind, Field: "cloud_sts", Class: ClassBrokered, Rule: RuleNotConfigured, Delivery: ClassCloudSTS, Reason: ReasonPlacementCredential},
}

// Entries returns the rows covering one field of a struct.
func Entries(structName, field string) []Entry {
	var out []Entry
	for _, e := range Table {
		if e.Struct == structName && e.Field == field {
			out = append(out, e)
		}
	}
	return out
}

// Unclassified lists the exported fields of runner.SandboxSpec and
// runner.ProxyConfig that the table does not name, as "Struct.Field".
func Unclassified() []string {
	return slices.Concat(
		unclassifiedIn(StructSandboxSpec, reflect.TypeFor[runner.SandboxSpec]()),
		unclassifiedIn(StructProxyConfig, reflect.TypeFor[runner.ProxyConfig]()),
	)
}

func unclassifiedIn(name string, t reflect.Type) []string {
	var out []string
	for i := range t.NumField() {
		if f := t.Field(i); f.IsExported() && len(Entries(name, f.Name)) == 0 {
			out = append(out, name+"."+f.Name)
		}
	}
	return out
}

// pathExists reports whether a dotted path of field names resolves under t.
func pathExists(t reflect.Type, path string) bool {
	for _, seg := range strings.Split(path, ".") {
		for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Map {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return false
		}
		f, ok := t.FieldByName(seg)
		if !ok {
			return false
		}
		t = f.Type
	}
	return true
}
