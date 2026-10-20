// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The AWS CLI arm. An org row of cli.kind aws gives the run's `aws` the
// person's own IAM Identity Center sign-in, the way a bedrock_sso provider
// gives the model one: the sandbox holds a profile and an inert token cache,
// the proxy sets the person's live SSO token on the one GetRoleCredentials
// request the row pins, and AWS answers with role credentials the CLI keeps
// in the sandbox until they expire. The SSO token itself never enters it.

// cliAWSMechanism is the snapshot's and the re-auth hold's name for this lane.
const cliAWSMechanism = "aws_cli"

// Member-visible sentences of the arm.
const (
	// cliAWSInjectOffRefusal: WARDYN_AWS_SSO_PROXY_INJECT off would put the
	// SSO token in the sandbox, so a CLI row is refused rather than degraded.
	cliAWSInjectOffRefusal = "This run uses the AWS CLI, which this deployment has turned off: " +
		"WARDYN_AWS_SSO_PROXY_INJECT is off, so your AWS sign-in would be written into the sandbox. " +
		"Remove the AWS CLI from the run, or ask your admin."
	// cliAWSWithBedrockSSORefusal: one sandbox has one AWS profile, and the
	// Bedrock sign-in lane already stages it.
	cliAWSWithBedrockSSORefusal = "This run uses an Amazon Bedrock sign-in and the AWS CLI together, " +
		"and a sandbox holds one AWS profile. Choose another model provider, or remove the AWS CLI from the run."
	cliAWSReauthRaisedSentence = "This run's AWS CLI sign-in can no longer be renewed, so the run is HELD " +
		"while you sign in again. It resumes by itself when the sign-in lands."
)

// awsCLIDispatch is the AWS CLI row a run attached, as the gate admitted it.
type awsCLIDispatch struct {
	ComponentID string
	AWS         types.CLIAWS
	// Hosts are the row's own hosts, which the proxy re-signs on when it can
	// (WARDYN_AWS_SIGV4_AT_PROXY).
	Hosts []string
}

// awsCLI is the run's AWS CLI row, nil when it attached none. The gate admits
// at most one, and only an org row.
func (c runComponents) awsCLI() *awsCLIDispatch {
	for _, a := range c.attached {
		if aws := attachedAWSCLI(a); aws != nil {
			return &awsCLIDispatch{ComponentID: a.snapshot.ComponentID.String(), AWS: *aws, Hosts: a.snapshot.Definition.Hosts}
		}
	}
	return nil
}

// attachedAWSCLI is an attached org row's AWS CLI block, nil for anything else.
func attachedAWSCLI(a attachedComponent) *types.CLIAWS {
	cli := a.snapshot.Definition.CLI
	if a.snapshot.SelfDefined || a.snapshot.ComponentID == nil || cli == nil || cli.Kind != types.CLIKindAWS {
		return nil
	}
	return cli.AWS
}

// runUsesAWSSignIn reports whether the run's chosen provider, or any included
// harness's, is a bedrock_sso provider: its lane stages the sandbox's one AWS
// profile.
func runUsesAWSSignIn(primary runProviderChoice, set []harnessChoice) bool {
	sso := func(c runProviderChoice) bool { return c.chosen && c.provider.Kind == types.ModelProviderBedrockSSO }
	return sso(primary) || slices.ContainsFunc(set, func(h harnessChoice) bool { return sso(h.choice) })
}

