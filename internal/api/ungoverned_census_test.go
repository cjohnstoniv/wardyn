// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"
)

// ungovernedCallerCensus is every production function in internal/api that asks runUngoverned,
// isOperator or isSecurityOperator, the predicates that let a caller skip governance, with how many
// times it asks and where the Wardyn boundary binds what it admits:
//   - R: the last step of resolveRunPolicyFacts (boundaryRunStep), over the ceiling boundaryCeiling met;
//   - S: a step run's policy (Record Mode's boundaryRecordPolicy);
//   - D: dispatch (dispatchAdmitted's floor check);
//   - V: revive (reviveBelowBoundaryFloor);
//   - A: approvals (boundaryAdmitApproval, after the member gate);
//   - E: end changes (boundaryEndPlan, the create end cap and the run-limit sweep);
//   - authz-only: a route tier, reach, list scope or display that admits no run policy.
//
// Keyed by (file, enclosing function), never by line. A new call fails until it is classified here.
var ungovernedCallerCensus = map[string]struct {
	calls int
	via   string
}{
	"apitokens.go:roleSnapshotDrops":                           {4, "authz-only: a token's stamped tier, not a run"},
	"approvals.go:authorizeUserDecision":                       {1, "A: the member decision gate; the boundary hook runs after it for every decider"},
	"approvals.go:resolveAlwaysTarget":                         {1, "A: an always-scope approval's target; the boundary hook bounds the host"},
	"approvals_decidable.go:decidableKindAndOwner":             {1, "A: who may decide; the boundary hook bounds what is admitted"},
	"approvals_list_view.go:approvalsViewScope":                {1, "authz-only: which approvals a reader lists"},
	"approvals_reauth.go:canSeeApproval":                       {1, "authz-only: approval read reach"},
	"attach.go:refuseAttachEntry":                              {1, "authz-only: a session door; traffic in the session is the dispatched proxy config"},
	"attach_ticket.go:handleAttachTicket":                      {3, "authz-only: attach reach and the ticket's reach stamp"},
	"audit.go:auditRowsFor":                                    {1, "authz-only: audit read scope"},
	"audit.go:auditScope":                                      {1, "authz-only: audit read scope"},
	"audit_partition_export.go:exportAuditPartition":           {1, "authz-only: audit export tier"},
	"boundary_meet.go:boundaryEndPlan":                         {1, "E: an ungoverned caller's end change is held to the boundary's maximum"},
	"capabilities.go:capAllowed":                               {1, "R: capability admission; the spec it admits meets the boundary at resolve"},
	"capabilities.go:newCapBatch":                              {1, "R: capability admission; the spec it admits meets the boundary at resolve"},
	"components_builtin.go:builtinGitHubGrant":                 {1, "D: the built-in GitHub grant clamp; grants meet the boundary at resolve and dispatch"},
	"compose_setup.go:setupBackendItem":                        {1, "authz-only: a setup row's detail"},
	"compose_setup.go:setupMemberView":                         {1, "authz-only: a workspace's read tier on the setup page"},
	"govern_admin.go:handleRecordGoverned":                     {1, "S: a Record Mode session; its policy is met with the boundary (boundaryRecordPolicy)"},
	"govern_admin.go:markGovernanceExempt":                     {1, "authz-only: the governance_exempt audit marker"},
	"govern_admin.go:presetLaunchOpenTo":                       {1, "R: preset visibility at launch; the preset's policy resolves through the boundary"},
	"govern_admin.go:refuseNoTicketAttach":                     {1, "authz-only: a session door"},
	"govern_admin.go:runUngoverned":                            {1, "authz-only: the predicate itself"},
	"governance.go:resolveChainCeiling":                        {1, "R: the operator arm; boundaryCeiling meets whatever it returns"},
	"governance_policy_ref.go:meGovernanceContact":             {1, "authz-only: a display contact"},
	"governance_run_doors.go:boundUIApps":                      {1, "authz-only: a door the boundary holds no knob for"},
	"grants.go:handleListGrants":                               {1, "authz-only: grant read reach"},
	"helpers.go:ownsRunOrAdmin":                                {1, "authz-only: run reach"},
	"helpers.go:ownsRunOrSuperAdmin":                           {1, "authz-only: run reach"},
	"helpers.go:ownsWorkspaceOrAdmin":                          {1, "authz-only: workspace reach"},
	"helpers.go:ownsWorkspaceOrSecurityAdmin":                  {1, "authz-only: workspace reach"},
	"helpers.go:workspaceReadTierFor":                          {2, "authz-only: workspace read tier"},
	"http.go:requireOperator":                                  {1, "authz-only: the route tier"},
	"http.go:requireSecurityOperator":                          {1, "authz-only: the route tier"},
	"inline_policy_bounds.go:boundEnvSecretPosture":            {1, "R: the admin-only env-secret drop; the grants left meet the boundary's eligible_grants at resolve"},
	"me.go:handleMe":                                           {5, "authz-only: what GET /me shows"},
	"member_drives.go:memberDriveDoorRefusal":                  {1, "authz-only: a drive door the boundary holds no knob for"},
	"membermode.go:refuseAdminViewLaunch":                      {1, "authz-only: the User view launch door"},
	"policies.go:handleGetDefaultPolicy":                       {1, "authz-only: a policy read"},
	"policies.go:handleGetPolicy":                              {2, "authz-only: a policy read"},
	"policies.go:handleListPolicies":                           {1, "authz-only: a policy read"},
	"presets.go:presetForRead":                                 {2, "authz-only: preset read reach"},
	"presets.go:presetOpenTo":                                  {1, "authz-only: preset visibility; a preset launch resolves through the boundary"},
	"recording.go:recordingAuthorizer":                         {1, "authz-only: recording read reach"},
	"recording.go:recordingReader":                             {1, "authz-only: recording read reach"},
	"run_attention.go:handleMeAttention":                       {1, "authz-only: a list scope"},
	"run_containment.go:handleGetRunContainment":               {2, "authz-only: a read"},
	"run_end_create.go:resolveCreateEnd":                       {1, "E: an ungoverned run's end is capped by the boundary when it names a maximum"},
	"run_entry.go:getRunForEntry":                              {1, "authz-only: run reach"},
	"run_limit_sources.go:limitSources":                        {1, "authz-only: a display of where limits came from"},
	"run_overrides_apply.go:applySecretOverrides":              {2, "R: a removal only narrows; the spec meets the boundary at resolve"},
	"run_owner_authority.go:ownerCapabilityRefusal":            {1, "V: the owner re-check at revive, beside the boundary floor check"},
	"run_placement_eligibility.go:deniedPlacements":            {1, "D: placement; the run it admits meets the boundary at resolve and dispatch"},
	"run_policy_overlay.go:applyPolicyOverlay":                 {1, "R: the member overlay clamp; the boundary is the last step after it"},
	"run_policy_resolution.go:resolveRunPolicyFacts":           {2, "R: the member clamp gates; boundaryRunStep is the last step on both arms"},
	"run_policy_view.go:handleGetRunPolicy":                    {1, "authz-only: a read"},
	"run_request_governance.go:runRequestGovernance":           {1, "E: the operator create ceiling carries the boundary's end cap"},
	"run_revive.go:revivePATNarrowing":                         {1, "V: the revive narrowing; the boundary floor check runs at revive"},
	"run_workspace_authorization.go:repoAdmissionRefusal":      {1, "R: repository admission; the spec it yields meets the boundary at resolve"},
	"run_workspace_authorization.go:workspaceProvidersRefusal": {1, "R: provider admission; the spec it yields meets the boundary at resolve"},
	"runner_pools.go:permittedPools":                           {1, "D: pool placement; the run meets the boundary at resolve and dispatch"},
	"runner_pools_catalogue.go:personAllowance":                {1, "D: a pool allowance; the run meets the boundary at resolve and dispatch"},
	"runner_pools_runners.go:appendRunnerMembers":              {1, "authz-only: a list"},
	"runner_registration.go:handleRunnerTokenSelf":             {1, "authz-only: runner registration reach"},
	"runner_usage.go:remoteUsage":                              {1, "authz-only: a measured reason's detail"},
	"runs_dispatch_ceiling.go:memberResources":                 {1, "D: the size cap; the boundary holds no size knob"},
	"runs_list_filter.go:parseRunsListParams":                  {1, "authz-only: a list scope"},
	"runs_policy.go:handleListRuns":                            {1, "authz-only: a list scope"},
	"runs_policy.go:resolvePolicy":                             {1, "R: the ADO and GitHub caps strip; boundary knobs meet at resolve and dispatch"},
	"runs_policy.go:secretOwnerFromRequest":                    {1, "R: the credential namespace; the grants meet the boundary at resolve"},
	"secrets.go:handleListSecrets":                             {1, "authz-only: the credential namespace"},
	"secrets.go:handlePutSecret":                               {1, "authz-only: the credential namespace"},
	"secrets.go:secretOwnerParam":                              {1, "authz-only: the credential namespace"},
	"setup.go:handleSetupStatus":                               {3, "authz-only: setup rows"},
	"setup_checks_kek.go:kekLocalRows":                         {1, "authz-only: setup rows"},
	"setup_checks_keys.go:keyCustodyRows":                      {1, "authz-only: setup rows"},
	"setup_claude_signin_image.go:claudeSignInImageRows":       {1, "authz-only: setup rows"},
	"setup_integrations.go:handleListIntegrations":             {1, "authz-only: a list"},
	"sshkeys.go:handleAddSSHKey":                               {1, "authz-only: key reach"},
	"template_list.go:templateCallerOf":                        {1, "authz-only: template reach"},
	"user_drives_me.go:userDriveDeniedByProfile":               {1, "authz-only: a display flag"},
	"user_drives_preview.go:handlePreviewUserDrive":            {1, "authz-only: drive storage layout"},
	"user_drives_run.go:driveDoorProfile":                      {1, "R: a drive door the boundary holds no knob for; the run it admits meets the boundary at resolve"},
	"user_drives_tier.go:upsertUserDriveGrantAs":               {1, "authz-only: drive storage layout"},
	"user_view.go:meUserViewTypes":                             {1, "authz-only: the User view types"},
	"workspace_admission.go:admitLauncherRepo":                 {1, "R: launcher repository admission; the spec meets the boundary at resolve"},
	"workspace_admission.go:recordLaneVeto":                    {1, "authz-only: an audit row"},
	"workspace_admission.go:writeAdmissionLaunchRefusal":       {1, "authz-only: a refusal message"},
	"workspace_egress_github.go:unionDirectGitHubEgress":       {1, "D: the GitHub host clamp; hosts meet the boundary at dispatch"},
	"workspace_refs.go:launchCarrierRule":                      {1, "R: the launch carrier; the run meets the boundary at resolve"},
	"workspaces.go:handleListWorkspaces":                       {1, "authz-only: a list scope"},
}

