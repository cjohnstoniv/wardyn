// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// localDispatchFields pins authoring metadata as well as the actual wire spec.
// Nil/empty child carriers are inspected too. A new provider, grant, config or
// control field therefore needs an explicit classification before local use.
var localDispatchFields = map[reflect.Type][]string{
	reflect.TypeFor[providerGrantSnapshot](): strings.Fields("ProviderUID OwnerSubject"),
	reflect.TypeFor[azureGrantSnapshot]():    strings.Fields("ProviderUID OwnerSubject Audience"),
	reflect.TypeFor[awsSSOScopeSnapshot]():   strings.Fields("OwnerSubject CredentialSource Mechanism SSOAccountID SSORoleName Region ProviderUID"),
	reflect.TypeFor[adoEntraScopeSnapshot](): strings.Fields("ProviderRowID Organisation OwnerSubject CredentialSource TenantID ClientID TokenMode Capabilities"),

	reflect.TypeFor[time.Time]():                 nil, // opaque timestamp, not credential authority
	reflect.TypeFor[adoEntraGrade]():             strings.Fields("graded rowID org tokenMode"),
	reflect.TypeFor[adoEntraRun]():               strings.Fields("rowID org owner tenantID clientID tokenMode credentialSource caps capsFromPolicy ceiling patHours serverHost"),
	reflect.TypeFor[adoStandingBound]():          strings.Fields("operator governance"),
	reflect.TypeFor[bedrockAuth]():               strings.Fields("env egressHosts ready bearer runtimeHost runtimePort region model ssoInject ssoAccountID ssoRoleName ssoRegion ssoProxyInject maskErr"),
	reflect.TypeFor[bedrockCredGrade]():          strings.Fields("graded host"),
	reflect.TypeFor[bedrockTransportAudit]():     strings.Fields("region model endpoint mode detail hosts"),
	reflect.TypeFor[chosenProvider]():            strings.Fields("provider owner"),
	reflect.TypeFor[componentConfig]():           strings.Fields("ordinal source selfDefined env"),
	reflect.TypeFor[componentDispatch]():         strings.Fields("HeaderHosts MITMHosts Config"),
	reflect.TypeFor[dispatchCeiling]():           strings.Fields("localSelfDefinedComponents resolved deny profile maxEphemeralDiskMiB maxCPUMillis maxMemoryMiB adoEntra bedrock adoStanding agentGuardrailLocks"),
	reflect.TypeFor[dispatchParams]():            strings.Fields("TrustedOutput RunToken Image Policy FirstGitHubGrantID GitGrants PATBroker PATAPI GitPATGrants SSHGrants Injections Interactive TaskMode InteractiveStart SeedAutoTools ToolApprovals ApprovalExpiryAfter ExtraEnv Toolchains UserMounts EphemeralDirs Drive Components"),
	reflect.TypeFor[llmTransport]():              strings.Fields("bedrock bedrockReady injectBedrockBearer injectBedrockSSO azure provider secretEnvKeys bedrockAudit"),
	reflect.TypeFor[providerAzureLane]():         strings.Fields("provider owner harness endpoint host route audience model fast"),
	reflect.TypeFor[toolchainNeeds]():            strings.Fields("goTools jvmTools"),
	reflect.TypeFor[userMountPosture]():          strings.Fields("Roots Sources"),
	reflect.TypeFor[policyref.Contact]():         strings.Fields("Owner Email RequestURL RequestText"),
	reflect.TypeFor[types.ADOEntraConfig]():      strings.Fields("TenantID ClientID CapabilityCeiling DefaultProfile TokenMode PATMaxHours PATMaxDays RESTAPI"),
	reflect.TypeFor[types.AgentProvider]():       strings.Fields("ID Disabled DefaultProvider"),
	reflect.TypeFor[types.AgentProviders]():      strings.Fields("Agents"),
	reflect.TypeFor[types.ArtifactOverride]():    strings.Fields("BaseURL TokenSecretRef"),
	reflect.TypeFor[types.AzureSettings]():       strings.Fields("Endpoint Route"),
	reflect.TypeFor[types.BedrockSettings]():     strings.Fields("Region BaseURL SSOStartURL SSOAccountID SSORoleName"),
	reflect.TypeFor[types.ComponentSettings]():   strings.Fields("RequireVaultForCredentials DenyResidentDelivery AutonomyCap"),
	reflect.TypeFor[types.EgressRedirect]():      strings.Fields("From To TokenSecretRef TokenIntegrationRef Ecosystem"),
	reflect.TypeFor[types.EphemeralProvider]():   strings.Fields("DefaultDiskMiB MaxDiskMiB"),
	reflect.TypeFor[types.GitProvider]():         strings.Fields("ID Kind Disabled BaseURLs Lanes GitHubAppInstallURL CredentialSource Entra"),
	reflect.TypeFor[types.Integration]():         strings.Fields("ID Name Kind Disabled Secrets Egress Config Docs DisabledCapabilities CreatedAt UpdatedAt legacyTopology"),
	reflect.TypeFor[types.IntegrationDelivery](): strings.Fields("Mode Header Format Path Var"),
	reflect.TypeFor[types.IntegrationSecret]():   strings.Fields("Role SecretName Delivery"),
	reflect.TypeFor[types.ModelProvider]():       strings.Fields("ID UID Name Kind Disabled BaseURL Auth Bedrock Azure Harnesses"),
	reflect.TypeFor[types.ModelProviders]():      strings.Fields("Providers"),
	reflect.TypeFor[types.ProviderAuth]():        strings.Fields("Header Format"),
	reflect.TypeFor[types.ProviderHarness]():     strings.Fields("Harness Model FastModel Path AuthHeader AuthFormat"),
	reflect.TypeFor[types.RunnerSettings]():      strings.Fields("Enabled"),
	reflect.TypeFor[types.SiteBranding]():        strings.Fields("LogoPath"),
	reflect.TypeFor[types.SiteConfig]():          strings.Fields("Runners UpstreamProxySecretRef UpstreamProxyURL UpstreamProxyNoProxy ArtifactOverrides EgressRedirects ScmHosts Integrations InternalHosts Egress WorkspaceProviders AgentProviders ModelProviders SignInHelpText SignInHelpURL PolicyHelp Branding Components EffectiveScmHosts WithheldScmHosts OnboardingCompletedAt"),
	reflect.TypeFor[types.SiteEgress]():          strings.Fields("BaselineHosts"),
	reflect.TypeFor[types.StorageProviders]():    strings.Fields("Ephemeral UserDrive"),
	reflect.TypeFor[types.UserDriveProvider]():   strings.Fields("Disabled MaxSizeMiB"),
	reflect.TypeFor[types.WithheldScmHost]():     strings.Fields("Host ProviderID ProviderKind"),
	reflect.TypeFor[types.WorkspaceProviders]():  strings.Fields("Git Storage GitPatBrokerEnabled"),
}

