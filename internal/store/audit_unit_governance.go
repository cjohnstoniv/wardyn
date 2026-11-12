// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"

	"github.com/google/uuid"
)

// unitGovernanceActions are the governance rows a unit admin's feed serves when they name one of the
// caller's units: as the target, as data.unit_id, or inside data.units. A closed list: a new governance
// action is served only once it is added here and its keys are on the governance view's allow list.
// governance.change.* rows are left out on purpose: the applied row records the outcome, and a pending
// change is read in /governance/changes.
var unitGovernanceActions = []string{
	"governance.unit.write", "governance.unit.delete",
	"governance.membership.write", "governance.membership.delete", "governance.membership.promote",
	"governance.unit_admin.grant", "governance.unit_admin.revoke",
	"governance.deciders.set",
	"governance.envelope.set", "governance.envelope.delete",
	"governance.elevation.request", "governance.elevation.approve", "governance.elevation.reject", "governance.elevation.expire",
	"governance.image_request.request", "governance.image_request.approve", "governance.image_request.reject",
}

// unitRunRowsClause is the rows of the runs frozen to $1's units.
const unitRunRowsClause = "run_id IN (SELECT id FROM agent_runs WHERE unit_id = ANY($1))"

// unitGovernanceArmClause is the rows with no run that are listed governance rows naming a unit of $3
// (the unit ids as text): by target, by data.unit_id, or inside data.units when that is an array. The
// array test keeps a scalar units value from matching as a one-item array.
const unitGovernanceArmClause = `(run_id IS NULL AND action = ANY($2)
	AND (target = ANY($3) OR data->>'unit_id' = ANY($3)
		OR (jsonb_typeof(data->'units') = 'array' AND data->'units' ?| $3)))`

// unitAuditFirstClause and its arguments: the runs' rows alone for an actor-filtered feed, and with the
// governance arm otherwise ($2 is the action list, $3 the unit ids as text).
func unitAuditFirstClause(units []uuid.UUID, withGovernance bool) (string, []any) {
	if !withGovernance {
		return unitRunRowsClause, []any{units}
	}
	ids := make([]string, len(units))
	for i, u := range units {
		ids[i] = u.String()
	}
	return fmt.Sprintf("(%s\n\t\t\tOR %s)", unitRunRowsClause, unitGovernanceArmClause), []any{units, unitGovernanceActions, ids}
}
