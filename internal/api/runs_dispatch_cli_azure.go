// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/armscope"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The Azure CLI arm. An org row of cli.kind azure gives the run's `az` the
// person's own Entra sign-in for Azure Resource Manager. The sandbox holds a
// profile listing the row's subscriptions and a token cache whose one access
// token is an inert sentinel; the proxy's management.azure.com door
// (internal/egress/proxy/azure_arm_door.go) holds each request to the row's
// subscriptions and access, and adds the token the control plane redeems
// (injection_azure_cli.go). No path here or anywhere else writes that token,
// or the sign-in behind it, into the sandbox.

// azureCLIMechanism is the grant snapshot's and the sign-in hold's name for
// this lane. It is no other lane's, so neither Entra capture resolves, or is
// resolved by, the other's rows.
const azureCLIMechanism = "azure_cli"

const (
	// azureCLIConfigEnvVar carries the generated ~/.azure files, in the shape
	// awsSSOConfigEnvVar carries ~/.aws (agent-run-lib.sh materialises both).
	azureCLIConfigEnvVar = "WARDYN_AZURE_CLI_CONFIG_B64"
	// envAzureTelemetry turns the CLI's usage telemetry off: it would be a
	// request to a host the run was never given.
	envAzureTelemetry = "AZURE_CORE_COLLECT_TELEMETRY"

	// azureCLITokenSentinel is the cache's access token: it authenticates
	// nothing, and the door replaces it on every request it forwards.
	azureCLITokenSentinel = "wardyn-proxy-injects-azure-token"

	// The cache is keyed by what the CLI itself would have stored: its own
	// public client id and the scope it asks Resource Manager tokens for. The
	// format is the one recorded from az 2.91.0 (msal 1.39.0) on Linux.
	azureCLIClientID   = "04b07795-8ddb-461a-bbee-02f9e1bf7b46"
	azureCLICacheScope = "https://management.core.windows.net//.default"
	// azureCLICacheExpiry is 2100-01-01: the library renews an entry that has
	// expired, and a renewal is a token request the door refuses.
	azureCLICacheExpiry = "4102444800"
	// The account the profile and the cache agree on. The CLI looks its cache
	// entry up by this name; neither value is sent to Azure.
	azureCLIAccountID   = "00000000-0000-0000-0000-000000000000"
	azureCLIAccountName = "wardyn"

	azureCLIHTTPSPort   = "443"
	azureCLIGrantTTLSec = 3600 // bounds the mint, never the hold: a re-resolve redeems afresh
)

// DRAFT (canon pending). Member-visible sentences of the arm.
const (
	azureCLISecondRowRefusal = "a run can attach one Azure CLI component; remove the second"
	azureCLINoEntraRefusal   = "This run uses the Azure CLI, which signs you in through this deployment's Microsoft Entra console sign-in, " +
		"and none is configured. Remove the Azure CLI from the run, or ask your admin."
	azureCLINotPersonRefusal = "This run uses the Azure CLI, which uses your own Azure sign-in, and the admin token is a shared " +
		"credential rather than a person. Sign in to the console and start the run as yourself."
	azureCLIServiceIdentityRefusal = "This run uses the Azure CLI, which uses a person's own Azure sign-in, and a service identity has none. " +
		"Remove the Azure CLI from the run, or have a person start it."
	azureCLINotSignedIn = "This run uses the Azure CLI, and you are not signed in to Azure for it. " +
		"Sign in to Azure for the component, then start the run again."
	azureCLINoCertificateAuthority = "This run was not launched: its Azure sign-in needs a per-run certificate authority to be added on the wire, " +
		"and none was provisioned. Without one the proxy could not check or credential the Azure CLI's requests."
)

// azureCLIDispatch is the Azure CLI row a run attached, as the gate admitted it.
type azureCLIDispatch struct {
	ComponentID string
	Azure       types.CLIAzure
}

// azureCLI is the run's Azure CLI row, nil when it attached none. The gate
// admits at most one, and only an org row.
func (c runComponents) azureCLI() *azureCLIDispatch {
	for _, a := range c.attached {
		if az := attachedAzureCLI(a); az != nil {
			return &azureCLIDispatch{ComponentID: a.snapshot.ComponentID.String(), Azure: *az}
		}
	}
	return nil
}

