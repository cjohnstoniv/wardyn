// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Mode is how an org-held credential reaches a local run (OD-12).
type Mode string

const (
	ModeOwn            Mode = "own"             // the person's own material, wherever it already lives
	ModeViaOrg         Mode = "via_org"         // only in the org; used through the org's egress for the run's life
	ModeRunnerResident Mode = "runner_resident" // in the runner's OS keystore, then the local proxy or sandbox; an Azure DevOps minted token only in the runner proxy's memory
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
	ClassADOMintedPAT:      viaOrgResidentRefuse, // resident only per connection (ADOEntraConfig.RunnerResident)
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

// BuildableModes is the subset of AllowedModes(class) a local run can be
// delivered under today: via_org only where a via_org-shaped table row names
// the class, runner_resident only where its delivery is built (residentBuilt).
// A stored policy may still name the other modes; they refuse at every door.
func BuildableModes(class string) []Mode {
	var out []Mode
	for _, m := range classModes[class] {
		switch m {
		case ModeViaOrg:
			if !slices.ContainsFunc(Table, func(e Entry) bool { return e.ViaOrg && e.Rule != RuleNotConfigured && e.Delivery == class }) {
				continue
			}
		case ModeRunnerResident:
			if !residentBuilt[class] {
				continue
			}
		}
		out = append(out, m)
	}
	return out
}

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

// PostureUnmet names the first field of req that p does not meet; "" when p
// meets it.
func PostureUnmet(req *PostureRequirement, p *types.RunnerPosture) string {
	if unmet := PostureUnmetAll(req, p); len(unmet) > 0 {
		return unmet[0]
	}
	return ""
}

// PostureUnmetAll names every field of req that p does not meet, in a fixed
// order; nil when p meets it. A runner that never reported posture meets no
// requirement ("posture"), and an os_min names every OS a runner may run: an
// unlisted OS does not meet it.
func PostureUnmetAll(req *PostureRequirement, p *types.RunnerPosture) []string {
	switch {
	case req == nil:
		return nil
	case p == nil:
		return []string{"posture"}
	}
	var out []string
	if req.MDMManaged && !p.MDMManaged {
		out = append(out, "mdm_managed")
	}
	if req.DiskEncrypted && !p.DiskEncrypted {
		out = append(out, "disk_encrypted")
	}
	if len(req.OSMin) > 0 {
		if min, ok := req.OSMin[p.OS]; !ok || !versionAtLeast(p.OSVersion, min) {
			out = append(out, "os_min")
		}
	}
	return out
}

// requires lists req's fields, as a refusal names the requirement.
func (req *PostureRequirement) requires() string {
	var out []string
	if req.MDMManaged {
		out = append(out, "mdm_managed")
	}
	if req.DiskEncrypted {
		out = append(out, "disk_encrypted")
	}
	for _, name := range slices.Sorted(maps.Keys(req.OSMin)) {
		out = append(out, "os_min "+name+" "+req.OSMin[name])
	}
	return strings.Join(out, ", ")
}

// versionAtLeast compares dot-separated numeric versions; a part that is not a
// number is not at least anything.
func versionAtLeast(v, min string) bool {
	a, b := strings.Split(v, "."), strings.Split(min, ".")
	for i := range max(len(a), len(b)) {
		x, y := 0, 0
		var err error
		if i < len(a) {
			if x, err = strconv.Atoi(a[i]); err != nil {
				return false
			}
		}
		if i < len(b) {
			if y, err = strconv.Atoi(b[i]); err != nil {
				return false
			}
		}
		if x != y {
			return x > y
		}
	}
	return true
}

// ClassPolicy is one class's configured mode.
type ClassPolicy struct {
	Mode           Mode                `json:"mode"`
	RequirePosture *PostureRequirement `json:"require_posture,omitempty"`
}

// DeliveryPolicy is the `local_credential_delivery` governance document. A class
// absent from it means refuse. ByTrust overrides a class for runners of one trust
// class (types.RunnerTrustClass, stored and written only by a security operator);
// a class it does not name falls back to Classes, for every trust class.
type DeliveryPolicy struct {
	Classes map[string]ClassPolicy                 `json:"classes"`
	ByTrust map[types.RunnerTrustClass]TrustPolicy `json:"by_trust,omitempty"`
}

// TrustPolicy is one trust class's overrides of the global classes.
type TrustPolicy struct {
	Classes map[string]ClassPolicy `json:"classes"`
}

// ErrInvalidPolicy marks a delivery policy a writer must refuse (a 400).
var ErrInvalidPolicy = errors.New("placement: invalid credential delivery policy")

// Validate checks every class key and mode against design §7.2 and §7.5, and each
// trust class's overrides against the same bounds. via_org is never trust-gated: its
// credential never reaches the runner, so an override may neither deliver a class
// via_org nor override a class the global row delivers via_org. require_posture
// under org_cluster is refused: posture is a laptop's self-report.
func (p DeliveryPolicy) Validate() error {
	if err := validateClasses(p.Classes, ""); err != nil {
		return err
	}
	for _, trust := range slices.Sorted(maps.Keys(p.ByTrust)) {
		tp := p.ByTrust[trust]
		if !trust.Valid() {
			return fmt.Errorf("%w: unknown trust class %q", ErrInvalidPolicy, trust)
		}
		if err := validateClasses(tp.Classes, "by_trust."+string(trust)+"."); err != nil {
			return err
		}
		for _, class := range sortedKeys(tp.Classes) {
			cp := tp.Classes[class]
			switch {
			case cp.Mode == ModeViaOrg:
				return fmt.Errorf("%w: by_trust.%s.%s: via_org is never set per trust class", ErrInvalidPolicy, trust, class)
			case p.Classes[class].Mode == ModeViaOrg:
				return fmt.Errorf("%w: by_trust.%s.%s: the class is delivered via_org, which is never trust-gated", ErrInvalidPolicy, trust, class)
			case cp.RequirePosture != nil && trust == types.RunnerTrustOrgCluster:
				return fmt.Errorf("%w: by_trust.%s.%s: require_posture does not apply to an org_cluster runner", ErrInvalidPolicy, trust, class)
			}
		}
	}
	return nil
}

func validateClasses(classes map[string]ClassPolicy, prefix string) error {
	for _, name := range sortedKeys(classes) {
		cp, class := classes[name], name
		name = prefix + name
		if _, known := classModes[class]; !known {
			return fmt.Errorf("%w: unknown class %q", ErrInvalidPolicy, name)
		}
		if !Writable(class) {
			return fmt.Errorf("%w: class %q is not configurable", ErrInvalidPolicy, name)
		}
		if !slices.Contains(classModes[class], cp.Mode) {
			return fmt.Errorf("%w: class %q cannot be delivered as %q (allowed: %s)", ErrInvalidPolicy, name, cp.Mode, joinModes(classModes[class]))
		}
		if cp.RequirePosture != nil && cp.Mode != ModeRunnerResident {
			return fmt.Errorf("%w: class %q has require_posture without mode %q", ErrInvalidPolicy, name, ModeRunnerResident)
		}
	}
	return nil
}

// ErrUnbuildablePolicy marks a delivery policy that sets a mode the engine cannot build for a class (a 422).
var ErrUnbuildablePolicy = errors.New("placement: credential delivery policy sets a mode that is not built")

// CheckBuildable refuses a policy that sets any class, in Classes or in any trust class's overrides, to a
// mode outside BuildableModes(class). Validate stays the bounds check a stored document is judged on; this
// is the write-time check only, so a stored document is never rewritten or refused by it.
func (p DeliveryPolicy) CheckBuildable() error {
	check := func(classes map[string]ClassPolicy, prefix string) error {
		for _, class := range sortedKeys(classes) {
			if m := classes[class]; !slices.Contains(BuildableModes(class), m.Mode) {
				return fmt.Errorf("%w: class %q cannot be delivered as %q yet (the server can build: %s)",
					ErrUnbuildablePolicy, prefix+class, m.Mode, joinModes(BuildableModes(class)))
			}
		}
		return nil
	}
	if err := check(p.Classes, ""); err != nil {
		return err
	}
	for _, trust := range slices.Sorted(maps.Keys(p.ByTrust)) {
		if err := check(p.ByTrust[trust].Classes, "by_trust."+string(trust)+"."); err != nil {
			return err
		}
	}
	return nil
}

// ForTrust is a class's policy on a runner of trust class trust: the trust class's
// override when it names the class, else the global row, else refuse. A class the
// global row delivers via_org is the same on every runner (Validate keeps overrides
// off it); any other class on an unknown trust class refuses.
func (p DeliveryPolicy) ForTrust(class string, trust types.RunnerTrustClass) ClassPolicy {
	global, ok := p.Classes[class]
	if ok && global.Mode == ModeViaOrg {
		return global
	}
	if !trust.Valid() {
		return ClassPolicy{Mode: ModeRefuse}
	}
	if cp, found := p.ByTrust[trust].Classes[class]; found {
		return cp
	}
	if ok {
		return global
	}
	return ClassPolicy{Mode: ModeRefuse}
}

// TrustClassesByRank is the closed trust set, highest rank first.
func TrustClassesByRank() []types.RunnerTrustClass {
	return []types.RunnerTrustClass{types.RunnerTrustOrgCluster, types.RunnerTrustManagedDevice, types.RunnerTrustPersonal}
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
