// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// foldMode is the door a run request is folded for. The three doors share one
// fold so Review and the policy preview cannot drift from the launch they
// describe; what differs between them is named by a step's modes in
// runFoldSteps or by an explicit f.mode test inside a step, both of which
// TestPreflightMirrorsLaunchGates reads.
type foldMode uint8

const (
	foldCreate    foldMode = 1 << iota // POST /runs: real gates, audited, renews, persists afterwards
	foldPreflight                      // POST /runs/preflight: launch's refusals, dry, no row
	foldPreview                        // the policy preview: no credential values, no capacity probes
	foldAll       = foldCreate | foldPreflight | foldPreview
)

// runFold is one request's trip through runFoldSteps: the inputs the door's
// prologue decided, and what each step resolved for the steps after it and for
// the door's own edge (create's persistence and audit, Review's grading, the
// preview's facts).
type runFold struct {
	mode    foldMode
	w       http.ResponseWriter
	r       *http.Request
	req     *createRunRequest
	ceiling governanceCeiling
	reqCC   types.ConfinementClass
	// baseline is the operator's egress baseline, read ONCE per request by stepBaseline: the
	// confinement floor, the autonomy posture and the door's own grading and facts all read this
	// value, so one request cannot grade on two sets.
	baseline composer.Baseline

	spec              types.RunPolicySpec
	policyID          *uuid.UUID
	policyWarns       []string
	source            policySourceRecord
	ephemeralDirs     []string
	driveMount        *types.DriveMount
	wsRefs            []types.Workspace
	present           map[string]bool
	reqEvents         []requirementAuditEntry
	directGitHubAdded []string
	comps             runComponents
	enforced          types.ConfinementClass
	mpChoice          runProviderChoice
	modelCred         modelCredentialFacts
	autonomy          types.AutonomyResolution
	autonomyWarns     []string
	scmSite           types.SiteConfig
	adoGrade          adoEntraGrade
	bedrockGrade      bedrockCredGrade
	adoNarrowed       string
	fitWarnings       []string
	prov              []provenanceRow
}

// runFoldStep is one gate or fold of the shared sequence. run writes its own
// refusal and returns false once it has answered.
type runFoldStep struct {
	run   func(s *Server, f *runFold, rec *foldRecorder) bool
	modes foldMode
}

// runFoldSteps is every gate between a decoded, authorized run request and
// create's mint, in the order all three doors meet them. Adding a gate is one
// entry here plus its name in TestPreflightMirrorsLaunchGates' pinned list.
//
// The ORDER is security-relevant and is the contract; do not sort it:
//   - The egress baseline is read first and once; the floor, the autonomy
//     posture and the door's grading read that one value.
//   - Policy first: the member clamp and grant narrowing run before anything
//     is granted, and every later step reads the clamped spec.
//   - Workspace seed after policy, the drive after the seed: seeding can set
//     req.Image, so its re-checks follow it, and its onboarding gate is the
//     chokepoint on the resolved spec that no drive may answer before.
//   - Workspace requirements, the direct-GitHub union and components before
//     the confinement floor: the CC3 blast-radius floor reads the grants and
//     hosts they add (invariant 5), and components see what admins already
//     credentialed. The dry doors widen workspace egress before the
//     requirements fold; create widens after the mint (unionRunEgress).
//   - Confinement, then the model provider, then autonomy: the autonomy grade
//     reads the enforced class and the chosen model credential.
//   - Azure DevOps standing and PAT narrowing read autonomy's site snapshot.
//   - Host capacity, the run cap and the quota fit last, so a refusal leaves
//     no identity and no run row.
var runFoldSteps = []runFoldStep{
	{(*Server).stepBaseline, foldAll},
	{(*Server).stepPolicy, foldAll},
	{(*Server).stepSeedWorkspace, foldAll},
	{(*Server).stepDrive, foldAll},
	{(*Server).stepWorkspaces, foldAll},
	{(*Server).stepPreviewEgress, foldPreflight | foldPreview},
	{(*Server).stepRequirements, foldAll},
	{(*Server).stepGitHubEgress, foldAll},
	{(*Server).stepComponents, foldAll},
	{(*Server).stepConfinement, foldAll},
	{(*Server).stepModelProvider, foldAll},
	{(*Server).stepAutonomy, foldAll},
	{(*Server).stepADOStanding, foldAll},
	{(*Server).stepPATNarrowing, foldPreflight | foldPreview},
	{(*Server).stepHostCapacity, foldCreate | foldPreflight},
	{(*Server).stepRunCap, foldCreate | foldPreflight},
	{(*Server).stepRunFit, foldCreate | foldPreflight},
}

// foldRunRequest runs runFoldSteps for mode over a request the door has
// already decoded and authorized. ok=false means a step has answered.
//
// The preflight door passes no reqCC: it parses confinement_class inside
// stepConfinement, where launch's order meets it.
func (s *Server) foldRunRequest(w http.ResponseWriter, r *http.Request, mode foldMode,
	req *createRunRequest, ceiling governanceCeiling, reqCC types.ConfinementClass,
) (runFold, bool) {
	f := runFold{mode: mode, w: w, r: r, req: req, ceiling: ceiling, reqCC: reqCC}
	rec := &foldRecorder{}
	for _, step := range runFoldSteps {
		if step.modes&mode != 0 && !step.run(s, &f, rec) {
			return runFold{}, false
		}
	}
	f.prov = rec.result()
	return f, true
}