// attachedAzureCLI is an attached org row's Azure CLI block, nil for anything else.
func attachedAzureCLI(a attachedComponent) *types.CLIAzure {
	cli := a.snapshot.Definition.CLI
	if a.snapshot.SelfDefined || a.snapshot.ComponentID == nil || cli == nil || cli.Kind != types.CLIKindAzure {
		return nil
	}
	return cli.Azure
}

// azureCLIRow reads the live org row id names as an Azure CLI row; ok=false
// when there is none: no such row, another kind, or a store that cannot say.
func (s *Server) azureCLIRow(ctx context.Context, id uuid.UUID) (types.CLIAzure, bool) {
	cs, ok := s.cfg.Store.(store.ComponentStore)
	if !ok {
		return types.CLIAzure{}, false
	}
	row, err := cs.GetComponent(ctx, id, "")
	if err != nil {
		return types.CLIAzure{}, false
	}
	az := attachedAzureCLI(attachedComponent{snapshot: types.RunComponent{ComponentID: &id, Definition: row.Definition}})
	if az == nil {
		return types.CLIAzure{}, false
	}
	return *az, true
}

// azureCLISubscriptions is the row's subscriptions as the gate compares them:
// lowercase and sorted, so two rows that pin the same set compare equal.
func azureCLISubscriptions(az types.CLIAzure) []string {
	return slices.Sorted(slices.Values(az.SubscriptionIDs()))
}

// azureARMGrantSnapshot is what dispatch records on the lane's grant: whose
// sign-in it is, for which row, in which tenant, for which audience, and the
// subscriptions and access the run's proxy was configured with. It grants
// nothing: the sink requires the owner to be the run token's own subject and
// re-derives the row, and refuses on any difference rather than serve a
// running sandbox a token for a row that has since changed.
type azureARMGrantSnapshot struct {
	Mechanism     string   `json:"mechanism"`
	ComponentID   string   `json:"component_id"`
	OwnerSubject  string   `json:"owner_subject"`
	TenantID      string   `json:"tenant_id"`
	Audience      string   `json:"audience"`
	Subscriptions []string `json:"subscriptions"`
	Access        string   `json:"access"`
}

func (sn azureARMGrantSnapshot) authored() bool {
	return sn.Mechanism == azureCLIMechanism && sn.ComponentID != "" && sn.OwnerSubject != "" && sn.TenantID != "" &&
		sn.Audience == azureARMAudience && len(sn.Subscriptions) > 0
}

// azureCLIEntra is the console's own Entra sign-in application, keyed to one
// Azure CLI row. ok=false when this deployment's console does not sign people
// in through Entra, or the application it names is not usable: only that
// application's sign-in can be bound to a session (ado_entra.go).
func (s *Server) azureCLIEntra(componentID string) (ADOEntraConfig, bool) {
	if s.cfg.AzureCLIEntra == nil {
		return ADOEntraConfig{}, false
	}
	cfg := s.cfg.AzureCLIEntra(componentID)
	if cfg.validateApp() != nil || !cfg.isLoginApplication() {
		return ADOEntraConfig{}, false
	}
	if _, err := cfg.authority(); err != nil {
		return ADOEntraConfig{}, false
	}
	return cfg, true
}

// azureCLISignedIn is a dry check that owner's own sign-in for the row can
// serve a run: stored, not known to have ended, and covering Resource Manager.
// It spends no refresh token; a sign-in only a renewal would find dead
// surfaces at the proxy's boot. An unreadable sign-in is an error, never "not
// signed in". The caller marks ctx with the read's purpose: a status read at
// the run doors, a dispatch read at dispatch.
func (s *Server) azureCLISignedIn(ctx context.Context, owner, componentID string) (bool, error) {
	ec, err := azureARMCapture(componentID)
	if err != nil {
		return false, nil
	}
	blob, found, err := s.readEntraBlob(ctx, owner, ec)
	if err != nil {
		return false, err
	}
	if !found || blob.signInEnded() {
		return false, nil
	}
	_, covered := ec.consentCovers(blob.Scopes, ec.scopes)
	return covered, nil
}