func localDispatchUnclassified() []string {
	return slices.Concat(
		placement.SchemaUnclassified(reflect.TypeFor[providerGrantSnapshot](), "providerGrantSnapshot", localDispatchFields),
		placement.SchemaUnclassified(reflect.TypeFor[azureGrantSnapshot](), "azureGrantSnapshot", localDispatchFields),
		placement.SchemaUnclassified(reflect.TypeFor[awsSSOScopeSnapshot](), "awsSSOScopeSnapshot", localDispatchFields),
		placement.SchemaUnclassified(reflect.TypeFor[adoEntraScopeSnapshot](), "adoEntraScopeSnapshot", localDispatchFields),
		placement.SchemaUnclassified(reflect.TypeFor[dispatchParams](), "dispatchParams", localDispatchFields),
		placement.SchemaUnclassified(reflect.TypeFor[dispatchCeiling](), "dispatchCeiling", localDispatchFields),
		placement.SchemaUnclassified(reflect.TypeFor[llmTransport](), "llmTransport", localDispatchFields),
		placement.SchemaUnclassified(reflect.TypeFor[adoEntraRun](), "adoEntraRun", localDispatchFields),
		placement.SchemaUnclassified(reflect.TypeFor[types.SiteConfig](), "SiteConfig", localDispatchFields),
	)
}