// ungovernedCensusTags are the classifications a census entry may name.
var ungovernedCensusTags = []string{"R", "S", "D", "V", "A", "E", "authz-only"}

// ungovernedCalls adds, per "file:function", f's calls of the predicates that let a caller skip
// governance.
func ungovernedCalls(name string, f *ast.File, into map[string]int) {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		// Scope: calls of the three predicates by callee name; a method value and isAdminTokenCaller are outside it.
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var callee string
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				callee = fun.Sel.Name
			case *ast.Ident:
				callee = fun.Name
			}
			if callee == "runUngoverned" || callee == "isOperator" || callee == "isSecurityOperator" {
				into[name+":"+fn.Name.Name]++
			}
			return true
		})
	}
}

// scanUngovernedCalls is ungovernedCalls over every production file in this package.
func scanUngovernedCalls(t *testing.T) map[string]int {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]int{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ungovernedCalls(name, f, found)
	}
	return found
}

// ungovernedCensusProblems compares the calls found with the census.
func ungovernedCensusProblems(found map[string]int) []string {
	var problems []string
	for key, n := range found {
		entry, listed := ungovernedCallerCensus[key]
		switch {
		case !listed:
			problems = append(problems, fmt.Sprintf("%s asks a governance-skipping predicate %d time(s) and is not in ungovernedCallerCensus: tag where the boundary binds what it admits (R/S/D/V/A/E) or authz-only", key, n))
		case entry.calls != n:
			problems = append(problems, fmt.Sprintf("%s asks %d time(s), the census says %d: classify the new call", key, n, entry.calls))
		}
	}
	for key, entry := range ungovernedCallerCensus {
		if _, ok := found[key]; !ok {
			problems = append(problems, key+" is in ungovernedCallerCensus but asks nowhere: remove the entry")
		}
		if tag, _, _ := strings.Cut(entry.via, ":"); !slices.Contains(ungovernedCensusTags, tag) {
			problems = append(problems, fmt.Sprintf("%s is tagged %q, not one of %v", key, tag, ungovernedCensusTags))
		}
	}
	slices.Sort(problems)
	return problems
}