// runAWSCLIRows is the ids of the AWS CLI org rows a run's component snapshot
// carries; none on a store that records no components.
func (s *Server) runAWSCLIRows(ctx context.Context, runID uuid.UUID) (map[string]bool, error) {
	cs, ok := s.cfg.Store.(store.ComponentStore)
	if !ok {
		return nil, nil
	}
	rows, err := cs.ListRunComponents(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, rc := range rows {
		if attachedAWSCLI(attachedComponent{snapshot: rc}) != nil {
			out[rc.ComponentID.String()] = true
		}
	}
	return out, nil
}

// cliAWSScope is the scope a person's sign-in for an AWS CLI row lives in.
func cliAWSScope(componentID, owner string) awsSSOScope {
	return awsSSOScope{perUser: true, owner: owner, cli: componentID}
}

// cliAWSConfigFileContents is the Bedrock lane's profile with the row's
// default region added: the CLI reads it from the profile.
func cliAWSConfigFileContents(b awsSSOBlob, region string) string {
	return awsSSOConfigFileContents(b) + "region = " + region + "\n"
}

// cliAWSPortalEgress is the one host the CLI must reach that the row's own
// hosts do not list: its portal, where the proxy sets the session. The CLI
// never calls SSO-OIDC, since its cache never expires inside a run.
func cliAWSPortalEgress(ssoRegion, endpointOverride string) []string {
	if endpointOverride != "" {
		return ssoEgressHosts(ssoRegion, endpointOverride)
	}
	return []string{ssoPortalHost(ssoRegion, "")}
}

// authorAWSCLI stages the run's AWS CLI row: the profile and the inert cache
// in the sandbox, the portal on its allowlist, and the grant and MITM entry
// that set the run owner's session on the wire. The kill switch is read here,
// at dispatch, and its off position refuses the run: the only other way to
// serve the CLI is the SSO token in the sandbox. ok=false: the run was marked
// FAILED and dispatch stops.
func (s *Server) authorAWSCLI(ctx context.Context, run types.AgentRun, cli *awsCLIDispatch, policy *types.RunPolicySpec,
	sandboxEnv map[string]string, injections []runner.InjectionGrant,
) ([]runner.InjectionGrant, []string, *proxy.AWSSigV4Config, bool) {
	if cli == nil {
		return injections, nil, nil, true
	}
	refuse := func(msg string) ([]runner.InjectionGrant, []string, *proxy.AWSSigV4Config, bool) {
		s.failAndRevoke(ctx, run.ID, types.RunStarting, msg)
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"error": msg, "component_id": cli.ComponentID})))
		return injections, nil, nil, false
	}
	switch {
	case !s.cfg.AWSSSOProxyInject:
		return refuse(cliAWSInjectOffRefusal)
	case sandboxEnv[awsSSOConfigEnvVar] != "":
		return refuse(cliAWSWithBedrockSSORefusal)
	}
	a := cli.AWS
	// Re-signing: the placeholder profile with the row's Region, and no token
	// cache or portal route; otherwise the profile, the inert cache and the portal.
	sigv4Hosts, sigv4 := s.awsSigV4Hosts(cli.Hosts)
	if sigv4 {
		stageAWSSigV4Files(sandboxEnv, a.Region)
	} else {
		profile := awsSSOBlob{StartURL: a.StartURL, Region: a.SSORegion, AccountID: a.AccountID, RoleName: a.RoleName}
		s.stageAWSSSOFiles(sandboxEnv, cliAWSConfigFileContents(profile, a.Region), profile, true)
		unionAllowedDomains(policy, cliAWSPortalEgress(a.SSORegion, s.cfg.AWSSSOEndpointOverride))
	}
	return s.authorAWSSSOGrant(ctx, run, awsSSOScopeSnapshot{
		OwnerSubject:     runIdentitySubject(ctx, run.CreatedBy),
		CredentialSource: string(types.CredentialSourcePerUser),
		Mechanism:        cliAWSMechanism,
		SSOAccountID:     a.AccountID,
		SSORoleName:      a.RoleName,
		Region:           a.SSORegion,
		ComponentID:      cli.ComponentID,
		SSOStartURL:      a.StartURL,
	}, injections, sigv4Hosts)
}

// runCarriesCLIComponent reports whether run still carries the org row id as an
// attached component; a failed read says no, since a scope is never resolved
// from a failed read.
func runCarriesCLIComponent(ctx context.Context, cs store.ComponentStore, run types.AgentRun, id uuid.UUID) bool {
	attached, err := cs.ListRunComponents(ctx, run.ID)
	return err == nil && slices.ContainsFunc(attached, func(rc types.RunComponent) bool {
		return !rc.SelfDefined && rc.ComponentID != nil && *rc.ComponentID == id
	})
}

// cliSSOScopeAt is the AWS CLI arm of resolveAWSSSOInjection's scope
// re-derivation: the run owner's own session for the row the grant recorded,
// while the run still carries that row and the live row still pins what the
// grant recorded. drift names the first thing that moved ("" = equal); a read
// that fails drifts too, since a scope is never resolved from a failed read.
func (s *Server) cliSSOScopeAt(ctx context.Context, run types.AgentRun, sn awsSSOScopeSnapshot, subject string) (awsSSOScope, string) {
	if sn.OwnerSubject == "" || sn.OwnerSubject != subject {
		return awsSSOScope{}, "owner_not_caller"
	}
	cs, ok := s.cfg.Store.(store.ComponentStore)
	id, err := uuid.Parse(sn.ComponentID)
	if !ok || err != nil {
		return awsSSOScope{}, "component"
	}
	if !runCarriesCLIComponent(ctx, cs, run, id) {
		return awsSSOScope{}, "component"
	}
	row, err := cs.GetComponent(ctx, id, "")
	if err != nil {
		return awsSSOScope{}, "component"
	}
	live := attachedAWSCLI(attachedComponent{snapshot: types.RunComponent{ComponentID: &id, Definition: row.Definition}})
	if live == nil || live.StartURL != sn.SSOStartURL || live.SSORegion != sn.Region ||
		live.AccountID != sn.SSOAccountID || live.RoleName != sn.SSORoleName {
		return awsSSOScope{}, "sso_pin"
	}
	return cliAWSScope(sn.ComponentID, subject), ""
}

// bindsSnapshot reports whether a stored session is the one a CLI grant's
// snapshot pins: the same portal, region, account and role.
func (b awsSSOBlob) bindsSnapshot(sn awsSSOScopeSnapshot) bool {
	return b.StartURL == sn.SSOStartURL && b.Region == sn.Region && b.AccountID == sn.SSOAccountID && b.RoleName == sn.SSORoleName
}