// azureCLIFiles is the ~/.azure a run's CLI reads: the profile listing the
// row's subscriptions, and a token cache holding one Resource Manager access
// token whose secret is the sentinel, with no refresh token and no id token.
// It takes no credential, so it can write none.
func azureCLIFiles(tenantID string, subscriptions []string, now time.Time) map[string]string {
	account := map[string]any{"name": azureCLIAccountName, "type": "user"}
	subs := make([]map[string]any, 0, len(subscriptions))
	for i, id := range subscriptions {
		subs = append(subs, map[string]any{
			"id": id, "name": id, "state": "Enabled", "isDefault": i == 0,
			"tenantId": tenantID, "homeTenantId": tenantID, "environmentName": "AzureCloud",
			"user": account, "managedByTenants": []string{},
		})
	}
	home := azureCLIAccountID + "." + tenantID
	// The library looks entries up by these lowercased keys.
	accountKey := strings.ToLower(home + "-" + proxy.AzureLoginHost + "-" + tenantID)
	tokenKey := strings.ToLower(home + "-" + proxy.AzureLoginHost + "-accesstoken-" + azureCLIClientID + "-" + tenantID + "-" + azureCLICacheScope)
	cache := map[string]any{
		"Account": map[string]any{accountKey: map[string]string{
			"home_account_id": home, "environment": proxy.AzureLoginHost, "realm": tenantID,
			"local_account_id": azureCLIAccountID, "username": azureCLIAccountName, "authority_type": "MSSTS",
		}},
		// No refresh_on: one in the past makes the library renew.
		"AccessToken": map[string]any{tokenKey: map[string]string{
			"credential_type": "AccessToken", "secret": azureCLITokenSentinel, "home_account_id": home,
			"environment": proxy.AzureLoginHost, "client_id": azureCLIClientID, "target": azureCLICacheScope,
			"realm": tenantID, "token_type": "Bearer", "cached_at": strconv.FormatInt(now.Unix(), 10),
			"expires_on": azureCLICacheExpiry, "extended_expires_on": azureCLICacheExpiry,
		}},
		"IdToken": map[string]any{}, "RefreshToken": map[string]any{}, "AppMetadata": map[string]any{},
	}
	return map[string]string{
		".azure/azureProfile.json":     string(mustJSON(map[string]any{"subscriptions": subs})),
		".azure/msal_token_cache.json": string(mustJSON(cache)),
	}
}

// azureCLIPlan is what the lane adds to one run's sidecar configuration.
type azureCLIPlan struct {
	gate *proxy.AzureARMGrant
	// opened is the allowlist entries the lane itself added for its door, and
	// only those: an entry the run's policy already carried is not one.
	opened []string
}

// gateFor is the door's grant while its credential is still one of the run's
// injections. A later phase can withhold it (the governance ceiling denies the
// host): the door goes with it, since the proxy refuses a door with no
// credential behind it, and the host stays denied.
func (pl azureCLIPlan) gateFor(injections []runner.InjectionGrant) *proxy.AzureARMGrant {
	if !slices.ContainsFunc(injections, func(in runner.InjectionGrant) bool { return hostEqual(in.Rule.Host, armscope.Host) }) {
		return nil
	}
	return pl.gate
}

// closeWithheld takes back the allowlist entries the lane opened when a later
// phase withheld its credential, and so its door. Both hosts are safe to allow
// only behind the door: without it the proxy would treat the Entra sign-in
// host as an ordinary allowed host the run's own policy never listed. An entry
// the policy carried on its own is not the lane's and stays.
func (pl azureCLIPlan) closeWithheld(policy *types.RunPolicySpec, injections []runner.InjectionGrant) {
	if pl.gate == nil || pl.gateFor(injections) != nil {
		return
	}
	policy.AllowedDomains = slices.DeleteFunc(slices.Clone(policy.AllowedDomains), func(entry string) bool {
		return slices.Contains(pl.opened, entry)
	})
}

// azureCLIReviveRefusal refuses the revive of a run that has the door once its
// owner's profile denies Resource Manager. Dispatch answers that deny with
// closeWithheld, which needs the entries the lane opened, and a stored proxy
// config does not say which of its allowlist entries those were. Without them a
// revive has no safe narrowing: dropping the credential alone leaves a door the
// proxy refuses to start with, and dropping the door as well leaves the Entra
// sign-in host an ordinary allowed host. A run dispatched under the deny has no
// door and revives as any run does.
func azureCLIReviveRefusal(cfg *proxy.Config, deny []string) *reviveError {
	if cfg.AzureARM == nil || !ceilingDenies(deny, armscope.Host) {
		return nil
	}
	return reviveRefused(http.StatusConflict, reasonReviveCeilingDeniesAzureCLI,
		"the owner's governance profile now denies Azure Resource Manager, which this run's Azure CLI needs; start a new run")
}

