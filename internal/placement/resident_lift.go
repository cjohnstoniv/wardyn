// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"reflect"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// residentBuilt are the classes whose runner_resident delivery is built and
// pinned (design 0.9 §7.4): a dispatch authors the placeholder where the value
// is read, the org hands the value to the run's runner before the create, and
// erases it at run end. An Azure DevOps minted token carries no value: the
// runner's proxy fetches the per-run token from the organisation over the relay
// on each request, as the cluster's does, and holds it in memory only. A grant
// record of one of them is admitted resident; a record, a placeholder or an
// intent of any other class keeps the refusal "runner_resident delivery of this
// credential is not built yet". Each class was added only once every one of its
// pins passed.
var residentBuilt = map[string]bool{
	ClassEnvSecret:    true,
	ClassFileSecret:   true,
	ClassSSHKey:       true,
	ClassGitPATHelper: true,
	ClassADOMintedPAT: true,
}

// ResidentGrant reports whether an org-held grant of origin is delivered
// runner_resident under policy on a runner of trust class trust, for a run
// whose blast-radius floor is floor (a floor refuses it here, at create, not
// first at dispatch), mirroring ViaOrgGrant: resident is true when
// the policy delivers its class runner_resident and the table admits its grant
// record; ref is the refusal when the policy delivers it so but the record is
// refused (a class not built, a posture requirement, the class's bounds). Both
// are zero for a class the policy does not deliver runner_resident.
func ResidentGrant(origin CredentialOrigin, policy DeliveryPolicy, trust types.RunnerTrustClass, floor types.ConfinementClass) (resident bool, ref *Refusal) {
	if origin.Class == ClassOwn || origin.Class == "" || policy.ForTrust(origin.Delivery, trust).Mode != ModeRunnerResident {
		return false, nil
	}
	if r := classifyCredential(Entries, "ProxyConfig.Policy.EligibleGrants", origin, LocalPlan{Delivery: policy, Trust: trust, ConfinementFloor: floor}, reflect.Value{}); r != nil {
		return false, r
	}
	return true, nil
}
