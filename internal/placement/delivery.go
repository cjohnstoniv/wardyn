// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// Mode is how an org-held credential reaches a local run (OD-12).
type Mode string

const (
	ModeOwn            Mode = "own"             // the person's own material, wherever it already lives
	ModeViaOrg         Mode = "via_org"         // only in the org; used through the org's egress for the run's life
	ModeRunnerResident Mode = "runner_resident" // in the runner's OS keystore, then the local proxy or sandbox
	ModeRefuse         Mode = "refuse"          // nowhere; the default for every org-held class
)

// Class keys of the delivery policy: the closed set of design §7.2.
const (
	ClassOAuthSubscription = "oauth_subscription"
	ClassAPIKey            = "api_key"
	ClassBedrockBearer     = "bedrock_bearer"
	ClassAWSSSOBearer      = "aws_sso_bearer"
	ClassAzureFoundryToken = "azure_foundry_token"
	ClassBedrockRoleCreds  = "bedrock_role_credentials"
	ClassEnvSecret         = "env_secret"
	ClassFileSecret        = "file_secret"
	ClassSSHKey            = "ssh_key"
	ClassGitPATBroker      = "git_pat_broker"
	ClassGitPATHelper      = "git_pat_helper"
	ClassGitHubToken       = "github_token"
	ClassADOMintedPAT      = "ado_minted_pat"
	ClassCloudSTS          = "cloud_sts"
)

var viaOrgResidentRefuse = []Mode{ModeViaOrg, ModeRunnerResident, ModeRefuse}

// classModes are the modes each class may be configured to; the first entry of
// a fixed class is its only mode.
var classModes = map[string][]Mode{
	ClassOAuthSubscription: {ModeOwn},
	ClassAPIKey:            viaOrgResidentRefuse,
	ClassBedrockBearer:     viaOrgResidentRefuse,
	ClassAWSSSOBearer:      viaOrgResidentRefuse,
	ClassAzureFoundryToken: viaOrgResidentRefuse,
	ClassBedrockRoleCreds:  {ModeRunnerResident, ModeRefuse},
	ClassEnvSecret:         {ModeRunnerResident, ModeRefuse},
	ClassFileSecret:        {ModeRunnerResident, ModeRefuse},
	ClassSSHKey:            {ModeRunnerResident, ModeRefuse},
	ClassGitPATBroker:      viaOrgResidentRefuse,
	ClassGitPATHelper:      {ModeRunnerResident, ModeRefuse},
	ClassGitHubToken:       {ModeViaOrg, ModeRefuse},
	ClassADOMintedPAT:      {ModeViaOrg, ModeRefuse},
	ClassCloudSTS:          {ModeRefuse},
}

// fixedClasses cannot be written: the person's own sign-in, and a class that
// needs attestation no laptop sandbox has.
var fixedClasses = []string{ClassOAuthSubscription, ClassCloudSTS}

// DeliveryClasses lists every class key, sorted.
func DeliveryClasses() []string {
	out := make([]string, 0, len(classModes))
	for k := range classModes {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// AllowedModes is the set of modes a class may be delivered under; nil for an unknown class.
func AllowedModes(class string) []Mode { return slices.Clone(classModes[class]) }

// Writable reports whether a policy may configure the class.
func Writable(class string) bool {
	_, known := classModes[class]
	return known && !slices.Contains(fixedClasses, class)
}

// PostureRequirement is what a runner's reported posture (types.RunnerPosture,
// self-reported) must meet for a runner_resident class: only on that mode.
type PostureRequirement struct {
	MDMManaged    bool              `json:"mdm_managed,omitempty"`
	DiskEncrypted bool              `json:"disk_encrypted,omitempty"`
	OSMin         map[string]string `json:"os_min,omitempty"` // per-OS minimum version
}

// ClassPolicy is one class's configured mode.
type ClassPolicy struct {
	Mode           Mode                `json:"mode"`
	RequirePosture *PostureRequirement `json:"require_posture,omitempty"`
}

// DeliveryPolicy is the `local_credential_delivery` governance document. A class
// absent from it means refuse.
type DeliveryPolicy struct {
	Classes map[string]ClassPolicy `json:"classes"`
}

// ErrInvalidPolicy marks a delivery policy a writer must refuse (a 400).
var ErrInvalidPolicy = errors.New("placement: invalid credential delivery policy")

// Validate checks every class key and mode against design §7.2 and §7.5.
func (p DeliveryPolicy) Validate() error {
	for _, class := range sortedKeys(p.Classes) {
		cp := p.Classes[class]
		if _, known := classModes[class]; !known {
			return fmt.Errorf("%w: unknown class %q", ErrInvalidPolicy, class)
		}
		if !Writable(class) {
			return fmt.Errorf("%w: class %q is not configurable", ErrInvalidPolicy, class)
		}
		if !slices.Contains(classModes[class], cp.Mode) {
			return fmt.Errorf("%w: class %q cannot be delivered as %q (allowed: %s)", ErrInvalidPolicy, class, cp.Mode, joinModes(classModes[class]))
		}
		if cp.RequirePosture != nil && cp.Mode != ModeRunnerResident {
			return fmt.Errorf("%w: class %q has require_posture without mode %q", ErrInvalidPolicy, class, ModeRunnerResident)
		}
	}
	return nil
}

// For is the configured policy of a class: refuse when absent.
func (p DeliveryPolicy) For(class string) ClassPolicy {
	if cp, ok := p.Classes[class]; ok {
		return cp
	}
	return ClassPolicy{Mode: ModeRefuse}
}

func sortedKeys(m map[string]ClassPolicy) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func joinModes(ms []Mode) string {
	s := make([]string, len(ms))
	for i, m := range ms {
		s[i] = string(m)
	}
	return strings.Join(s, ", ")
}

const residentPlaceholderPrefix = "wardyn-resident:"

// ResidentPlaceholder is what a dispatched spec carries in place of a
// runner_resident value: it names the grant, never the secret.
func ResidentPlaceholder(grantID uuid.UUID) string {
	return residentPlaceholderPrefix + grantID.String()
}

// ResidentGrantID reads a placeholder back; false when v is not one.
func ResidentGrantID(v string) (uuid.UUID, bool) {
	rest, ok := strings.CutPrefix(v, residentPlaceholderPrefix)
	if !ok {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(rest)
	return id, err == nil
}