// authorAzureCLI stages the run's Azure CLI row: the profile and the inert
// cache in the sandbox, the two hosts on its allowlist, and the grant and door
// that put the run owner's token on the wire. A run with no per-run
// certificate authority, no console Entra sign-in, or whose owner is not
// signed in for the row is REFUSED rather than degraded: the only other way to
// serve the CLI is a real token in the sandbox. ok=false: the run was marked
// FAILED and dispatch stops.
func (s *Server) authorAzureCLI(ctx context.Context, run types.AgentRun, cli *azureCLIDispatch, caCertPEM, caKeyPEM string,
	policy *types.RunPolicySpec, sandboxEnv map[string]string, injections []runner.InjectionGrant,
) ([]runner.InjectionGrant, azureCLIPlan, bool) {
	if cli == nil {
		return injections, azureCLIPlan{}, true
	}
	refuse := func(msg string) ([]runner.InjectionGrant, azureCLIPlan, bool) {
		s.failAndRevoke(ctx, run.ID, types.RunStarting, msg)
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"error": msg, "component_id": cli.ComponentID})))
		return injections, azureCLIPlan{}, false
	}
	owner := runIdentitySubject(ctx, run.CreatedBy)
	cfg, configured := s.azureCLIEntra(cli.ComponentID)
	switch {
	case caCertPEM == "" || caKeyPEM == "":
		return refuse(azureCLINoCertificateAuthority)
	case !configured:
		return refuse(azureCLINoEntraRefusal)
	case !s.credentialPerson(owner):
		return refuse(azureCLINotPersonRefusal)
	case isServicePrincipal(owner):
		return refuse(azureCLIServiceIdentityRefusal)
	}
	if signedIn, err := s.azureCLISignedIn(secretstore.WithPurpose(ctx, secretstore.PurposeDispatch), owner, cli.ComponentID); err != nil {
		return refuse("Wardyn couldn't read your Azure sign-in for this run's Azure CLI just now — nothing was started. Try again in a moment.")
	} else if !signedIn {
		return refuse(azureCLINotSignedIn)
	}
	subs := azureCLISubscriptions(cli.Azure)
	scope := map[string]any{
		"host": armscope.Host, "header": "Authorization", "format": "Bearer %s",
		"secret_name": cliSecretName(cli.ComponentID, providerEntraPart),
		// TLS only and unpinned: the door, not the rule, decides which requests carry the token.
		"require_tls": true,
		"snapshot": azureARMGrantSnapshot{
			Mechanism: azureCLIMechanism, ComponentID: cli.ComponentID, OwnerSubject: owner, TenantID: cfg.TenantID,
			Audience: azureARMAudience, Subscriptions: subs, Access: cli.Azure.Access,
		},
	}
	raw, err := json.Marshal(scope)
	if err != nil {
		return refuse("could not author the Azure CLI's credential injection")
	}
	grantID := uuid.New()
	if _, err := s.createDispatchGrant(ctx, run, types.CredentialGrant{
		ID: grantID, RunID: run.ID, CreatedAt: time.Now(),
		Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: raw, TTLSeconds: azureCLIGrantTTLSec},
	}, false); err != nil {
		return refuse(loggedMsg(ctx, "could not record the Azure CLI's credential grant", err))
	}
	rule, err := injectionRuleFromScope(raw)
	if err != nil {
		return refuse(fmt.Sprintf("could not compile the Azure CLI's credential injection: %v", err))
	}
	injections = append(injections, runner.InjectionGrant{GrantID: grantID, Rule: rule})
	sandboxEnv[azureCLIConfigEnvVar] = encodeArtifactConfig(azureCLIFiles(cfg.TenantID, subs, s.cfg.Now()))
	sandboxEnv[envAzureTelemetry] = "0"
	// Port-qualified: the door terminates 443 alone, and refuses a tunnel to either host on any other port.
	opened := unionAllowedDomains(policy, []string{
		net.JoinHostPort(armscope.Host, azureCLIHTTPSPort), net.JoinHostPort(proxy.AzureLoginHost, azureCLIHTTPSPort),
	})
	return injections, azureCLIPlan{opened: opened, gate: &proxy.AzureARMGrant{
		TenantID: cfg.TenantID, Subscriptions: subs, Write: cli.Azure.Access == types.ComponentAccessWrite,
	}}, true
}