// TestUngovernedCallerCensus: every call of runUngoverned, isOperator and isSecurityOperator in
// internal/api's production files is classified by where the Wardyn boundary binds it.
func TestUngovernedCallerCensus(t *testing.T) {
	for _, p := range ungovernedCensusProblems(scanUngovernedCalls(t)) {
		t.Error(p)
	}
}

// TestUngovernedCallerCensusFlagsAPlantedTwin: a planted door that asks runUngoverned, and a twin
// call planted in a listed function, are each reported, and nothing else is.
func TestUngovernedCallerCensusFlagsAPlantedTwin(t *testing.T) {
	const planted = `package api
func (s *Server) plantedDoor(ctx context.Context) bool { return s.runUngoverned(ctx) }
func (s *Server) resolveChainCeiling(ctx context.Context) { _ = s.isOperator(ctx) }
`
	f, err := parser.ParseFile(token.NewFileSet(), "governance.go", planted, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := scanUngovernedCalls(t)
	ungovernedCalls("governance.go", f, found)
	got := ungovernedCensusProblems(found)
	want := []string{
		"governance.go:plantedDoor asks a governance-skipping predicate 1 time(s) and is not in ungovernedCallerCensus: tag where the boundary binds what it admits (R/S/D/V/A/E) or authz-only",
		"governance.go:resolveChainCeiling asks 2 time(s), the census says 1: classify the new call",
	}
	if !slices.Equal(got, want) {
		t.Errorf("planted twins reported as %q, want %q", got, want)
	}
}
