// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"reflect"
	"slices"
	"strings"
)

// nestedFields pins every inspected dispatch carrier. A parent exemption never
// exempts a newly added child: both the table and this closed schema must cover it.
var nestedFields = map[reflect.Type][]string{
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
	reflect.TypeFor[types.RunPolicySpec]():     strings.Fields("AllowedDomains DeniedDomains AllowAllEgress FirstUseApproval FirstUseHoldSeconds MaxHolds AllowedMethods MinConfinementClass EligibleGrants AutoStopAfterSec WorkspaceMounts WorkspaceRepos LLMInspection UIApps Resources ToolRules GitPushAnyBranch PushRules AzureDevOpsCapabilities GitHubCapabilities"),
	reflect.TypeFor[types.ToolRule]():          strings.Fields("Tool Effect"),
	reflect.TypeFor[types.UIApp]():             strings.Fields("Name Port Path"),
	reflect.TypeFor[types.WorkspaceMount]():    strings.Fields("Source Target ReadOnly"),
	reflect.TypeFor[types.WorkspaceRepo]():     strings.Fields("Repo Target Ref"),
}

// NestedUnclassified checks types even when the current value is nil or empty.
func NestedUnclassified() []string {
	return nestedUnclassified(reflect.TypeFor[runner.SandboxSpec](), StructSandboxSpec, 0)
}

func nestedUnclassified(t reflect.Type, path string, depth int) []string {
	if depth > 32 {
		return []string{path}
	}
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	var out []string
	fields, known := nestedFields[t]
	root := t == reflect.TypeFor[runner.SandboxSpec]() || t == reflect.TypeFor[runner.ProxyConfig]()
	if !root && !known {
		return []string{path}
	}
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		child := path + "." + f.Name
		if !root && !slices.Contains(fields, f.Name) {
			out = append(out, child)
			continue
		}
		out = append(out, nestedUnclassified(f.Type, child, depth+1)...)
	}
	return out
}
