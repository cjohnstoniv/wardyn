// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The namespace census: the proof, at the registration of a new issuer key k, that no stored string already
// starts delegate:person:<k>:. Doors have refused the prefix since v0.8.0, but data from before that, and
// strings an admin typed, could still carry it, and a portal registered with that k would then inherit them.
package store

import (
	"context"
	"fmt"
	"regexp"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// censusColumn is one column the census reads. A text column is matched as
// lower(col) LIKE 'delegate:person:<k>:%', a JSON column as text, anywhere in it. Where narrows a subject
// column to the rows whose subject is a user.
type censusColumn struct {
	Table, Column string
	JSON          bool
	Where         string
}

// namespaceCensusColumns is every column that can name a person and confer or restrict something. The guard
// test (TestNamespaceCensus_MigrationScanGuard) fails when a migration adds a column of this kind that is in
// neither this list nor namespaceCensusExempt, so a person-bearing column cannot be added unclassified.
var namespaceCensusColumns = []censusColumn{
	{"ado_run_pat_state", "owner", false, ""},
	{"ado_run_pats", "owner", false, ""},
	{"ado_signin_ends", "owner", false, ""},
	{"agent_runs", "created_by", false, ""},
	{"api_tokens", "principal", false, ""},
	{"approval_rules", "created_by", false, ""},
	{"approvals", "decided_by", false, ""},
	// A session acts as its person, so a stored portal principal there is what a registration's census must find.
	{"assistant_sessions", "principal", false, ""},
	{"attach_tickets", "principal", false, ""},
	{"aws_sso_refused_tokens", "owner", false, ""},
	{"aws_sso_spent_tokens", "owner", false, ""},
	{"capability_grants", "created_by", false, ""},
	{"capability_grants", "subject", false, "subject_type = 'user'"},
	{"capability_restrictions", "created_by", false, ""},
	{"cli_sessions", "principal", false, ""},
	{"cli_sign_ins", "principal", false, ""},
	{"components", "created_by", false, ""},
	{"components", "owner", false, ""},
	{"delegated_tokens", "principal", false, ""},
	{"delegates", "registered_by", false, ""},
	{"desktop_signins", "holder", false, ""},
	{"governance_change_approvals", "approver", false, ""},
	{"governance_changes", "decided_by", false, ""},
	{"governance_changes", "proposed_by", false, ""},
	{"governance_memberships", "created_by", false, ""},
	{"governance_memberships", "subject", false, "subject_type = 'user'"},
	{"governance_restriction_members", "created_by", false, ""},
	{"governance_restriction_members", "subject", false, "subject_type = 'user'"},
	{"governance_restrictions", "created_by", false, ""},
	{"governance_unit_admins", "subject", false, "subject_type = 'user'"},
	{"governance_units", "created_by", false, ""},
	{"kept_machines", "owner_subject", false, ""},
	{"key_domain_assignments", "subject", false, "subject_type = 'user'"},
	{"key_domain_login_groups", "principal", false, ""},
	{"launch_presets", "created_by", false, ""},
	{"mask_owner_erasures", "owner", false, ""},
	{"mask_values", "owner", false, ""},
	{"mcp_call_sends", "owner", false, ""},
	{"oidc_session_cuts", "sub", false, ""},
	{"oidc_session_revocations", "sub", false, ""},
	{"oidc_session_sids", "sub", false, ""},
	{"people", "created_by", false, ""},
	{"people", "principal", false, ""},
	{"person_invitations", "created_by", false, ""},
	{"portal_grants", "principal", false, ""},
	{"principal_groups", "principal", false, ""},
	{"principal_identities", "principal", false, ""},
	{"principal_identity_unbinds", "principal", false, ""},
	{"principal_keys", "owner", false, ""},
	{"principal_prefs", "principal", false, ""},
	{"provider_connection_changes", "owner", false, ""},
	{"role_mappings", "created_by", false, ""},
	{"run_attach_leases", "principal", false, ""},
	{"run_components", "owner", false, ""},
	{"run_create_keys", "principal", false, ""},
	{"run_launch_intents", "created_by", false, ""},
	{"run_mask_manifest", "owner", false, ""},
	{"run_mask_values", "owner", false, ""},
	{"run_queue", "owner", false, ""},
	{"run_support_grants", "helper", false, ""},
	{"run_template_revisions", "created_by", false, ""},
	{"run_templates", "created_by", false, ""},
	{"run_templates", "owner_id", false, ""},
	{"runner_inventories", "owner", false, ""},
	{"runner_pending_actions", "owner", false, ""},
	{"runner_pools", "created_by", false, ""},
	{"runner_pools", "owner", false, ""},
	{"runner_registration_tokens", "owner", false, ""},
	{"runners", "owner", false, ""},
	{"secret_owner_generations", "owned_by", false, ""},
	{"secrets", "owned_by", false, ""},
	{"service_identities", "created_by", false, ""},
	{"service_identities", "sponsor", false, ""},
	{"ssh_public_keys", "principal", false, ""},
	{"template_group_admins", "person", false, ""},
	{"ui_run_layouts", "principal", false, ""},
	{"user_drive_grants", "created_by", false, ""},
	{"user_drive_grants", "subject", false, "subject_type = 'user'"},
	{"user_drives", "created_by", false, ""},
	{"user_drives", "owner_subject", false, ""},
	{"user_types", "created_by", false, ""},
	{"wall_presets", "created_by", false, ""},
	{"wall_presets", "owner_id", false, ""},
	{"workload_bindings", "created_by", false, ""},
	{"workload_bindings", "sponsor", false, ""},
	{"workload_run_ends", "owner", false, ""},
	{"workloads", "created_by", false, ""},
	{"workloads", "owner", false, ""},
	{"workspaces", "owned_by", false, ""},
	{"run_policies", "spec", true, ""},
	{"governance_units", "ceiling", true, ""},
	{"governance_units", "overlay", true, ""},
	{"launch_presets", "request", true, ""},
	{"run_template_revisions", "document", true, ""},
	{"run_queue", "launch", true, ""},
	{"run_queue", "authority", true, ""},
	{"run_launch_intents", "document", true, ""},
	{"remote_runners", "people_subjects", true, ""},
	{"runner_pool_use_policies", "subjects", true, ""},
	{"governance_restrictions", "spec", true, ""},
	{"site_config", "config", true, ""},
	{"approval_rules", "spec", true, ""},
}

// namespaceCensusExempt is every column the guard sees that the census does not read, with the reason. Audit
// rows are exempt as a table, except the two policy actions of a running run, which the census reads with its
// own query (the UI gateway reads a run's UI-app callers from them).
var namespaceCensusExempt = map[string]string{
	"portal_issuers.created_by":                 "the admin who bound the issuer; this table is the registry the census consults",
	"run_support_grants.owner_email":            "an email address; a principal is never stored in it",
	"runner_pending_actions.subject":            "the action's subject kind (run or asserted), not a person",
	"runner_registration_tokens.owner_kind":     "a kind word, not a principal",
	"runners.owner_kind":                        "a kind word, not a principal",
	"sweeper_leader.holder":                     "the replica that holds the sweeper lease, not a person",
	"agent_runs.allowed_models":                 "a run's own settings and recorded state, not a list that names people",
	"agent_runs.created_via_actor":              "the agent a provider named as acting, recorded and never authorised",
	"agent_runs.image_admission":                "a run's own settings and recorded state, not a list that names people",
	"agent_runs.inner_walls":                    "a run's own settings and recorded state, not a list that names people",
	"agent_runs.local_folder":                   "a run's own settings and recorded state, not a list that names people",
	"agent_runs.local_folder_status":            "a run's own settings and recorded state, not a list that names people",
	"agent_runs.model_allowlists":               "a run's own settings and recorded state, not a list that names people",
	"agent_runs.policy_generations":             "a run's own settings and recorded state, not a list that names people",
	"agent_runs.run_limits":                     "a run's own settings and recorded state, not a list that names people",
	"agent_runs.service":                        "a run's own settings and recorded state, not a list that names people",
	"agent_runs.service_status":                 "a run's own settings and recorded state, not a list that names people",
	"agent_runs.vm_machine":                     "a run's own settings and recorded state, not a list that names people",
	"agent_runs.wall_preset":                    "a run's own settings and recorded state, not a list that names people",
	"api_tokens.groups":                         "group names or claim values, never a person principal",
	"approvals.requested_scope":                 "a run's own settings and recorded state, not a list that names people",
	"assistant_session_events.payload":          "transcript events; confer nothing, swept with the run output",
	"assistant_sessions.draft":                  "a draft confers nothing; what it names is checked when it becomes a held change or a write",
	"assistant_sessions.person_letters":         "the letters a transcript uses for people's names; confers nothing, swept with the transcript",
	"attach_tickets.via_actor":                  "the agent a provider named as acting, recorded and never authorised",
	"audit_events.data":                         "audit rows confer no authority; the policy rows of a running run are read by the census with its own query",
	"base_images.steps":                         "workspace, source and image content; the owner is a TEXT column the census reads",
	"cli_sessions.groups":                       "group names or claim values, never a person principal",
	"cli_sign_ins.groups":                       "group names or claim values, never a person principal",
	"components.definition":                     "component and wall definitions; no person list",
	"credential_delivery_policy.policy":         "runner, run-containment and sweep telemetry; not an authority source",
	"credential_grants.spec":                    "a run's own settings and recorded state, not a list that names people",
	"delegated_tokens.groups":                   "group names or claim values, never a person principal",
	"delegates.required_claim":                  "group names or claim values, never a person principal",
	"deprovision_jobs.audit_event_payload":      "one audit event's data pinned for a retried step, sealed for a human actor; audit rows confer no authority",
	"deprovision_jobs.detail":                   "runner, run-containment and sweep telemetry; not an authority source",
	"desktop_signins.pause_record":              "runner, run-containment and sweep telemetry; not an authority source",
	"governance_boundary.contact":               "limits, contact details and boundary settings; no person list (a boundary spec has no ui_apps field)",
	"governance_boundary.limits":                "limits, contact details and boundary settings; no person list (a boundary spec has no ui_apps field)",
	"governance_boundary.spec":                  "limits, contact details and boundary settings; no person list (a boundary spec has no ui_apps field)",
	"governance_changes.diff":                   "a held change keeps its subject here by design: the store ruling at approval refuses it, and the census cannot see what is not yet a grant",
	"governance_changes.payload":                "a held change keeps its subject here by design: the store ruling at approval refuses it, and the census cannot see what is not yet a grant",
	"governance_restrictions.contact":           "limits, contact details and boundary settings; no person list (a boundary spec has no ui_apps field)",
	"governance_restrictions.limits":            "limits, contact details and boundary settings; no person list (a boundary spec has no ui_apps field)",
	"governance_unit_deciders.rules":            "a unit decider rule names only owner, unit_admins or group:<name>",
	"governance_units.contact":                  "limits, contact details and boundary settings; no person list (a boundary spec has no ui_apps field)",
	"governance_units.limits":                   "limits, contact details and boundary settings; no person list (a boundary spec has no ui_apps field)",
	"governance_units.overlay_limits":           "limits, contact details and boundary settings; no person list (a boundary spec has no ui_apps field)",
	"groundtruth_sensors.observed_by_kind":      "runner, run-containment and sweep telemetry; not an authority source",
	"key_domain_login_groups.groups":            "group names or claim values, never a person principal",
	"principal_groups.groups":                   "group names or claim values, never a person principal",
	"principal_groups.roles":                    "role claim values, never a person principal",
	"principal_prefs.value":                     "a person's own display or preference data, keyed by a TEXT principal column the census reads",
	"run_components.definition":                 "component and wall definitions; no person list",
	"run_containment_pending.tuple":             "runner, run-containment and sweep telemetry; not an authority source",
	"run_queue.policy_ref":                      "runner, run-containment and sweep telemetry; not an authority source",
	"runner_execution_exits.payload":            "runner, run-containment and sweep telemetry; not an authority source",
	"runner_health.offline_summary":             "runner, run-containment and sweep telemetry; not an authority source",
	"runner_health.relay_status":                "runner, run-containment and sweep telemetry; not an authority source",
	"runner_inventory_observations.assertion":   "runner, run-containment and sweep telemetry; not an authority source",
	"runner_inventory_observations.containment": "runner, run-containment and sweep telemetry; not an authority source",
	"runner_pending_actions.proof":              "runner, run-containment and sweep telemetry; not an authority source",
	"runner_pools.limits":                       "limits, contact details and boundary settings; no person list (a boundary spec has no ui_apps field)",
	"runner_spool_cursors.recent_hashes":        "runner, run-containment and sweep telemetry; not an authority source",
	"runners.posture":                           "runner, run-containment and sweep telemetry; not an authority source",
	"sources.profile":                           "workspace, source and image content; the owner is a TEXT column the census reads",
	"sources.requirements":                      "workspace, source and image content; the owner is a TEXT column the census reads",
	"ui_run_layouts.layout":                     "a person's own display or preference data, keyed by a TEXT principal column the census reads",
	"wall_presets.spec":                         "component and wall definitions; no person list",
	"workloads.intent":                          "runner, run-containment and sweep telemetry; not an authority source",
	"workloads.last_refusal":                    "runner, run-containment and sweep telemetry; not an authority source",
	"workspaces.approved_egress":                "workspace, source and image content; the owner is a TEXT column the census reads",
	"workspaces.attachments":                    "workspace, source and image content; the owner is a TEXT column the census reads",
	"workspaces.base_image":                     "workspace, source and image content; the owner is a TEXT column the census reads",
	"workspaces.denied_egress":                  "workspace, source and image content; the owner is a TEXT column the census reads",
	"workspaces.image_build":                    "workspace, source and image content; the owner is a TEXT column the census reads",
	"workspaces.llm_cred":                       "workspace, source and image content; the owner is a TEXT column the census reads",
	"workspaces.profile":                        "workspace, source and image content; the owner is a TEXT column the census reads",
	"workspaces.record_results":                 "workspace, source and image content; the owner is a TEXT column the census reads",
	"workspaces.requirements":                   "workspace, source and image content; the owner is a TEXT column the census reads",
	"workspaces.sources":                        "workspace, source and image content; the owner is a TEXT column the census reads",
}

// censusKeyShape is what a k is: 26 characters of lowercase base32.
// NamespaceCensusColumns is the columns the census reads, for the test that proves each one is found.
func NamespaceCensusColumns() []CensusColumn {
	out := make([]CensusColumn, len(namespaceCensusColumns))
	for i, c := range namespaceCensusColumns {
		out[i] = CensusColumn(c)
	}
	return out
}

// CensusColumn is one column the census reads: its table and name, whether it holds JSON, and the filter on
// its row when the column names a subject of any type.
type CensusColumn = censusColumn

var censusKeyShape = regexp.MustCompile(`^[a-z2-7]{26}$`)

// NamespaceCensusQ counts, per column, the stored strings that start delegate:person:<issuerKey>:, and
// returns only the columns with a count above zero, keyed "table.column", counts and never values. It also
// counts identity rows whose issuer is delegate:person:<issuerKey>, and the policy rows of runs that are still
// running. It is called only for a k with no binding (the registration, and the approval's decision
// transaction); a bound k's own people and records are expected there.
func NamespaceCensusQ(ctx context.Context, q Querier, issuerKey string) (map[string]int, error) {
	if !censusKeyShape.MatchString(issuerKey) {
		return nil, fmt.Errorf("store: namespace census: %q is not an issuer key", issuerKey)
	}
	prefix := types.PortalPersonPrefix + issuerKey + ":"
	out := map[string]int{}
	count := func(name, sql string, args ...any) error {
		var n int
		if err := q.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			return fmt.Errorf("store: namespace census %s: %w", name, err)
		}
		if n > 0 {
			out[name] = n
		}
		return nil
	}
	for _, c := range namespaceCensusColumns {
		expr, pat := `lower(`+c.Column+`) LIKE $1`, prefix+"%"
		if c.JSON {
			expr, pat = `lower(`+c.Column+`::text) LIKE $1`, "%"+prefix+"%"
		}
		sql := `SELECT count(*) FROM ` + c.Table + ` WHERE ` + expr
		if c.Where != "" {
			sql += ` AND ` + c.Where
		}
		if err := count(c.Table+"."+c.Column, sql, pat); err != nil {
			return nil, err
		}
	}
	if err := count("principal_identities.issuer", `SELECT count(*) FROM principal_identities WHERE issuer = $1`,
		types.PortalPersonPrefix+issuerKey); err != nil {
		return nil, err
	}
	states := make([]string, 0, len(types.NonTerminalRunStates))
	for _, st := range types.NonTerminalRunStates {
		states = append(states, string(st))
	}
	if err := count("audit_events.data", `
		SELECT count(*) FROM audit_events e JOIN agent_runs r ON r.id = e.run_id
		 WHERE e.action IN ('run.policy.resolve', 'run.policy.generation') AND r.state = ANY($2)
		   AND lower(e.data::text) LIKE $1`, "%"+prefix+"%", states); err != nil {
		return nil, err
	}
	return out, nil
}
