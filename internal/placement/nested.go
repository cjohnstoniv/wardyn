// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// nestedFields pins every inspected dispatch carrier. A parent exemption never
// exempts a newly added child: both the table and this closed schema must cover it.
var nestedFields = map[reflect.Type][]string{
	reflect.TypeFor[proxy.Config](): strings.Fields("RunID ControlPlaneURL ControlPlaneCAPEM RunToken Policy Injection Listen DecisionBufferSize MITMCACertPEM MITMCAKeyPEM MITMHosts GitGrants PATGrants " +
		"BrokeredPATGrantIDs ADOGrant AzureGates MITMLLM UpstreamProxyURL UpstreamProxyNoProxy TrustedCAPEM InternalHosts LLMUpstreams LLMChannelHosts LLMUnavailableDetail Unattended Attribution"),
	reflect.TypeFor[proxy.InjectionConfig]():   strings.Fields("InjectionRule GrantID"),
	reflect.TypeFor[LocalPlan]():               strings.Fields("Spec Origins CredentialIntents OrgConfigKeys VerifiedLocalPaths UpstreamProxySecretRef TrustedOutput SelfDefinedComponents LocalSelfDefinedComponents Delivery"),
	reflect.TypeFor[CredentialOrigin]():        strings.Fields("Class Delivery GrantKind Stored OwnNamespace OwnerOnly"),
	reflect.TypeFor[CredentialIntent]():        strings.Fields("Field Origin"),
	reflect.TypeFor[DeliveryPolicy]():          strings.Fields("Classes"),
	reflect.TypeFor[ClassPolicy]():             strings.Fields("Mode RequirePosture"),
	reflect.TypeFor[PostureRequirement]():      strings.Fields("MDMManaged DiskEncrypted OSMin"),
	reflect.TypeFor[egress.InjectionRule]():    strings.Fields("Host Header SecretName Format RequireTLS PinPath PinQuery PinRoutes"),
	reflect.TypeFor[egress.PinRoute]():         strings.Fields("Method Path"),
	reflect.TypeFor[policyref.Ref]():           strings.Fields("Source Name Owner Email RequestURL RequestText"),
	reflect.TypeFor[proxy.ADOGrantConfig]():    strings.Fields("Organization Capabilities Hosts"),
	reflect.TypeFor[proxy.AzureGateConfig]():   strings.Fields("Host Route Models BodyCap"),
	reflect.TypeFor[proxy.PATGrant]():          strings.Fields("GrantID Username Repos Access Forge API"),
	reflect.TypeFor[runner.InjectionGrant]():   strings.Fields("GrantID Rule"),
	reflect.TypeFor[runner.ManagedFile]():      strings.Fields("Path Mode AgentOwned Content"),
	reflect.TypeFor[runner.Mount]():            strings.Fields("Source Target ReadOnly MemberAuthored DriveAuthored"),
	reflect.TypeFor[runner.Resources]():        strings.Fields("CPUMillis MemoryMiB CPURequestMillis MemoryRequestMiB PidsLimit DiskMiB DiskMiBFilled"),
	reflect.TypeFor[types.DriveMount]():        strings.Fields("DriveID Backend ObjectName HostRoot DriveName StorageClass HomeName SubjectHash Target ReadOnly SizeMiB Enforcement"),
	reflect.TypeFor[types.GrantSpec]():         strings.Fields("Kind Scope TTLSeconds RequiresApproval OwnerOnly"),
	reflect.TypeFor[types.InternalHost]():      strings.Fields("HostSuffix CIDRs Baseline"),
	reflect.TypeFor[types.LLMInspectionSpec](): strings.Fields("Mode WorkspaceSecretNames WorkspaceSecretValues DetectSecrets DetectSecretPatterns DetectEntropy DetectPII DetectorSidecarURL ClassifiedMarkers ScanAttachments InspectForwardEgress MaxScanBytes OnScannerError RequireInspectableLLM InterceptTLS BlockMinSeverity"),
	reflect.TypeFor[types.PushRulesSpec]():     strings.Fields("DenyPaths MaxInspectPackMiB RequireReviewPaths HoldSeconds DenyNewExecutables MaxFileSizeMiB"),
	reflect.TypeFor[types.ResourceLimits]():    strings.Fields("CPUMillis MemoryMiB PidsLimit DiskMiB"),
	reflect.TypeFor[types.RunPolicySpec](): strings.Fields("AllowedDomains DeniedDomains AllowAllEgress FirstUseApproval FirstUseHoldSeconds MaxHolds AllowedMethods MinConfinementClass EligibleGrants AutoStopAfterSec " +
		"WorkspaceMounts WorkspaceRepos LLMInspection UIApps Resources ToolRules GitPushAnyBranch PushRules AzureDevOpsCapabilities GitHubCapabilities"),
	reflect.TypeFor[types.ToolRule]():       strings.Fields("Tool Effect"),
	reflect.TypeFor[types.UIApp]():          strings.Fields("Name Port Path"),
	reflect.TypeFor[types.WorkspaceMount](): strings.Fields("Source Target ReadOnly"),
	reflect.TypeFor[types.WorkspaceRepo]():  strings.Fields("Repo Target Ref"),
}

// NestedUnclassified checks types even when the current value is nil or empty.
func NestedUnclassified() []string {
	return slices.Concat(
		nestedUnclassified(reflect.TypeFor[runner.SandboxSpec](), StructSandboxSpec, 0),
		nestedUnclassified(reflect.TypeFor[proxy.Config](), "StoredProxyConfig", 0),
		nestedUnclassified(reflect.TypeFor[CredentialOrigin](), "CredentialOrigin", 0),
		nestedUnclassified(reflect.TypeFor[CredentialIntent](), "CredentialIntent", 0),
		nestedUnclassified(reflect.TypeFor[DeliveryPolicy](), "DeliveryPolicy", 0),
		nestedUnclassified(reflect.TypeFor[LocalPlan](), "LocalPlan", 0),
	)
}

// SchemaUnclassified shares the closed schema with private control-plane
// dispatch metadata. The additional entries are explicitly reviewed field
// manifests; nil marks an opaque scalar such as time.Time, never a parent
// exemption for future fields.
func SchemaUnclassified(t reflect.Type, path string, additional map[reflect.Type][]string, opaque map[string]string) []string {
	return schemaUnclassified(t, path, 0, additional, mergeOpaque(opaque))
}

func nestedUnclassified(t reflect.Type, path string, depth int) []string {
	return schemaUnclassified(t, path, depth, nil, opaqueFields)
}

func schemaUnclassified(t reflect.Type, path string, depth int, additional map[reflect.Type][]string, opaque map[string]string) []string {
	if depth > 32 {
		return []string{path}
	}
	var out []string
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		if t.Kind() == reflect.Map {
			out = append(out, schemaUnclassified(t.Key(), path+"[key]", depth+1, additional, opaque)...)
		}
		t = t.Elem()
	}
	// A value of unknown dynamic type cannot be walked; opaqueFields names the
	// few fields that are not data, checked where the field is declared.
	if t.Kind() == reflect.Interface || t.Kind() == reflect.Func {
		return append(out, path)
	}
	if t.Kind() != reflect.Struct {
		return out
	}
	fields, known := nestedFields[t]
	if explicit, ok := additional[t]; ok {
		fields, known = explicit, true
		if explicit == nil {
			return out
		}
	}
	root := tableRoots[t]
	if !root && !known {
		return append(out, path)
	}
	for i := range t.NumField() {
		f := t.Field(i)
		// An unexported embedded struct still promotes its exported fields to JSON.
		if !f.IsExported() && !f.Anonymous && additional == nil {
			continue
		}
		child := path + "." + f.Name
		if _, ok := opaque[t.Name()+"."+f.Name]; ok {
			continue
		}
		if !slices.Contains(fields, f.Name) && (!root || (!f.IsExported() && f.Anonymous)) {
			out = append(out, child)
			continue
		}
		out = append(out, schemaUnclassified(f.Type, child, depth+1, additional, opaque)...)
	}
	return out
}

// tableRoots are the structs whose fields the table classifies one by one; a
// child of anything else must also be listed in a manifest.
var tableRoots = map[reflect.Type]bool{reflect.TypeFor[runner.SandboxSpec](): true, reflect.TypeFor[runner.ProxyConfig](): true}

// opaqueFields names the interface and func fields that are not data, as
// "Type.Field", with why. Nothing else of such a kind passes.
var opaqueFields = map[string]string{
	"SandboxSpec.OnWaiting":  "a callback, tagged json:\"-\"; waiting events replace it",
	"SandboxSpec.ExecOutput": "a writer, tagged json:\"-\"; the output stream replaces it",
}

func mergeOpaque(extra map[string]string) map[string]string {
	all := maps.Clone(opaqueFields)
	maps.Copy(all, extra)
	return all
}
